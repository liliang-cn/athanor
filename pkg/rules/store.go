package rules

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

// Engine is the half of CortexDB's rule surface this package uses.
//
// One method, and an interface rather than the brain itself, for pkg/livedb's
// reason about its importer: every test here needs a derivation that does not
// depend on what happens to be in a graph, and the two answers that matter —
// a run that derived edges and a run that hit a cap — are otherwise only
// reachable by building a graph shaped to produce them.
type Engine interface {
	ApplyRules(ctx context.Context, req cortexdb.RulesApplyRequest) (*cortexdb.RulesApplyResponse, error)
}

// Store is the rules, their acts, and the firings made under them.
type Store struct {
	db      *sql.DB
	dialect sqldialect.Dialect
	engine  Engine

	// ledger, when set, receives a copy of every act this store records. It is
	// an audit view and never an authority — see ledger.go.
	ledger Ledger

	// now is time.Now in production and a fixed clock in a test that needs two
	// acts to be distinguishable. Unexported: a caller cannot forge when a
	// decision was made.
	now func() time.Time
}

// New takes the brain's handle and its dialect and ensures the schema.
//
// The dialect comes from the parent rather than from a DSN or a guess, so this
// package cannot end up disagreeing with the handle it shares with pkg/graph —
// pkg/ontologies' rule, kept. The brain is also the default engine: the rules
// reason about the graph this Athanor serves, and firing them anywhere else
// would derive edges into a store nobody is looking at.
func New(db *cortexdb.DB, opts ...Option) (*Store, error) {
	if db == nil {
		return nil, errors.New("rules: nil cortexdb.DB")
	}
	s := &Store{
		db: db.SQL(), dialect: db.Dialect(), engine: db,
		now: func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(s)
	}
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, schemaSQL(s.dialect)); err != nil {
		return nil, fmt.Errorf("rules: create schema: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, publishedIndex); err != nil {
		return nil, fmt.Errorf("rules: create the one-published index: %w", err)
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
	return s.db.ExecContext(ctx, s.dialect.Rebind(q), args...)
}

func (s *Store) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.dialect.Rebind(q), args...)
}

func (s *Store) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.dialect.Rebind(q), args...)
}

func (s *Store) txExec(ctx context.Context, tx *sql.Tx, q string, args ...any) (sql.Result, error) {
	return tx.ExecContext(ctx, s.dialect.Rebind(q), args...)
}

func (s *Store) txQueryRow(ctx context.Context, tx *sql.Tx, q string, args ...any) *sql.Row {
	return tx.QueryRowContext(ctx, s.dialect.Rebind(q), args...)
}

// at is the clock, truncated to what both databases can hold.
//
// PostgreSQL's TIMESTAMPTZ resolves to microseconds and Go's time.Time to
// nanoseconds, so a rule published at .123456789 comes back from PostgreSQL as
// .123456 and from SQLite unchanged — the Rule returned by Publish and the row
// a later Get reads would agree on one backend and not on the other.
func (s *Store) at() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// ruleColumns is the one place the column order is decided; scanRule reads it
// back. Written out rather than SELECT *, so a column added later cannot
// silently shift what a scan lands in.
const ruleColumns = `id, lineage, state, parent, rule_text, name, confidence, weight,
	metadata, note, created_by, created_at, published_by, published_at, retired_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanRule(r rowScanner) (Rule, error) {
	var (
		v           Rule
		state       string
		metadata    string
		publishedAt sql.NullTime
		retiredAt   sql.NullTime
	)
	if err := r.Scan(
		&v.ID, &v.Lineage, &state, &v.Parent, &v.Text, &v.Name, &v.Confidence, &v.Weight,
		&metadata, &v.Note, &v.CreatedBy, &v.CreatedAt, &v.PublishedBy, &publishedAt, &retiredAt,
	); err != nil {
		return Rule{}, err
	}
	v.State = State(state)
	if metadata != "" && metadata != "{}" {
		if err := json.Unmarshal([]byte(metadata), &v.Metadata); err != nil {
			return Rule{}, fmt.Errorf("rules: %s: reading its metadata: %w", v.ID, err)
		}
	}
	if publishedAt.Valid {
		t := publishedAt.Time.UTC()
		v.PublishedAt = &t
	}
	if retiredAt.Valid {
		t := retiredAt.Time.UTC()
		v.RetiredAt = &t
	}
	v.CreatedAt = v.CreatedAt.UTC()
	return v, nil
}

// Get reads one rule by its id.
func (s *Store) Get(ctx context.Context, id string) (Rule, error) {
	return s.get(ctx, nil, id)
}

func (s *Store) get(ctx context.Context, tx *sql.Tx, id string) (Rule, error) {
	const q = `SELECT ` + ruleColumns + ` FROM athanor_rules WHERE id = ?`
	var row rowScanner
	if tx == nil {
		row = s.queryRow(ctx, q, id)
	} else {
		row = s.txQueryRow(ctx, tx, q, id)
	}
	v, err := scanRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return v, err
}

// List reports every rule, newest first within a lineage. An empty lineage is
// every lineage; an empty state is every state.
//
// ORDER BY on every query, deliberately: two backends with no order clause are
// two different answers to one call, and the divergence shows up as a flaky
// test long after the query was written.
func (s *Store) List(ctx context.Context, lineage string, state State) ([]Rule, error) {
	q := `SELECT ` + ruleColumns + ` FROM athanor_rules`
	var (
		args  []any
		where []string
	)
	if lineage != "" {
		where = append(where, `lineage = ?`)
		args = append(args, lineage)
	}
	if state != "" {
		where = append(where, `state = ?`)
		args = append(args, string(state))
	}
	for i, clause := range where {
		if i == 0 {
			q += ` WHERE ` + clause
			continue
		}
		q += ` AND ` + clause
	}
	q += ` ORDER BY lineage ASC, created_at DESC, id DESC`
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("rules: list: %w", err)
	}
	defer rows.Close()
	out := []Rule{}
	for rows.Next() {
		v, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Current is the published rule of a lineage — the one an operator fires.
func (s *Store) Current(ctx context.Context, lineage string) (Rule, error) {
	const q = `SELECT ` + ruleColumns + ` FROM athanor_rules
		WHERE lineage = ? AND state = ? ORDER BY published_at DESC, id DESC`
	v, err := scanRule(s.queryRow(ctx, q, lineage, string(Published)))
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, fmt.Errorf("%w: nothing is published in lineage %q", ErrNotFound, lineage)
	}
	return v, err
}

// Acts reports the decisions recorded about one rule, oldest first. An empty
// subject is every act.
func (s *Store) Acts(ctx context.Context, subject string) ([]Act, error) {
	return s.acts(ctx, subject, "", 0)
}

func (s *Store) acts(ctx context.Context, subject, kind string, limit int) ([]Act, error) {
	q := `SELECT id, kind, actor, at, subject, note, detail FROM athanor_rule_acts`
	var (
		args  []any
		where []string
	)
	if subject != "" {
		where = append(where, `subject = ?`)
		args = append(args, subject)
	}
	if kind != "" {
		where = append(where, `kind = ?`)
		args = append(args, kind)
	}
	for i, clause := range where {
		if i == 0 {
			q += ` WHERE ` + clause
			continue
		}
		q += ` AND ` + clause
	}
	// Oldest first when the whole history is asked for, because that is the
	// order the acts happened in and the order a reader of a workflow wants.
	// A capped read is the opposite: a limit means "the recent ones".
	if limit > 0 {
		q += ` ORDER BY at DESC, id DESC LIMIT ?`
		args = append(args, limit)
	} else {
		q += ` ORDER BY at ASC, id ASC`
	}
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("rules: acts: %w", err)
	}
	defer rows.Close()
	out := []Act{}
	for rows.Next() {
		var (
			a      Act
			detail string
		)
		if err := rows.Scan(&a.ID, &a.Kind, &a.Actor, &a.At, &a.Subject, &a.Note, &detail); err != nil {
			return nil, err
		}
		a.At = a.At.UTC()
		if detail != "" {
			if err := json.Unmarshal([]byte(detail), &a.Detail); err != nil {
				return nil, fmt.Errorf("rules: act %s: reading its detail: %w", a.ID, err)
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Firings reports what rules actually did, newest first. An empty rule is
// every rule.
//
// It reads the acts back rather than keeping a table of its own: a firing is
// an act with a result, and two tables recording one event is two tables that
// can disagree about whether it happened.
func (s *Store) Firings(ctx context.Context, rule string, limit int) ([]Firing, error) {
	if limit <= 0 {
		limit = 20
	}
	acts, err := s.acts(ctx, rule, ActApply, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Firing, 0, len(acts))
	for _, a := range acts {
		f, err := firingFrom(a)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// firingFrom rebuilds a firing out of the act that recorded it. The detail was
// written by marshalling a Firing, so this is the same trip back.
func firingFrom(a Act) (Firing, error) {
	body, err := json.Marshal(a.Detail)
	if err != nil {
		return Firing{}, fmt.Errorf("rules: firing %s: %w", a.ID, err)
	}
	var f Firing
	if err := json.Unmarshal(body, &f); err != nil {
		return Firing{}, fmt.Errorf("rules: firing %s: %w", a.ID, err)
	}
	f.Act = a.ID
	return f, nil
}

// record writes one act. Every state change in this package goes through it,
// inside whatever transaction made the change, so a decision and its record
// commit together or neither does.
//
// It returns the act it wrote, with its minted id, so the caller can hand it to
// the ledger once the transaction that made it has committed. The ledger is
// never written from in here: it is a second writer on the same handle, and
// inside an open write transaction that is a deadlock on SQLite.
//
// An id already set is kept, for the one act whose id has to exist before the
// act does: an application stamps its own id onto every edge it derives, so
// that an edge names the firing that made it and not merely the rule.
func (s *Store) record(ctx context.Context, tx *sql.Tx, act Act) (Act, error) {
	var err error
	if act.ID == "" {
		if act.ID, err = mintID(); err != nil {
			return Act{}, err
		}
	}
	detail := ""
	if len(act.Detail) > 0 {
		body, err := json.Marshal(act.Detail)
		if err != nil {
			return Act{}, fmt.Errorf("rules: record %s: %w", act.Kind, err)
		}
		detail = string(body)
	}
	const q = `INSERT INTO athanor_rule_acts (id, kind, actor, at, subject, note, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?)`
	if tx == nil {
		_, err = s.exec(ctx, q, act.ID, act.Kind, act.Actor, act.At, act.Subject, act.Note, detail)
	} else {
		_, err = s.txExec(ctx, tx, q, act.ID, act.Kind, act.Actor, act.At, act.Subject, act.Note, detail)
	}
	if err != nil {
		return Act{}, fmt.Errorf("rules: record %s: %w", act.Kind, err)
	}
	return act, nil
}
