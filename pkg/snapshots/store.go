package snapshots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
	"github.com/liliang-cn/cortexdb/v2/pkg/sqldialect"
)

// Store is the named moments, on the brain's own handle.
type Store struct {
	db      *cortexdb.DB
	sql     *sql.DB
	dialect sqldialect.Dialect

	// now is time.Now in production and a fixed clock in a test that needs two
	// moments to be distinguishable. Unexported: a caller cannot forge when a
	// snapshot was taken, which is the whole of what a snapshot asserts.
	now func() time.Time
}

// Option configures a Store.
type Option func(*Store)

// WithClock fixes time, so two moments in one test are far enough apart to
// order.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// New takes the brain's handle and ensures the schema.
//
// The whole *cortexdb.DB rather than its SQL handle alone, unlike
// pkg/ontologies: this package's reads are graph reads — GraphSnapshotAt,
// ContractTally, GraphDiff — and re-implementing any of them against the
// tables underneath would be a second opinion about what the graph held,
// which is the one thing a snapshot must not be.
func New(db *cortexdb.DB, opts ...Option) (*Store, error) {
	if db == nil {
		return nil, errors.New("snapshots: nil cortexdb.DB")
	}
	s := &Store{db: db, sql: db.SQL(), dialect: db.Dialect(), now: func() time.Time { return time.Now().UTC() }}
	for _, opt := range opts {
		opt(s)
	}
	if _, err := s.sql.ExecContext(context.Background(), schemaSQL(s.dialect)); err != nil {
		return nil, fmt.Errorf("snapshots: create schema: %w", err)
	}
	return s, nil
}

// Dialect reports which SQL this store is speaking.
func (s *Store) Dialect() sqldialect.Dialect { return s.dialect }

// Every statement here is written with `?` and rebound on the way past —
// pkg/ontologies' shape, and for its reason: `?` is SQLite's spelling and a
// syntax error in PostgreSQL, and the alternative is either two copies of
// every query or a query builder standing between a reader and the SQL.
func (s *Store) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return s.sql.ExecContext(ctx, s.dialect.Rebind(q), args...)
}

func (s *Store) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.sql.QueryContext(ctx, s.dialect.Rebind(q), args...)
}

func (s *Store) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return s.sql.QueryRowContext(ctx, s.dialect.Rebind(q), args...)
}

// at is the clock, truncated to what both databases can hold.
//
// PostgreSQL's TIMESTAMPTZ resolves to microseconds and Go's time.Time to
// nanoseconds, so a moment stamped at .123456789 comes back from PostgreSQL as
// .123456 and from SQLite unchanged. Truncating where the value is minted
// makes the two agree — and here it does more than tidy a comparison: the
// stored instant is what every later as-of read is aimed at, so a stamp that
// changed on its way through a column would aim them a few hundred nanoseconds
// away from the moment that was named.
func (s *Store) at() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// columns is the one place the column order is decided; scan reads it back.
// Written out rather than SELECT *, so a column added later cannot silently
// shift what a scan lands in.
const columns = `name, at, state, actor, by_name, note, nodes, edges, orphans,
	grades, ontologies, loads, more_loads, load_count, replaced,
	dropped_actor, dropped_by, dropped_at, drop_note`

type rowScanner interface{ Scan(dest ...any) error }

func scan(r rowScanner) (Snapshot, error) {
	var (
		s          Snapshot
		state      string
		grades     string
		ontologies string
		loads      string
		moreLoads  int
		replaced   sql.NullTime
		droppedAt  sql.NullTime
	)
	if err := r.Scan(
		&s.Name, &s.At, &state, &s.Actor, &s.By, &s.Note,
		&s.Counts.Nodes, &s.Counts.Edges, &s.Counts.Orphans,
		&grades, &ontologies, &loads, &moreLoads, &s.Footing.LoadCount, &replaced,
		&s.DroppedActor, &s.DroppedBy, &droppedAt, &s.DropNote,
	); err != nil {
		return Snapshot{}, err
	}
	s.State = State(state)
	s.At = s.At.UTC()
	s.Footing.MoreLoads = moreLoads != 0
	if err := json.Unmarshal([]byte(grades), &s.Grades); err != nil {
		return Snapshot{}, fmt.Errorf("snapshots: %s: reading its grades: %w", s.Name, err)
	}
	if ontologies != "" && ontologies != "{}" {
		if err := json.Unmarshal([]byte(ontologies), &s.Footing.Ontologies); err != nil {
			return Snapshot{}, fmt.Errorf("snapshots: %s: reading its vocabularies: %w", s.Name, err)
		}
	}
	if loads != "" && loads != "[]" {
		if err := json.Unmarshal([]byte(loads), &s.Footing.Loads); err != nil {
			return Snapshot{}, fmt.Errorf("snapshots: %s: reading its loads: %w", s.Name, err)
		}
	}
	if replaced.Valid {
		t := replaced.Time.UTC()
		s.Replaced = &t
	}
	if droppedAt.Valid {
		t := droppedAt.Time.UTC()
		s.DroppedAt = &t
	}
	return s, nil
}

// Get reads one named moment.
func (s *Store) Get(ctx context.Context, name string) (Snapshot, error) {
	const q = `SELECT ` + columns + ` FROM athanor_snapshots WHERE name = ?`
	snap, err := scan(s.queryRow(ctx, q, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return snap, err
}

// ListQuery narrows a listing.
type ListQuery struct {
	// Actor keeps only the moments one key took. It is how row confinement is
	// applied: a key confined to a user_id may see what it signed and nothing
	// else, which works here for the reason it works on the ledger and did not
	// on the pipeline — a snapshot names who took it.
	Actor string
	// State keeps only kept or only dropped. Empty is kept ones only, because
	// a dropped moment is one somebody said to stop treating as current and a
	// listing that keeps offering it has not honoured that.
	State State
	Limit int
}

// List reports the named moments, newest first.
//
// ORDER BY on every query, deliberately: two backends with no order clause are
// two different answers to one call, and the divergence shows up as a flaky
// test long after the query was written.
func (s *Store) List(ctx context.Context, q ListQuery) ([]Snapshot, error) {
	query := `SELECT ` + columns + ` FROM athanor_snapshots WHERE state = ?`
	state := q.State
	if state == "" {
		state = Kept
	}
	args := []any{string(state)}
	if q.Actor != "" {
		query += ` AND actor = ?`
		args = append(args, q.Actor)
	}
	query += ` ORDER BY at DESC, name DESC`
	if q.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, q.Limit)
	}
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("snapshots: list: %w", err)
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		snap, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}
