package livedb

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// The credential never reaches a table, a log or a response. What does is
// this: the machine, the port, the database, and — when none of those could
// be read out of the connection string — a digest standing in for all three.
//
// The rule the shapes below exist to keep is that a DSN which cannot be
// parsed is redacted MORE, not less. A parser that falls through to "return
// it as it came" is a parser that publishes a password the first time
// somebody uses a driver option nobody here anticipated.
type dsnFacts struct {
	Host     string
	Port     string
	Database string
	// Opaque stands in for the other three when the connection string parsed
	// as none of the known shapes. It is a digest of the scrubbed string, so
	// two unparsable DSNs are still two different sources — while the string
	// a reader is shown says only that it could not be read.
	Opaque string
}

// passwordKeys are the parameter names a credential hides behind in the
// keyword form and in query strings. Matched case-insensitively.
var passwordKeys = map[string]bool{
	"password": true, "passwd": true, "pwd": true, "pass": true,
}

// The conservative fallback. Neither of these understands a DSN; they only
// know the two places a secret sits in a string that looks vaguely like one,
// and they run before the digest so that the digest is over a string with no
// credential in it.
var (
	kvPassword  = regexp.MustCompile(`(?i)\b(password|passwd|pwd|pass)=[^\s&;]*`)
	urlPassword = regexp.MustCompile(`://([^:/@]*):[^@/]*@`)
	barePasswrd = regexp.MustCompile(`^([^:@/]*):[^@]*@`)
	// mysqlDSN is go-sql-driver's shape: user:pass@net(addr)/dbname?params.
	mysqlDSN = regexp.MustCompile(`^(?:([^:@/]*)(?::([^@]*))?@)?([a-zA-Z0-9]+)?(?:\(([^)]*)\))?/([^?]*)(?:\?(.*))?$`)
	// unparsed recognises this package's own opaque rendering on the way back
	// in, so a plan read out of a table keys the same as the one written.
	unparsed = regexp.MustCompile(`^[a-z]*://\[unparsed:([0-9a-f]+)\]$`)
)

// normalizeDriver folds the spellings the drivers answer to onto the two this
// package names.
func normalizeDriver(d string) string {
	switch strings.ToLower(strings.TrimSpace(d)) {
	case "postgres", "postgresql", "pgx", "pg":
		return "postgres"
	case "mysql", "mariadb":
		return "mysql"
	default:
		return strings.ToLower(strings.TrimSpace(d))
	}
}

// redactDSN reads what may be read out of a connection string and returns the
// string with the credential gone. It is total: every input has a redaction,
// and the one it cannot read gets the shortest one.
func redactDSN(driver, dsn string) (dsnFacts, string) {
	driver = normalizeDriver(driver)
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return dsnFacts{}, ""
	}
	if m := unparsed.FindStringSubmatch(dsn); m != nil {
		return dsnFacts{Opaque: m[1]}, dsn
	}
	if strings.Contains(dsn, "://") {
		if facts, red, ok := redactURL(dsn); ok {
			return facts, red
		}
	}
	if strings.Contains(dsn, "=") && !strings.Contains(dsn, "://") {
		if facts, red, ok := redactKeywords(dsn); ok {
			return facts, red
		}
	}
	if facts, red, ok := redactMySQL(dsn); ok {
		return facts, red
	}
	return dsnFacts{Opaque: opaqueDigest(dsn)}, driver + "://[unparsed:" + opaqueDigest(dsn) + "]"
}

// redactURL handles the scheme://user:pass@host:port/db?params form, which is
// what both drivers accept and what almost every deployment writes.
func redactURL(dsn string) (dsnFacts, string, bool) {
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return dsnFacts{}, "", false
	}
	if u.User != nil {
		// url.User, not UserPassword with a placeholder: the placeholder gets
		// percent-escaped and the result reads worse than the truth, which is
		// that this package does not have the password.
		u.User = url.User(u.User.Username())
	}
	q := u.Query()
	for k := range q {
		if passwordKeys[strings.ToLower(k)] {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	facts := dsnFacts{
		Host:     strings.ToLower(u.Hostname()),
		Port:     u.Port(),
		Database: strings.TrimPrefix(u.Path, "/"),
	}
	if facts.Database == "" {
		facts.Database = q.Get("dbname")
	}
	return facts, u.String(), true
}

// redactKeywords handles libpq's `host=… password=… dbname=…`. The rebuilt
// string is sorted, so two spellings of one connection redact identically and
// therefore key identically.
func redactKeywords(dsn string) (dsnFacts, string, bool) {
	fields := strings.Fields(dsn)
	kv := map[string]string{}
	for _, f := range fields {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			return dsnFacts{}, "", false
		}
		kv[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(v, `'"`)
	}
	if kv["host"] == "" && kv["dbname"] == "" {
		return dsnFacts{}, "", false
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		if passwordKeys[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+kv[k])
	}
	return dsnFacts{
		Host:     strings.ToLower(kv["host"]),
		Port:     kv["port"],
		Database: kv["dbname"],
	}, strings.Join(parts, " "), true
}

// redactMySQL handles go-sql-driver's user:pass@tcp(host:port)/db?params.
func redactMySQL(dsn string) (dsnFacts, string, bool) {
	m := mysqlDSN.FindStringSubmatch(dsn)
	if m == nil {
		return dsnFacts{}, "", false
	}
	user, net, addr, db, params := m[1], m[3], m[4], m[5], m[6]
	if db == "" && addr == "" {
		return dsnFacts{}, "", false
	}
	host, port := addr, ""
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host, port = addr[:i], addr[i+1:]
	}
	var b strings.Builder
	if user != "" {
		b.WriteString(user)
		b.WriteByte('@')
	}
	b.WriteString(net)
	if addr != "" {
		b.WriteString("(" + addr + ")")
	}
	b.WriteString("/" + db)
	if params != "" {
		if q, err := url.ParseQuery(params); err == nil {
			for k := range q {
				if passwordKeys[strings.ToLower(k)] {
					q.Del(k)
				}
			}
			if enc := q.Encode(); enc != "" {
				b.WriteString("?" + enc)
			}
		}
	}
	return dsnFacts{Host: strings.ToLower(host), Port: port, Database: db}, b.String(), true
}

// opaqueDigest is the identity of a connection string nothing here could
// read. The digest is taken after the two regexes above have had a pass, so
// what is hashed — and therefore what a rotated password would have to change
// to move the key — carries no credential.
func opaqueDigest(dsn string) string {
	scrubbed := kvPassword.ReplaceAllString(dsn, "$1=***")
	scrubbed = urlPassword.ReplaceAllString(scrubbed, "://$1@")
	scrubbed = barePasswrd.ReplaceAllString(scrubbed, "$1@")
	sum := sha256.Sum256([]byte("livedb-opaque-v1\n" + scrubbed))
	return hex.EncodeToString(sum[:])[:16]
}

// defaultSchema fills in what the driver would have defaulted to, so that a
// caller who left Schema empty and one who typed "public" are one source
// rather than two.
func defaultSchema(driver, schema string, facts dsnFacts) string {
	if schema != "" {
		return schema
	}
	switch driver {
	case "postgres":
		return "public"
	case "mysql":
		// MySQL has no schema below the database; the DSN's database IS it.
		return facts.Database
	default:
		return ""
	}
}

// sourceKey is the stable identity of a (database, schema, tableset).
//
// It is derived from the facts, never from the credential, which is what
// makes it survive a rotation: a plan signed under one password is still the
// plan in force when the password changes, and the run that supplies the new
// one is still checked against the database the signature was about. The user
// is left out of the key for the same reason the password is — an application
// that moves from one role to another has not moved to another database.
//
// It reads either the live DSN or the redacted one, because a Source
// reconstructed from a stored plan has only the second, and the two must key
// the same or Current would never match a run.
func sourceKey(s Source) string {
	driver := normalizeDriver(s.Driver)
	raw := s.DSN
	if strings.TrimSpace(raw) == "" {
		raw = s.Redacted
	}
	facts, _ := redactDSN(driver, raw)
	tables := append([]string(nil), s.Tables...)
	sort.Strings(tables)

	var b strings.Builder
	b.WriteString("livedb-source-v1\n")
	for _, part := range []string{
		driver, facts.Host, facts.Port, facts.Database, facts.Opaque,
		defaultSchema(driver, s.Schema, facts),
	} {
		b.WriteString(part)
		b.WriteByte('\n')
	}
	for _, t := range tables {
		b.WriteString(t)
		b.WriteByte('\x1f')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "src_" + hex.EncodeToString(sum[:])[:16]
}

// resolved returns the source as it is stored: the credential gone, the
// redaction filled in, the schema defaulted and the tables sorted.
func (s Source) resolved(tables []string) Source {
	driver := normalizeDriver(s.Driver)
	raw := s.DSN
	if strings.TrimSpace(raw) == "" {
		raw = s.Redacted
	}
	facts, red := redactDSN(driver, raw)
	out := Source{
		Driver:   driver,
		Redacted: red,
		Schema:   defaultSchema(driver, s.Schema, facts),
		Tables:   append([]string(nil), tables...),
	}
	sort.Strings(out.Tables)
	return out
}

// validateSource refuses what cannot be dialled before anything is opened.
func validateSource(s Source) error {
	switch normalizeDriver(s.Driver) {
	case "postgres", "mysql":
	default:
		return fmt.Errorf("livedb: driver %q is not one of postgres, mysql", s.Driver)
	}
	if strings.TrimSpace(s.DSN) == "" {
		return fmt.Errorf("livedb: the source needs a DSN; it is not stored, so it is supplied on every call")
	}
	return nil
}
