package snapshots

import (
	"fmt"

	"github.com/liliang-cn/cortexdb/v2/pkg/sqldialect"
)

// One table on the brain's own handle, pkg/ontologies' and pkg/livedb's
// arrangement and for their reasons: a package that needs storage takes
// db.SQL() and db.Dialect() and owns tables nobody else writes, rather than
// opening a database of its own. What it buys here is the whole point of a
// snapshot — the named moment and the graph it names are in one file, so a
// backup carries both and a restore cannot bring back a brain whose moments
// were somewhere else.
//
// DATETIME is SQLite's spelling and not a PostgreSQL type; TIMESTAMPTZ rather
// than TIMESTAMP because every value written here is already UTC and a naive
// column loses that on the way back. No BLOB, no AUTOINCREMENT, no SERIAL:
// the primary key is the name a person chose.
//
// There is no body column and there never will be one. What a snapshot would
// put in it is already in graph_node_history and graph_edge_history; see the
// package note on why a second copy is worse than no copy.
//
// `by_name` rather than `by` because BY is a keyword in both dialects and a
// bare column of that name is a syntax error, not a style question.
func schemaSQL(d sqldialect.Dialect) string {
	ts := "DATETIME"
	if d.Kind() == sqldialect.Postgres {
		ts = "TIMESTAMPTZ"
	}
	return fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS athanor_snapshots (
	name          TEXT PRIMARY KEY,
	at            %[1]s NOT NULL,
	state         TEXT NOT NULL,
	actor         TEXT NOT NULL,
	by_name       TEXT NOT NULL DEFAULT '',
	note          TEXT NOT NULL DEFAULT '',
	nodes         INTEGER NOT NULL DEFAULT 0,
	edges         INTEGER NOT NULL DEFAULT 0,
	orphans       INTEGER NOT NULL DEFAULT 0,
	grades        TEXT NOT NULL DEFAULT '{}',
	ontologies    TEXT NOT NULL DEFAULT '{}',
	loads         TEXT NOT NULL DEFAULT '[]',
	more_loads    INTEGER NOT NULL DEFAULT 0,
	load_count    INTEGER NOT NULL DEFAULT 0,
	replaced      %[1]s,
	dropped_actor TEXT NOT NULL DEFAULT '',
	dropped_by    TEXT NOT NULL DEFAULT '',
	dropped_at    %[1]s,
	drop_note     TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS athanor_snapshots_at    ON athanor_snapshots(at);
CREATE INDEX IF NOT EXISTS athanor_snapshots_actor ON athanor_snapshots(actor);
CREATE INDEX IF NOT EXISTS athanor_snapshots_state ON athanor_snapshots(state);
`, ts)
}
