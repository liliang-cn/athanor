package rules

import (
	"fmt"

	"github.com/liliang-cn/cortexdb/v2/pkg/sqldialect"
)

// Two tables on the brain's own handle, pkg/ontologies' arrangement and for
// its reasons: a package that needs storage takes db.SQL() and db.Dialect()
// and owns tables nobody else writes, rather than opening a database of its
// own. What that buys here is that a backup of the brain carries the rule, the
// firings made under it and the edges those firings derived — so "which rule
// put this edge here, and who fired it" is answerable from one file.
//
// DATETIME is SQLite's spelling and not a PostgreSQL type; TIMESTAMPTZ rather
// than TIMESTAMP because every value written here is already UTC and a naive
// column loses that on the way back. There is no BLOB, no AUTOINCREMENT and no
// SERIAL: every primary key is a caller-supplied or minted TEXT id.
//
// The acts table carries a `detail` column the vocabulary's does not, because
// one of the four acts here has a result — what an application derived — and a
// firing that only existed in the audit view would be a firing this package
// could not list back. The ledger is the audit view; this is the record.
func schemaSQL(d sqldialect.Dialect) string {
	ts := "DATETIME"
	if d.Kind() == sqldialect.Postgres {
		ts = "TIMESTAMPTZ"
	}
	return fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS athanor_rules (
	id           TEXT PRIMARY KEY,
	lineage      TEXT NOT NULL,
	state        TEXT NOT NULL,
	parent       TEXT NOT NULL DEFAULT '',
	rule_text    TEXT NOT NULL,
	name         TEXT NOT NULL DEFAULT '',
	confidence   REAL NOT NULL DEFAULT 0,
	weight       REAL NOT NULL DEFAULT 0,
	metadata     TEXT NOT NULL DEFAULT '{}',
	note         TEXT NOT NULL DEFAULT '',
	created_by   TEXT NOT NULL DEFAULT '',
	created_at   %[1]s NOT NULL,
	published_by TEXT NOT NULL DEFAULT '',
	published_at %[1]s,
	retired_at   %[1]s
);

CREATE INDEX IF NOT EXISTS athanor_rules_lineage ON athanor_rules(lineage);
CREATE INDEX IF NOT EXISTS athanor_rules_state   ON athanor_rules(state);

CREATE TABLE IF NOT EXISTS athanor_rule_acts (
	id      TEXT PRIMARY KEY,
	kind    TEXT NOT NULL,
	actor   TEXT NOT NULL,
	at      %[1]s NOT NULL,
	subject TEXT NOT NULL,
	note    TEXT NOT NULL DEFAULT '',
	detail  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS athanor_rule_acts_subject ON athanor_rule_acts(subject);
CREATE INDEX IF NOT EXISTS athanor_rule_acts_kind    ON athanor_rule_acts(kind);
CREATE INDEX IF NOT EXISTS athanor_rule_acts_at      ON athanor_rule_acts(at);
`, ts)
}

// publishedIndex is the rule "one rule in force per lineage" written where the
// database can hold it, rather than only in the transaction that intends it. A
// partial unique index is the same syntax on both backends.
//
// The transaction in Publish already retires the previous one, so this index
// should never fire. That is the point: if a second path to publishing is ever
// added and forgets, the second published row is refused rather than quietly
// making `current` ambiguous — and `current` is the rule an operator fires.
const publishedIndex = `
CREATE UNIQUE INDEX IF NOT EXISTS athanor_rules_one_published
	ON athanor_rules(lineage) WHERE state = 'published'
`
