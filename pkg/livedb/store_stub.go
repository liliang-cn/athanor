package livedb

// This file is the skeleton's floor: the unexported half of the frozen API,
// so that callers of pkg/livedb compile while the implementation is written.
// Every method here is replaced. Nothing in this file survives.

import (
	"context"
	"database/sql"
	"time"

	"github.com/liliang-cn/cortexdb/v2/pkg/connector"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
	"github.com/liliang-cn/cortexdb/v2/pkg/sqldialect"
)

type storeImpl struct {
	db      *sql.DB
	dialect sqldialect.Dialect

	ledger   Ledger
	opener   Opener
	importer Importer

	vault  connector.Vault
	keys   connector.KeyProvider
	tenant string

	now func() time.Time
}

// New takes the brain's handle and its dialect and ensures the schema.
func New(db *cortexdb.DB, opts ...Option) (*Store, error) {
	if db == nil {
		return nil, ErrNotImplemented
	}
	s := &Store{impl: &storeImpl{
		db:      db.SQL(),
		dialect: db.Dialect(),
		now:     func() time.Time { return time.Now().UTC() },
	}}
	for _, opt := range opts {
		opt(s)
	}
	return s, ErrNotImplemented
}

func sourceKey(Source) string                               { return "" }
func maskingPlan(Plan) connector.MaskingPlan                { return connector.MaskingPlan{} }
func (s *storeImpl) checkpoints() connector.CheckpointStore { return nil }

func (s *storeImpl) propose(context.Context, Source, ProposeOptions) (Plan, error) {
	return Plan{}, ErrNotImplemented
}
func (s *storeImpl) amend(context.Context, string, []Change, string) (Plan, error) {
	return Plan{}, ErrNotImplemented
}
func (s *storeImpl) sign(context.Context, string, string, string, string) (Plan, error) {
	return Plan{}, ErrNotImplemented
}
func (s *storeImpl) get(context.Context, string) (Plan, error)       { return Plan{}, ErrNotImplemented }
func (s *storeImpl) list(context.Context, ListQuery) ([]Plan, error) { return nil, ErrNotImplemented }
func (s *storeImpl) current(context.Context, string) (Plan, error)   { return Plan{}, ErrNotImplemented }
func (s *storeImpl) run(context.Context, RunRequest, string) (RunReport, error) {
	return RunReport{}, ErrNotImplemented
}
func (s *storeImpl) runs(context.Context, string, int) ([]RunReport, error) {
	return nil, ErrNotImplemented
}
