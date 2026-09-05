package ontologies

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

// Store is the versions and the acts, on the brain's handle.
type Store struct {
	db      *sql.DB
	dialect sqldialect.Dialect

	// now is time.Now in production and a fixed clock in a test that needs
	// two acts to be distinguishable. Unexported: a caller cannot forge when
	// a decision was made.
	now func() time.Time
}

// New takes the brain's handle and its dialect and ensures the schema.
//
// The dialect comes from the parent rather than from a DSN or a guess, so this
// package cannot end up disagreeing with the handle it shares with pkg/graph
// and pkg/cortexdb — agentmem's rule, kept.
func New(db *cortexdb.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("ontologies: nil cortexdb.DB")
	}
	s := &Store{db: db.SQL(), dialect: db.Dialect(), now: func() time.Time { return time.Now().UTC() }}
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, schemaSQL(s.dialect)); err != nil {
		return nil, fmt.Errorf("ontologies: create schema: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, publishedIndex); err != nil {
		return nil, fmt.Errorf("ontologies: create the one-published index: %w", err)
	}
	return s, nil
}

// Dialect reports which SQL this store is speaking.
func (s *Store) Dialect() sqldialect.Dialect { return s.dialect }

// Every statement here is written with `?` and rebound on the way past —
// pkg/agentmem's shape, and for its reason: `?` is SQLite's spelling and a
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

// versionColumns is the one place the column order is decided; scanVersion
// reads it back. Written out rather than SELECT *, so a column added later
// cannot silently shift what a scan lands in.
const versionColumns = `id, lineage, body, state, parent, part, proposals, proposed_from,
	created_by, created_at, approved_by, approved_at, published_at, retired_at, note`

type rowScanner interface{ Scan(dest ...any) error }

func scanVersion(r rowScanner) (Version, error) {
	var (
		v           Version
		body        string
		state       string
		proposals   string
		approvedAt  sql.NullTime
		publishedAt sql.NullTime
		retiredAt   sql.NullTime
	)
	if err := r.Scan(
		&v.ID, &v.Lineage, &body, &state, &v.Parent, &v.Part, &proposals, &v.ProposedFrom,
		&v.CreatedBy, &v.CreatedAt, &v.ApprovedBy, &approvedAt, &publishedAt, &retiredAt, &v.Note,
	); err != nil {
		return Version{}, err
	}
	v.State = State(state)
	v.Document = []byte(body)
	if proposals != "" && proposals != "[]" {
		if err := json.Unmarshal([]byte(proposals), &v.Proposals); err != nil {
			return Version{}, fmt.Errorf("ontologies: version %s: reading its proposals: %w", v.ID, err)
		}
	}
	if approvedAt.Valid {
		t := approvedAt.Time.UTC()
		v.ApprovedAt = &t
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

// Get reads one version by its ontology id.
func (s *Store) Get(ctx context.Context, id string) (Version, error) {
	return s.get(ctx, nil, id)
}

func (s *Store) get(ctx context.Context, tx *sql.Tx, id string) (Version, error) {
	const q = `SELECT ` + versionColumns + ` FROM athanor_ontology_versions WHERE id = ?`
	var row rowScanner
	if tx == nil {
		row = s.queryRow(ctx, q, id)
	} else {
		row = s.txQueryRow(ctx, tx, q, id)
	}
	v, err := scanVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return v, err
}

// List reports every version, newest first within a lineage. An empty lineage
// is every lineage.
//
// ORDER BY on every query, deliberately: two backends with no order clause are
// two different answers to one call, and the divergence shows up as a flaky
// test long after the query was written.
func (s *Store) List(ctx context.Context, lineage string) ([]Version, error) {
	q := `SELECT ` + versionColumns + ` FROM athanor_ontology_versions`
	var args []any
	if lineage != "" {
		q += ` WHERE lineage = ?`
		args = append(args, lineage)
	}
	q += ` ORDER BY lineage ASC, created_at DESC, id DESC`
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ontologies: list: %w", err)
	}
	defer rows.Close()
	out := []Version{}
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Current is the published version of a lineage — the JSON a client pastes
// into CreateJob.
func (s *Store) Current(ctx context.Context, lineage string) (Version, error) {
	const q = `SELECT ` + versionColumns + ` FROM athanor_ontology_versions
		WHERE lineage = ? AND state = ? ORDER BY published_at DESC, id DESC`
	v, err := scanVersion(s.queryRow(ctx, q, lineage, string(Published)))
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, fmt.Errorf("%w: nothing is published in lineage %q", ErrNotFound, lineage)
	}
	return v, err
}

// Acts reports the decisions recorded about one version, oldest first. An
// empty subject is every act.
func (s *Store) Acts(ctx context.Context, subject string) ([]Act, error) {
	q := `SELECT id, kind, actor, at, subject, note FROM athanor_ontology_acts`
	var args []any
	if subject != "" {
		q += ` WHERE subject = ?`
		args = append(args, subject)
	}
	q += ` ORDER BY at ASC, id ASC`
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ontologies: acts: %w", err)
	}
	defer rows.Close()
	out := []Act{}
	for rows.Next() {
		var a Act
		if err := rows.Scan(&a.ID, &a.Kind, &a.Actor, &a.At, &a.Subject, &a.Note); err != nil {
			return nil, err
		}
		a.At = a.At.UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// record writes one act. Every state change in this package goes through it,
// inside whatever transaction made the change, so a decision and its record
// commit together or neither does.
func (s *Store) record(ctx context.Context, tx *sql.Tx, kind, actor, subject, note string, at time.Time) error {
	id, err := mintID()
	if err != nil {
		return err
	}
	const q = `INSERT INTO athanor_ontology_acts (id, kind, actor, at, subject, note) VALUES (?, ?, ?, ?, ?, ?)`
	if tx == nil {
		_, err = s.exec(ctx, q, id, kind, actor, at, subject, note)
	} else {
		_, err = s.txExec(ctx, tx, q, id, kind, actor, at, subject, note)
	}
	if err != nil {
		return fmt.Errorf("ontologies: record %s: %w", kind, err)
	}
	return nil
}
