package server

import (
	"github.com/liliang-cn/alchemy/pkg/service"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// Options is what an operator says about one Athanor. Everything else is
// decided here or inherited from the two projects underneath.
type Options struct {
	// DBPath is the brain: a SQLite file, or a postgres:// DSN. The DSN
	// decides the backend, exactly as it does for cortexdb.Open.
	DBPath string

	// GRPCAddr serves alchemy.v1 and cortexdb.v1 on one listener. Existing
	// CortexDB clients — the MCP server in remote mode, the typed clients —
	// point CORTEXDB_REMOTE here and need no change.
	GRPCAddr string
	// HTTPAddr serves the REST translation of both, the review UI, the live
	// graph, the metrics, and Athanor's own pages.
	HTTPAddr string

	// KeyFile is the scoped-key policy, shared by every door. Token is the
	// legacy single-key shape. Both are authz.Resolve's arguments, so the rule
	// is CortexDB's: a key file, when present, is the entire policy.
	KeyFile string
	Token   string

	// BackupDir confines AdminService.Backup. Empty is the directory holding
	// DBPath.
	BackupDir string
	// Spool is where uploaded sources are written while a job runs. Empty is
	// the OS temporary directory.
	Spool string
	// JobCapacity bounds how many jobs the in-memory store admits. Zero takes
	// alchemy's default.
	JobCapacity int

	// LiveDBHosts confines which databases pkg/livedb may dial, as host or
	// host:port entries. Empty is unconfined, which is the right default for a
	// single-operator deployment and the wrong one for a shared Athanor.
	LiveDBHosts []string

	// Embedder, when set, is the brain's. Nil is lexical mode, which is a
	// supported path and the one the review pipeline's own connector refuses
	// to bypass: it never lets the store embed what alchemy did not.
	Embedder cortexdb.Embedder

	// Runner replaces the extraction pipeline. Nil is the real one; a test
	// supplies one that returns what it says, so nothing here needs a model.
	Runner service.Runner
}

func (o Options) withDefaults() Options {
	if o.GRPCAddr == "" {
		o.GRPCAddr = "127.0.0.1:47831"
	}
	if o.HTTPAddr == "" {
		o.HTTPAddr = "127.0.0.1:47832"
	}
	return o
}
