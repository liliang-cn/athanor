package ontologies

import (
	"fmt"

	"github.com/liliang-cn/cortexdb/v2/pkg/sqldialect"
)

// Two tables on the brain's own handle, the pattern pkg/agentmem established
// in CortexDB: a package that needs storage takes db.SQL() and db.Dialect()
// and owns tables nobody else writes, rather than opening a database of its
// own. What that buys here is the whole reason a vocabulary lives in the
// brain at all — a backup of the brain carries the ontologies, and a load and
// the version it was checked against commit or fail inside one transaction
// boundary.
//
// The two type names the databases spell differently are left as verbs, for
// agentmem's reasons: DATETIME is SQLite's spelling and not a PostgreSQL type,
// and TIMESTAMPTZ rather than TIMESTAMP because every value written here is
// already UTC and a naive column loses that on the way back. There is no BLOB,
// no AUTOINCREMENT and no SERIAL: every primary key is a caller-supplied or
// minted TEXT id, so the seeded-id-versus-sequence trap has nothing to bite.
func schemaSQL(d sqldialect.Dialect) string {
	ts := "DATETIME"
	if d.Kind() == sqldialect.Postgres {
		ts = "TIMESTAMPTZ"
	}
	return fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS athanor_ontology_versions (
	id            TEXT PRIMARY KEY,
	lineage       TEXT NOT NULL,
	body          TEXT NOT NULL,
	state         TEXT NOT NULL,
	parent        TEXT NOT NULL DEFAULT '',
	part          TEXT NOT NULL DEFAULT '',
	proposals     TEXT NOT NULL DEFAULT '[]',
	proposed_from TEXT NOT NULL DEFAULT '',
	created_by    TEXT NOT NULL DEFAULT '',
	created_at    %[1]s NOT NULL,
	approved_by   TEXT NOT NULL DEFAULT '',
	approved_at   %[1]s,
	published_at  %[1]s,
	retired_at    %[1]s,
	note          TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS athanor_ontology_versions_lineage ON athanor_ontology_versions(lineage);
CREATE INDEX IF NOT EXISTS athanor_ontology_versions_state   ON athanor_ontology_versions(state);

CREATE TABLE IF NOT EXISTS athanor_ontology_acts (
	id      TEXT PRIMARY KEY,
	kind    TEXT NOT NULL,
	actor   TEXT NOT NULL,
	at      %[1]s NOT NULL,
	subject TEXT NOT NULL,
	note    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS athanor_ontology_acts_subject ON athanor_ontology_acts(subject);
CREATE INDEX IF NOT EXISTS athanor_ontology_acts_at      ON athanor_ontology_acts(at);
`, ts)
}

// publishedIndex is the rule "one current vocabulary per lineage" written
// where the database can hold it, rather than only in the transaction that
// intends it. A partial unique index is the same syntax on both backends.
//
// The transaction in Publish already retires the previous one, so this index
// should never fire. That is the point: if a second path to publishing is ever
// added and forgets, the second published row is refused rather than quietly
// making `current` ambiguous — and `current` is the answer a client pastes
// into every job.
const publishedIndex = `
CREATE UNIQUE INDEX IF NOT EXISTS athanor_ontology_one_published
	ON athanor_ontology_versions(lineage) WHERE state = 'published'
`
