// Package braintest opens the brain a store test runs against, once per
// backend Athanor supports.
//
// Two packages in this repository own tables on the brain's own handle —
// pkg/ontologies and pkg/livedb — and both write SQL that has to be true on
// SQLite and on PostgreSQL. The dialect-aware helpers make that plausible;
// only running the suite on both makes it known. The difference is not
// academic: CortexDB's own connector.NewSQLiteCheckpointStore writes `?`
// placeholders and datetime('now') and is silently broken on PostgreSQL,
// which is exactly the shape of bug a SQLite-only suite cannot see.
//
// The helper lives here rather than in either package's _test.go so that the
// second package to need it inherits the first one's decisions instead of
// making its own slightly different ones.
package braintest

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
	"github.com/liliang-cn/cortexdb/v2/pkg/sqldialect"
)

// EnvDSN names the variable that turns the PostgreSQL half on. It wants a
// database this suite may create tables in — not the one an application is
// using — because every store under test ensures its own schema on New.
const EnvDSN = "ATHANOR_TEST_POSTGRES"

// Brain is one backend a suite runs against.
type Brain struct {
	// Name is the subtest's name: "sqlite" or "postgres".
	Name string
	// Kind is what the opened handle must report. Asserted rather than
	// assumed: a DSN typo that fell back to SQLite would otherwise show up as
	// a PostgreSQL run that passed without touching PostgreSQL.
	Kind sqldialect.Kind

	dsn string
}

// Backends is every brain this run covers.
//
// PostgreSQL is opt-in and its absence is said out loud rather than skipped
// quietly. A parity suite nobody notices is not running is a suite that has
// stopped being a parity suite, and t.Skip is invisible without -v.
func Backends(t *testing.T) []Brain {
	t.Helper()
	out := []Brain{{Name: "sqlite", Kind: sqldialect.SQLite}}
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Logf("%s unset — PostgreSQL is NOT covered by this run", EnvDSN)
		return out
	}
	return append(out, Brain{Name: "postgres", Kind: sqldialect.Postgres, dsn: dsn})
}

// Open returns a brain on this backend, closed when the test ends.
//
// A SQLite brain is a fresh file per call. A PostgreSQL brain is the one
// database the variable names, shared by every call in the run — so a suite
// that opens more than one has to mint its own ids rather than reuse a
// fixture's. Unique is here for that.
func (b Brain) Open(t *testing.T) *cortexdb.DB {
	t.Helper()
	path := b.dsn
	if path == "" {
		path = filepath.Join(t.TempDir(), "brain.db")
	}
	cfg := cortexdb.DefaultConfig(path)
	cfg.Dimensions = 4
	db, err := cortexdb.Open(cfg)
	if err != nil {
		t.Fatalf("braintest: open the %s brain: %v", b.Name, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if got := db.Dialect().Kind(); got != b.Kind {
		t.Fatalf("braintest: the %s brain speaks %s", b.Name, got)
	}
	return db
}

var seq atomic.Int64

// Unique is a name no other call in this process returns, and one a repeat of
// the whole run is unlikely to return again — which is what a shared
// PostgreSQL database needs from every id a test invents.
func Unique(prefix string) string {
	return fmt.Sprintf("%s%d_%d", prefix, time.Now().UnixNano()%1e9, seq.Add(1))
}
