package livedb

import (
	"context"
	"database/sql"
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
		return connector.NewPostgresCDCSource(src.DSN, connector.PostgresCDCOptions{
			Publication: "athanor_livedb",
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
