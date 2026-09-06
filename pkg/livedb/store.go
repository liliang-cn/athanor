package livedb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/liliang-cn/cortexdb/v2/pkg/connector"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
	"github.com/liliang-cn/cortexdb/v2/pkg/importflow"
	"github.com/liliang-cn/cortexdb/v2/pkg/sqldialect"
)

// storeImpl is the unexported half of Store. The exported type in livedb.go is
// frozen; everything that can change lives here.
type storeImpl struct {
	brain   *cortexdb.DB
	db      *sql.DB
	dialect sqldialect.Dialect

	ledger   Ledger
	opener   Opener
	importer Importer

	vault  connector.Vault
	keys   connector.KeyProvider
	tenant string

	// now is time.Now in production and a fixed clock in a test that needs
	// two acts to be distinguishable. Unexported: a caller cannot forge when
	// a decision was made.
	now func() time.Time
}

// New takes the brain's handle and its dialect and ensures the schema.
//
// The dialect comes from the parent rather than from a DSN or a guess, so this
// package cannot end up disagreeing with the handle it shares with pkg/graph
// and pkg/cortexdb — pkg/agentmem's rule, kept by pkg/ontologies and kept
// here.
func New(db *cortexdb.DB, opts ...Option) (*Store, error) {
	if db == nil {
		return nil, errors.New("livedb: nil cortexdb.DB")
	}
	s := &Store{impl: &storeImpl{
		brain:    db,
		db:       db.SQL(),
		dialect:  db.Dialect(),
		opener:   liveOpener{},
		importer: importflow.New(db),
		now:      func() time.Time { return time.Now().UTC() },
	}}
	for _, opt := range opts {
		opt(s)
	}
	ctx := context.Background()
	if _, err := s.impl.db.ExecContext(ctx, schemaSQL(s.impl.dialect)); err != nil {
		return nil, fmt.Errorf("livedb: create schema: %w", err)
	}
	if _, err := s.impl.db.ExecContext(ctx, signedIndex); err != nil {
		return nil, fmt.Errorf("livedb: create the one-signed index: %w", err)
	}
	return s, nil
}

// Dialect reports which SQL this store is speaking.
func (s *Store) Dialect() sqldialect.Dialect { return s.impl.dialect }

// Every statement here is written with `?` and rebound on the way past —
// pkg/agentmem's shape and pkg/ontologies', for their reason: `?` is SQLite's
// spelling and a syntax error in PostgreSQL, and the alternative is either two
// copies of every query or a query builder standing between a reader and the
// SQL.
func (s *storeImpl) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.dialect.Rebind(q), args...)
}

func (s *storeImpl) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.dialect.Rebind(q), args...)
}

func (s *storeImpl) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.dialect.Rebind(q), args...)
}

func (s *storeImpl) txExec(ctx context.Context, tx *sql.Tx, q string, args ...any) (sql.Result, error) {
	return tx.ExecContext(ctx, s.dialect.Rebind(q), args...)
}

func (s *storeImpl) txQueryRow(ctx context.Context, tx *sql.Tx, q string, args ...any) *sql.Row {
	return tx.QueryRowContext(ctx, s.dialect.Rebind(q), args...)
}

// planColumns is the one place the column order is decided; scanPlan reads it
// back. Written out rather than SELECT *, so a column added later cannot
// silently shift what a scan lands in.
const planColumns = `id, source_key, driver, redacted, db_schema, tables, treatments,
	hash, state, counts, created_by, created_at, signed_by, signed_at, sign_act, supersedes, note`

type rowScanner interface{ Scan(dest ...any) error }

// storedPlan is the row plus the two fields the frozen Plan has no home for.
type storedPlan struct {
	Plan
	// signAct is the ledger id of the signature. A run names it as its
	// premise, which is what makes "where did this node come from" walk back
	// to somebody's name.
	signAct string
}

func scanPlan(r rowScanner) (storedPlan, error) {
	var (
		p          storedPlan
		tables     string
		treatments string
		counts     string
		state      string
		signedAt   sql.NullTime
	)
	if err := r.Scan(
		&p.ID, &p.SourceKey, &p.Source.Driver, &p.Source.Redacted, &p.Source.Schema,
		&tables, &treatments, &p.Hash, &state, &counts,
		&p.CreatedBy, &p.CreatedAt, &p.SignedBy, &signedAt, &p.signAct, &p.Supersedes, &p.Note,
	); err != nil {
		return storedPlan{}, err
	}
	p.State = State(state)
	for _, d := range []struct {
		raw string
		to  any
	}{{tables, &p.Source.Tables}, {treatments, &p.Columns}, {counts, &p.Counts}} {
		if d.raw == "" {
			continue
		}
		if err := json.Unmarshal([]byte(d.raw), d.to); err != nil {
			return storedPlan{}, fmt.Errorf("livedb: plan %s: reading it back: %w", p.ID, err)
		}
	}
	if signedAt.Valid {
		p.SignedAt = signedAt.Time.UTC()
	}
	p.CreatedAt = p.CreatedAt.UTC()
	// The DSN is not a column, so there is nothing to clear — but saying so
	// where a reader looks for it is cheaper than making them check.
	p.Source.DSN = ""
	return p, nil
}

func (s *storeImpl) get(ctx context.Context, id string) (Plan, error) {
	p, err := s.load(ctx, nil, id)
	return p.Plan, err
}

func (s *storeImpl) load(ctx context.Context, tx *sql.Tx, id string) (storedPlan, error) {
	const q = `SELECT ` + planColumns + ` FROM athanor_livedb_plans WHERE id = ?`
	var row rowScanner
	if tx == nil {
		row = s.queryRow(ctx, q, id)
	} else {
		row = s.txQueryRow(ctx, tx, q, id)
	}
	p, err := scanPlan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return storedPlan{}, fmt.Errorf("%w: %s", ErrNoPlan, id)
	}
	return p, err
}

// list returns plans newest first. ORDER BY on every query, deliberately: two
// backends with no order clause are two different answers to one call, and the
// divergence shows up as a flaky test long after the query was written.
func (s *storeImpl) list(ctx context.Context, q ListQuery) ([]Plan, error) {
	sqlText := `SELECT ` + planColumns + ` FROM athanor_livedb_plans`
	var (
		where []string
		args  []any
	)
	if q.SourceKey != "" {
		where = append(where, `source_key = ?`)
		args = append(args, q.SourceKey)
	}
	if q.State != "" {
		where = append(where, `state = ?`)
		args = append(args, string(q.State))
	}
	for i, w := range where {
		if i == 0 {
			sqlText += ` WHERE ` + w
			continue
		}
		sqlText += ` AND ` + w
	}
	sqlText += ` ORDER BY created_at DESC, id DESC`
	if q.Limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, q.Limit)
	}
	rows, err := s.query(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("livedb: list plans: %w", err)
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Plan)
	}
	return out, rows.Err()
}

func (s *storeImpl) current(ctx context.Context, sourceKey string) (Plan, error) {
	const q = `SELECT ` + planColumns + ` FROM athanor_livedb_plans
		WHERE source_key = ? AND state = ? ORDER BY signed_at DESC, id DESC`
	p, err := scanPlan(s.queryRow(ctx, q, sourceKey, string(Signed)))
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, fmt.Errorf("%w: nothing is signed for %s", ErrNoPlan, sourceKey)
	}
	return p.Plan, err
}

func (s *storeImpl) runs(ctx context.Context, planID string, limit int) ([]RunReport, error) {
	q := `SELECT id, plan, source_key, started_at, ended_at, dry_run,
		rows_read, chunks, triples, skipped, drift, gone, errors
		FROM athanor_livedb_runs`
	var args []any
	if planID != "" {
		q += ` WHERE plan = ?`
		args = append(args, planID)
	}
	q += ` ORDER BY started_at DESC, id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("livedb: runs of %s: %w", planID, err)
	}
	defer rows.Close()
	out := []RunReport{}
	for rows.Next() {
		var (
			r                 RunReport
			dry               int
			drift, gone, errs string
		)
		if err := rows.Scan(&r.ID, &r.Plan, &r.SourceKey, &r.StartedAt, &r.EndedAt, &dry,
			&r.RowsRead, &r.Chunks, &r.Triples, &r.Skipped, &drift, &gone, &errs); err != nil {
			return nil, err
		}
		r.DryRun = dry != 0
		r.StartedAt, r.EndedAt = r.StartedAt.UTC(), r.EndedAt.UTC()
		for _, d := range []struct {
			raw string
			to  *[]string
		}{{drift, &r.Drift}, {gone, &r.Gone}, {errs, &r.Errors}} {
			if d.raw == "" || d.raw == "[]" {
				continue
			}
			if err := json.Unmarshal([]byte(d.raw), d.to); err != nil {
				return nil, fmt.Errorf("livedb: run %s: reading it back: %w", r.ID, err)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// mirror hands committed acts to the ledger, if there is one.
//
// It returns the first failure rather than swallowing it, and every caller
// ignores it — deliberately, and in one place each, so that a reader sees the
// decision rather than a call that silently cannot fail. The act it describes
// has already committed by the time this runs; that is the whole rule, and the
// reason is that a ledger write is a second writer on the same handle, and a
// second writer inside an open write transaction is a deadlock on SQLite.
func (s *storeImpl) mirror(ctx context.Context, acts ...Act) error {
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

// at is the clock, truncated to what both databases can hold.
//
// PostgreSQL's TIMESTAMPTZ resolves to microseconds and Go's time.Time to
// nanoseconds, so a plan signed at .123456789 comes back from PostgreSQL as
// .123456 and from SQLite unchanged: the Plan returned by Sign and the row a
// later Get reads would agree on one backend and not on the other, and the
// caller most likely to compare them is an auditor asking whether the plan on
// record is the plan somebody signed. Truncating where the value is minted
// makes the answer the same everywhere and costs nothing anybody can observe
// — nothing here is ordered at sub-microsecond resolution, and where two
// timestamps do tie the id breaks it.
func (s *storeImpl) at() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// mintID names a plan, a run or an act. Not a sequence: a BIGSERIAL and an
// INTEGER PRIMARY KEY AUTOINCREMENT are the one thing the two dialects cannot
// be handed the same DDL for, and none of these needs its id to be ordered —
// created_at and started_at order them.
func mintID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("livedb: mint %s id: %w", prefix, err)
	}
	return prefix + "_" + hex.EncodeToString(b[:]), nil
}

// checkpoints is the change stream's resume position on this store's own
// table. See Store.Checkpoints for why it is not
// connector.NewSQLiteCheckpointStore.
func (s *storeImpl) checkpoints() connector.CheckpointStore { return checkpointStore{s: s} }

type checkpointStore struct{ s *storeImpl }

func (c checkpointStore) Load(ctx context.Context, sourceKey string) (connector.Checkpoint, bool, error) {
	const q = `SELECT cursor_json, pos FROM athanor_livedb_checkpoints WHERE source_key = ?`
	var cp connector.Checkpoint
	err := c.s.queryRow(ctx, q, sourceKey).Scan(&cp.Cursor, &cp.Position)
	if errors.Is(err, sql.ErrNoRows) {
		return connector.Checkpoint{}, false, nil
	}
	if err != nil {
		return connector.Checkpoint{}, false, fmt.Errorf("livedb: load checkpoint %s: %w", sourceKey, err)
	}
	return cp, true, nil
}

func (c checkpointStore) Save(ctx context.Context, sourceKey string, cp connector.Checkpoint) error {
	// The timestamp is this store's clock rather than datetime('now'), which
	// is the other half of why this table is here: the function name differs
	// between the backends and a test's fixed clock has to reach it.
	const q = `INSERT INTO athanor_livedb_checkpoints (source_key, cursor_json, pos, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (source_key) DO UPDATE SET
			cursor_json = excluded.cursor_json, pos = excluded.pos, updated_at = excluded.updated_at`
	if _, err := c.s.exec(ctx, q, sourceKey, cp.Cursor, cp.Position, c.s.at()); err != nil {
		return fmt.Errorf("livedb: save checkpoint %s: %w", sourceKey, err)
	}
	return nil
}
