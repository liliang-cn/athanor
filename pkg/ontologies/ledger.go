package ontologies

import "context"

// The ledger, as this package sees it.
//
// The two tables here are the source of truth for the workflow: what state a
// version is in, which one is published, and who signed each act. Nothing
// about that changes because a ledger exists — a state machine whose authority
// is an audit view is a state machine that loses a version when the audit view
// is unavailable.
//
// What the ledger adds is that one query answers both. An operator asking "who
// changed the vocabulary, and who loaded a graph, and who accepted that
// finding" was asking three different stores until now. Mirroring each act
// into the brain's decision ledger makes it one.
//
// The interface is declared here rather than taken from pkg/cortexdb for the
// reason this package takes db.SQL() rather than opening a database: what it
// needs is narrow, and a narrow interface is what keeps the tests in this
// package running with no brain at all. A Store with no ledger records
// everything it always did and mirrors nothing.
type Ledger interface {
	// Record mirrors one act. It is called after the act has committed, never
	// inside its transaction — a ledger write is a write to the same handle,
	// and a second writer inside an open write transaction is a deadlock on
	// SQLite and a wasted round trip on PostgreSQL.
	//
	// An error is the mirror's, not the act's. The act stands; the caller of
	// this interface reports the failure and carries on.
	Record(ctx context.Context, act Act) error
}

// Option configures a Store.
type Option func(*Store)

// WithLedger mirrors every act this store records into the ledger too.
func WithLedger(l Ledger) Option {
	return func(s *Store) { s.ledger = l }
}

// mirror hands committed acts to the ledger, if there is one.
//
// It returns the first failure rather than swallowing it, and every caller
// ignores it — deliberately, and in one place each, so that a reader of
// workflow.go sees the decision rather than a call that silently cannot fail.
// The workflow's own record is already written by the time this runs.
func (s *Store) mirror(ctx context.Context, acts ...Act) error {
	if s.ledger == nil {
		return nil
	}
	for _, act := range acts {
		if err := s.ledger.Record(ctx, act); err != nil {
			return err
		}
	}
	return nil
}
