package livedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/liliang-cn/cortexdb/v2/pkg/connector"
	"github.com/liliang-cn/cortexdb/v2/pkg/importflow"
)

// liveOpener is the one real implementation of Opener: connector's own source
// for the rows, and a second handle on the same DSN for the two questions
// importflow.Source cannot answer.
//
// The second handle is not a waste. connector's sqlSource keeps its *sql.DB
// unexported and offers no way to ask which columns a primary key is made of;
// re-deriving that by reading its private state would be a fork of the
// connector maintained by nobody. A metadata connection costs one socket and
// is closed with the source.
type liveOpener struct{}

func (liveOpener) Open(ctx context.Context, src Source) (Live, error) {
	if err := validateSource(src); err != nil {
		return nil, err
	}
	driver := normalizeDriver(src.Driver)
	facts, _ := redactDSN(driver, src.DSN)
	schema := defaultSchema(driver, src.Schema, facts)

	opts := connector.SourceOptions{Schema: schema, Tables: src.Tables}
	var (
		inner     importflow.Source
		err       error
		sqlDriver = "pgx"
	)
	switch driver {
	case "postgres":
		inner, err = connector.NewPostgresSource(src.DSN, opts)
	case "mysql":
		sqlDriver = "mysql"
		inner, err = connector.NewMySQLSource(src.DSN, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("livedb: open %s: %w", driver, err)
	}
	meta, err := sql.Open(sqlDriver, src.DSN)
	if err != nil {
		_ = inner.Close()
		return nil, fmt.Errorf("livedb: open %s metadata: %w", driver, err)
	}
	if err := meta.PingContext(ctx); err != nil {
		_ = meta.Close()
		_ = inner.Close()
		return nil, fmt.Errorf("livedb: ping %s metadata: %w", driver, err)
	}
	return &liveSource{Source: inner, meta: meta, driver: driver, schema: schema}, nil
}

// Changes opens the driver's change stream. A driver whose stream is not
// configured says what is missing: an empty publication is a deployment step
// somebody skipped, and reporting it as "no events" would look like a quiet
// database for as long as anybody was willing to wait.
func (liveOpener) Changes(ctx context.Context, src Source, cp connector.Checkpoint) (connector.ChangeSource, error) {
	if err := validateSource(src); err != nil {
		return nil, err
	}
	live, err := (liveOpener{}).Open(ctx, src)
	if err != nil {
		return nil, err
	}
	defer func() { _ = live.Close() }()

	keys := map[string][]string{}
	schemas, err := live.Schemas(ctx)
	if err != nil {
		return nil, err
	}
	for _, sc := range schemas {
		k, err := live.Keys(ctx, sc.Table)
		if err != nil {
			return nil, err
		}
		keys[sc.Table] = k
	}
	switch normalizeDriver(src.Driver) {
	case "postgres":
		if meta, ok := live.(*liveSource); ok {
			if err := publicationCovers(ctx, meta.meta, livedbPublication, meta.schema, keys); err != nil {
				return nil, err
			}
		}
		return connector.NewPostgresCDCSource(src.DSN, connector.PostgresCDCOptions{
			Publication: livedbPublication,
			Slot:        "athanor_" + strings.TrimPrefix(src.Key(), "src_"),
			Tables:      keys,
			CreateSlot:  true,
		})
	case "mysql":
		return connector.NewMySQLBinlogSource(src.DSN, connector.MySQLBinlogOptions{Tables: keys})
	}
	return nil, fmt.Errorf("livedb: no change stream for driver %q", src.Driver)
}

// liveSource is connector's source plus Keys and Relations.
type liveSource struct {
	importflow.Source
	meta   *sql.DB
	driver string
	schema string
}

func (l *liveSource) Close() error {
	err := l.Source.Close()
	if merr := l.meta.Close(); err == nil {
		err = merr
	}
	return err
}

// Keys reads a table's primary key.
//
// On PostgreSQL this asks pg_catalog rather than information_schema, and that
// is not a preference. information_schema.table_constraints shows only
// constraints on tables the current user owns or holds "some privilege other
// than SELECT" on — so a role with nothing but SELECT, which is exactly the
// read-only role this whole feature asks an operator to connect with, sees an
// empty result and no error. The consequence was silent and total: every table
// looked keyless, every table got a RAG chunk and no graph node, and an import
// of a schema full of foreign keys produced zero triples while reporting
// success. pg_catalog is not privilege-filtered that way.
//
// MySQL's information_schema is filtered by ordinary table privileges, where
// SELECT is enough, so it keeps the portable query.
func (l *liveSource) Keys(ctx context.Context, table string) ([]string, error) {
	q := `SELECT a.attname
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON TRUE
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
		WHERE i.indisprimary AND n.nspname = %s AND c.relname = %s
		ORDER BY k.ord`
	if l.driver == "mysql" {
		q = `SELECT kcu.column_name
			FROM information_schema.table_constraints tc
			JOIN information_schema.key_column_usage kcu
			  ON kcu.constraint_name = tc.constraint_name
			 AND kcu.table_schema = tc.table_schema
			 AND kcu.table_name = tc.table_name
			WHERE tc.constraint_type = 'PRIMARY KEY'
			  AND tc.table_schema = %s AND tc.table_name = %s
			ORDER BY kcu.ordinal_position`
	}
	rows, err := l.meta.QueryContext(ctx, l.bind(q), l.schema, table)
	if err != nil {
		return nil, fmt.Errorf("livedb: primary key of %s: %w", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (l *liveSource) Relations(ctx context.Context, table string) ([]Relation, error) {
	// pg_catalog for the same reason as Keys: constraint_column_usage is
	// privilege-filtered even harder than table_constraints — it shows only
	// what the current user owns — so the read-only role this feature is meant
	// to be used with reads no foreign key at all through it.
	q := `SELECT con.conname, a.attname, tc.relname, ta.attname
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_class tc ON tc.oid = con.confrelid
		JOIN unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord) ON TRUE
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
		JOIN unnest(con.confkey) WITH ORDINALITY AS fk(attnum, ord) ON fk.ord = k.ord
		JOIN pg_attribute ta ON ta.attrelid = tc.oid AND ta.attnum = fk.attnum
		WHERE con.contype = 'f' AND n.nspname = %s AND c.relname = %s
		ORDER BY con.conname, k.ord`
	if l.driver == "mysql" {
		// MySQL has no constraint_column_usage; the referenced side is on
		// key_column_usage itself.
		q = `SELECT constraint_name, column_name, referenced_table_name, referenced_column_name
			FROM information_schema.key_column_usage
			WHERE table_schema = %s AND table_name = %s AND referenced_table_name IS NOT NULL
			ORDER BY constraint_name, ordinal_position`
	}
	rows, err := l.meta.QueryContext(ctx, l.bind(q), l.schema, table)
	if err != nil {
		return nil, fmt.Errorf("livedb: foreign keys of %s: %w", table, err)
	}
	defer rows.Close()

	byName := map[string]*Relation{}
	var order []string
	for rows.Next() {
		var name, col, target, targetCol string
		if err := rows.Scan(&name, &col, &target, &targetCol); err != nil {
			return nil, err
		}
		r, ok := byName[name]
		if !ok {
			r = &Relation{Table: table, Target: target}
			byName[name] = r
			order = append(order, name)
		}
		r.Columns = append(r.Columns, col)
		r.TargetKeys = append(r.TargetKeys, targetCol)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(order)
	out := make([]Relation, 0, len(order))
	for _, name := range order {
		r := byName[name]
		r.Predicate = fkPredicate(r.Columns, r.Target)
		out = append(out, *r)
	}
	return out, nil
}

// bind spells the two placeholders each backend wants. It is not
// sqldialect.Rebind: that one is about the brain's handle, and this query runs
// on somebody else's database.
func (l *liveSource) bind(q string) string {
	if l.driver == "mysql" {
		return fmt.Sprintf(q, "?", "?")
	}
	return fmt.Sprintf(q, "$1", "$2")
}

// fkPredicate names an edge after the thing it points at rather than after
// the column it was found on, because "customer" reads and queries better
// than "customer_id" and the column is already on the entity's properties.
func fkPredicate(cols []string, target string) string {
	if len(cols) == 1 {
		if stem := strings.TrimSuffix(cols[0], "_id"); stem != "" && stem != cols[0] {
			return stem
		}
	}
	return "references_" + target
}

// livedbPublication is the pgoutput publication a follow streams through. It
// is a constant because the check below and the stream must name the same one;
// two spellings would be a check that passes for a publication nothing reads.
const livedbPublication = "athanor_livedb"

// publicationCovers refuses a change stream that would carry nothing.
//
// This is the failure the package comment on Changes promises to catch, and
// postgres will not catch it: START_REPLICATION names the publication as a
// plugin argument and pgoutput resolves it per change, so a name nobody
// created matches no table and the stream opens, stays open, reports no error
// and delivers nothing. A follow in that state answers "running" for as long
// as anybody is willing to wait, which is the one thing a server that owns a
// job must never say about a database it cannot see.
//
// Creating the publication is deliberately not attempted. It is a DDL
// statement against somebody else's database, the credential a plan runs with
// is expected to be a reader, and a product whose pitch is that it does not
// touch your data until you have signed for it cannot open by issuing DDL.
// What is owed instead is the sentence that says what is missing and the
// statement that fixes it.
func publicationCovers(ctx context.Context, db *sql.DB, publication, schema string, keys map[string][]string) error {
	var all bool
	err := db.QueryRowContext(ctx,
		`SELECT puballtables FROM pg_publication WHERE pubname = $1`, publication).Scan(&all)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("livedb: this database has no publication %q, so a change stream would open and carry nothing. "+
			"Creating one is a deployment step and not a read, so it is not done from here: run "+
			"CREATE PUBLICATION %s FOR ALL TABLES; (or FOR TABLE ... naming the tables this plan covers) "+
			"as a user with rights to, and start the follow again", publication, publication)
	}
	if err != nil {
		return fmt.Errorf("livedb: read pg_publication: %w", err)
	}
	if all {
		return nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT tablename FROM pg_publication_tables WHERE pubname = $1 AND schemaname = $2`, publication, schema)
	if err != nil {
		return fmt.Errorf("livedb: read pg_publication_tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	published := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("livedb: read pg_publication_tables: %w", err)
		}
		published[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("livedb: read pg_publication_tables: %w", err)
	}
	var missing []string
	for table := range keys {
		name := table
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		if !published[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	// Named rather than counted: an operator adding tables to a publication
	// needs the names, and a follow that carried half a plan silently would be
	// the same quiet failure in a smaller size.
	return fmt.Errorf("livedb: publication %q does not carry %s, so a follow would keep the brain in step with only part of this plan. "+
		"Run ALTER PUBLICATION %s ADD TABLE %s; and start the follow again",
		publication, strings.Join(missing, ", "), publication, strings.Join(missing, ", "))
}
