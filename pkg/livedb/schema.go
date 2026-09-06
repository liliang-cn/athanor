package livedb

import (
	"fmt"

	"github.com/liliang-cn/cortexdb/v2/pkg/sqldialect"
)

// Three tables on the brain's own handle, pkg/ontologies' arrangement and for
// its reasons: a package that needs storage takes db.SQL() and db.Dialect()
// and owns tables nobody else writes, rather than opening a database of its
// own. What that buys here is that a backup of the brain carries the signed
// plan, the runs made under it, and the position the change stream had
// reached — so a restore resumes rather than re-importing, and "which
// signature let this node in" is answerable from one file.
//
// DATETIME is SQLite's spelling and not a PostgreSQL type; TIMESTAMPTZ rather
// than TIMESTAMP because every value written here is already UTC and a naive
// column loses that on the way back. There is no BLOB, no AUTOINCREMENT and
// no SERIAL: every primary key is a minted TEXT id.
//
// Three column names are deliberately not the obvious ones: `cursor_json`,
// `pos` and `db_schema` rather than `cursor`, `position` and `schema`. The
// reason first written here was that PostgreSQL reserves all three and the
// DDL would be a syntax error — which, checked against PostgreSQL 16 rather
// than assumed, is not true: all three are col_name keywords and all three
// are accepted as column names, in CREATE TABLE and in a SELECT list. The
// names stay anyway. They are reserved in the SQL standard's sense and are
// the kind of word a future backend, a migration tool or a hand-written
// psql query does trip over, and renaming a column that already holds a
// change stream's position is a worse day than not naming it `position`.
// What is not true is that this was load-bearing.
func schemaSQL(d sqldialect.Dialect) string {
	ts := "DATETIME"
	if d.Kind() == sqldialect.Postgres {
		ts = "TIMESTAMPTZ"
	}
	return fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS athanor_livedb_plans (
	id         TEXT PRIMARY KEY,
	source_key TEXT NOT NULL,
	driver     TEXT NOT NULL,
	redacted   TEXT NOT NULL DEFAULT '',
	db_schema  TEXT NOT NULL DEFAULT '',
	tables     TEXT NOT NULL DEFAULT '[]',
	treatments TEXT NOT NULL DEFAULT '[]',
	hash       TEXT NOT NULL,
	state      TEXT NOT NULL,
	counts     TEXT NOT NULL DEFAULT '{}',
	created_by TEXT NOT NULL DEFAULT '',
	created_at %[1]s NOT NULL,
	signed_by  TEXT NOT NULL DEFAULT '',
	signed_at  %[1]s,
	sign_act   TEXT NOT NULL DEFAULT '',
	supersedes TEXT NOT NULL DEFAULT '',
	note       TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS athanor_livedb_plans_source ON athanor_livedb_plans(source_key);
CREATE INDEX IF NOT EXISTS athanor_livedb_plans_state  ON athanor_livedb_plans(state);

CREATE TABLE IF NOT EXISTS athanor_livedb_runs (
	id         TEXT PRIMARY KEY,
	plan       TEXT NOT NULL,
	source_key TEXT NOT NULL,
	started_at %[1]s NOT NULL,
	ended_at   %[1]s NOT NULL,
	dry_run    INTEGER NOT NULL DEFAULT 0,
	rows_read  INTEGER NOT NULL DEFAULT 0,
	chunks     INTEGER NOT NULL DEFAULT 0,
	triples    INTEGER NOT NULL DEFAULT 0,
	skipped    INTEGER NOT NULL DEFAULT 0,
	drift      TEXT NOT NULL DEFAULT '[]',
	gone       TEXT NOT NULL DEFAULT '[]',
	errors     TEXT NOT NULL DEFAULT '[]'
);

CREATE INDEX IF NOT EXISTS athanor_livedb_runs_plan ON athanor_livedb_runs(plan);
CREATE INDEX IF NOT EXISTS athanor_livedb_runs_at   ON athanor_livedb_runs(started_at);

CREATE TABLE IF NOT EXISTS athanor_livedb_checkpoints (
	source_key  TEXT PRIMARY KEY,
	cursor_json TEXT NOT NULL DEFAULT '',
	pos         TEXT NOT NULL DEFAULT '',
	updated_at  %[1]s NOT NULL
);
`, ts)
}

// signedIndex is the rule "one signed plan per source" written where the
// database can hold it, rather than only in the transaction that intends it.
// A partial unique index is the same syntax on both backends.
//
// The transaction in Sign already supersedes the previous one, so this index
// should never fire. That is the point: if a second path to signing is ever
// added and forgets, the second signed row is refused rather than quietly
// making Current ambiguous — and Current is the plan every run is checked
// against.
const signedIndex = `
CREATE UNIQUE INDEX IF NOT EXISTS athanor_livedb_one_signed
	ON athanor_livedb_plans(source_key) WHERE state = 'signed'
`
