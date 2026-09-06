// Package livedb is the door onto a database somebody else runs.
//
// CortexDB has had this path for a while — pkg/connector dials a live
// Postgres or MySQL, pkg/importflow routes its rows into RAG chunks and graph
// triples, and connector.Desensitized stands between them so that what enters
// the graph is not what is in the table. Athanor exposed none of it. This
// package is that exposure, and it adds the one thing a product needs that a
// library does not: the act of deciding is recorded, and nothing reads a row
// until somebody has signed for what leaves the database.
//
// # The shape
//
// Three steps, in this order, and the order is the point.
//
//  1. Propose. Read the source's schemas — column names, types, and a few
//     sample values — classify every column, and write a DRAFT plan. This
//     touches no row beyond the sample and puts nothing in the brain.
//  2. Sign. A person reads the plan and signs it, naming the hash they read.
//     A hash that does not match the stored plan is refused: you signed
//     something else. The signature, not a checkbox, is the ledger entry.
//  3. Run. Only a signed plan runs. Records stream through the desensitizer
//     into importflow, and the run's ledger entry names the plan's entry as
//     its premise — so "where did this node come from" walks back to a
//     signature, and from there to the column it was made of.
//
// # What is deliberately absent
//
// The DSN. A plan stores the redacted connection string and never the
// password: a caller supplies the credential again on every run, and the
// store checks that the credential it was handed describes the same database
// the plan was signed for. A brain file that leaks therefore leaks a
// hostname, not a login.
//
// # Drift
//
// A column added to the table after the plan was signed is not in the plan.
// The desensitizer already fails closed on that — DesensitizerOptions.
// OnUnlisted defaults to drop, so an unnamed column cannot leak — but silent
// correctness is not enough for something a person signed. Every run
// re-reads the schema, compares it against the plan, and reports the drift by
// name in the report and in the ledger entry. The run proceeds and the column
// is dropped; the operator learns that re-signing is owed.
package livedb

import (
	"context"
	"errors"
	"time"

	"github.com/liliang-cn/cortexdb/v2/pkg/connector"
	"github.com/liliang-cn/cortexdb/v2/pkg/importflow"
)

// Errors callers distinguish. They are values rather than strings because the
// HTTP layer maps each to a different status, and a status decided by string
// matching is a status that changes when somebody edits a message.
var (
	// ErrNoPlan is a plan id nothing answers to.
	ErrNoPlan = errors.New("livedb: no such plan")
	// ErrUnsigned is a run against a plan nobody signed.
	ErrUnsigned = errors.New("livedb: the plan is not signed")
	// ErrStaleHash is a signature naming a hash the plan no longer has —
	// somebody amended it between the reading and the signing.
	ErrStaleHash = errors.New("livedb: the plan changed since you read it")
	// ErrNotDraft is an amendment to a plan that is already signed. A signed
	// plan is never edited in place; superseding it is the only change.
	ErrNotDraft = errors.New("livedb: a signed plan is not edited, it is superseded")
	// ErrWrongSource is a run whose credential describes a different database
	// from the one the plan was signed for.
	ErrWrongSource = errors.New("livedb: the credential does not describe the database this plan was signed for")
	// ErrNoVault is a plan holding a reversible treatment with nowhere to put
	// the original. Refused at signing, not at running: the operator learns
	// it while deciding rather than an hour into an import.
	ErrNoVault = errors.New("livedb: the plan has a reversible treatment but this Athanor has no vault")
)

// Source names one live database and the part of it a plan covers.
//
// DSN is the only field carrying a secret, and it is the only field never
// persisted and never returned. Everything else is what a reader of the plan
// needs in order to know which database this was.
type Source struct {
	// Driver is "postgres" or "mysql".
	Driver string `json:"driver"`
	// DSN is the connection string. It is cleared on the way out of this
	// package; see Plan.Source.
	DSN string `json:"dsn,omitempty"`
	// Redacted is the DSN with the password removed, which is what is stored
	// and shown.
	Redacted string `json:"redacted,omitempty"`
	// Schema is the database schema. Empty takes the driver's default
	// ("public" on Postgres).
	Schema string `json:"schema,omitempty"`
	// Tables is the allow-list. Empty means every base table, which Propose
	// resolves to the real list so a plan always names what it covers.
	Tables []string `json:"tables,omitempty"`
}

// Key is the stable identity of a (database, schema, tableset). It is the
// plan's lineage and the change stream's checkpoint key, and it is derived
// from the redacted DSN so that it is the same string whether or not the
// caller's credential rotated.
func (s Source) Key() string { return sourceKey(s) }

// Relation is one foreign key, discovered rather than declared: it is what
// turns a set of rows into a graph instead of a pile of nodes.
type Relation struct {
	Table      string   `json:"table"`
	Columns    []string `json:"columns"`
	Predicate  string   `json:"predicate"`
	Target     string   `json:"target"`
	TargetKeys []string `json:"target_keys"`
}

// Live is a source open for reading, plus the two questions importflow cannot
// ask it. A primary key is what a chunk is addressed by and what a change
// stream deletes on; a foreign key is what an edge is made of. Neither is on
// importflow.Source, so this interface adds them.
type Live interface {
	importflow.Source
	// Keys returns a table's primary-key columns, empty when it has none.
	Keys(ctx context.Context, table string) ([]string, error)
	// Relations returns a table's outbound foreign keys.
	Relations(ctx context.Context, table string) ([]Relation, error)
}

// Opener dials. It is an interface with one real implementation because every
// test in this package needs a source that is not a database — and because a
// fake that satisfies it is the only way to test the drift rule, which needs
// a schema that changes between two calls.
type Opener interface {
	// Open connects and returns the source for reading.
	Open(ctx context.Context, src Source) (Live, error)
	// Changes opens the source's change stream, resuming from cp. A driver
	// with no change stream configured returns an error naming what is
	// missing rather than a stream that silently never fires.
	Changes(ctx context.Context, src Source, cp connector.Checkpoint) (connector.ChangeSource, error)
}

// Treatment is one column's row on the plan: what it is, what was decided,
// and — the column a reviewer actually reads — what that decision does to a
// real value.
type Treatment struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	// Type is importflow's small type vocabulary: text, integer, number,
	// timestamp, or empty.
	Type string `json:"type,omitempty"`

	Kind        connector.PiiKind     `json:"pii_kind"`
	Sensitivity connector.Sensitivity `json:"sensitivity"`
	Action      connector.MaskAction  `json:"action"`
	// Reason is the classifier's note, and Source is who classified: "rule"
	// for the built-in classifier, "human" once somebody has changed it.
	Reason string `json:"reason,omitempty"`
	By     string `json:"by,omitempty"`

	// Sample is what the reviewer is shown of the current value, and Enters
	// is what the chosen treatment turns it into.
	//
	// A sample of a column the classifier believes is personal is shown
	// MASKED, not raw. The purpose of a sample is to let a person check the
	// guess, and the masked form does that — an address still reads as an
	// address — without putting the value on a screen, in a log, and in an
	// HTTP response before anybody has agreed it may leave the database. A
	// column classified as not personal is shown as it is, which is exactly
	// the case where a reviewer must be able to see the classifier was wrong.
	Sample string `json:"sample,omitempty"`
	Enters string `json:"enters,omitempty"`

	// Scan marks the column for free-text PII scanning on top of the
	// column-level treatment: the value is kept, and anything inside it that
	// reads like a phone number or an address is masked in place.
	//
	// It is on the treatment rather than in a list beside it so that the plan
	// a person reads and the plan the desensitizer is built from are one
	// object. A scan rule that lived only in a side table would be invisible
	// to Masking(), which is the rendering an auditor reads — and a review
	// screen that shows less than what ran is the failure this whole package
	// exists to prevent. It is hashed for the same reason: it changes what
	// leaves the database, so it is part of what was signed.
	Scan bool `json:"scan,omitempty"`
}

// Counts is the plan in one line, the summary the review screen leads with.
type Counts struct {
	Columns     int `json:"columns"`
	Personal    int `json:"personal"`
	Dropped     int `json:"dropped"`
	Masked      int `json:"masked"`
	Generalized int `json:"generalized"`
	Hashed      int `json:"hashed"`
	Redacted    int `json:"redacted"`
	Passed      int `json:"passed"`
	// Reversible is how many columns can be turned back, which is the number
	// the vault paragraph is about.
	Reversible int `json:"reversible"`
}

// State is where a plan is in its life.
type State string

const (
	// Draft is proposed and amendable and cannot run.
	Draft State = "draft"
	// Signed is the one plan currently in force for its source.
	Signed State = "signed"
	// Superseded is a plan a later signature replaced. It is kept, because a
	// run that happened under it is only explicable if it still exists.
	Superseded State = "superseded"
)

// Plan is the reviewable decision: what is read, what is masked, what never
// leaves.
type Plan struct {
	ID string `json:"id"`
	// SourceKey groups a plan with its predecessors over the same database.
	SourceKey string `json:"source_key"`
	// Source carries the redacted DSN; the DSN field is always empty here.
	Source Source `json:"source"`

	Columns []Treatment `json:"columns"`
	// Hash is sha256 over the canonical rendering of Source and Columns. It is
	// what a signature names, and what makes "I signed this" checkable.
	Hash   string `json:"hash"`
	State  State  `json:"state"`
	Counts Counts `json:"counts"`

	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	SignedBy  string    `json:"signed_by,omitempty"`
	SignedAt  time.Time `json:"signed_at,omitempty"`
	// Supersedes is the plan this one replaced, when it replaced one.
	Supersedes string `json:"supersedes,omitempty"`
	Note       string `json:"note,omitempty"`
}

// Masking renders the plan as the connector's own type, which is what the
// desensitizer takes. A plan that is not signed renders unsigned, and
// connector.NewDesensitizer refuses it — the same refusal, enforced twice, in
// this package and in the one below it.
func (p Plan) Masking() connector.MaskingPlan { return maskingPlan(p) }

// ProposeOptions tunes a proposal. Every field has a defensible zero.
type ProposeOptions struct {
	// SampleSize is how many rows per table are read for classification.
	// Zero takes the connector's default of five.
	SampleSize int `json:"sample_size,omitempty"`
	// ActionFor overrides the treatment chosen for a PII kind, for an
	// operator who has a house rule ("we never keep an email, even masked").
	ActionFor map[connector.PiiKind]connector.MaskAction `json:"action_for,omitempty"`
	// ScanText marks text columns for free-text PII scanning as well as
	// column-level classification.
	ScanText bool `json:"scan_text,omitempty"`
	// By is the key id proposing. It is not a signature.
	By   string `json:"by,omitempty"`
	Note string `json:"note,omitempty"`
}

// Change is one edit to a draft: a human overriding the classifier.
type Change struct {
	Table  string               `json:"table"`
	Column string               `json:"column"`
	Action connector.MaskAction `json:"action"`
	Kind   *connector.PiiKind   `json:"pii_kind,omitempty"`
	Reason string               `json:"reason,omitempty"`
}

// RunRequest asks for one import under a signed plan.
type RunRequest struct {
	// Plan is the signed plan's id.
	Plan string `json:"plan"`
	// DSN is the credential, supplied again because the store never kept it.
	DSN string `json:"dsn"`
	// Mapping states how rows become chunks and triples. Nil derives one from
	// the schema — primary key as the id, kept columns as properties, foreign
	// keys as edges — which is deterministic and calls no model. Supplying
	// one is how a caller who knows their schema does better.
	Mapping *importflow.MappingPlan `json:"mapping,omitempty"`
	// Namespace is the RAG namespace rows land in. Empty takes the plan id.
	Namespace string `json:"namespace,omitempty"`
	// Follow keeps the brain in step with the database after the first pass,
	// reading changes through this same plan. It returns when ctx is done.
	Follow bool `json:"follow,omitempty"`
	// DryRun reads and desensitizes but writes nothing, so an operator can
	// see the drift and the row count before committing to either.
	DryRun bool `json:"dry_run,omitempty"`
}

// RunReport is what one import did.
type RunReport struct {
	ID        string    `json:"id"`
	Plan      string    `json:"plan"`
	SourceKey string    `json:"source_key"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	DryRun    bool      `json:"dry_run,omitempty"`

	RowsRead int `json:"rows_read"`
	Chunks   int `json:"chunks"`
	Triples  int `json:"triples"`
	Skipped  int `json:"skipped"`

	// Drift names columns the database has and the signed plan does not.
	// They were dropped. A non-empty Drift means a re-signing is owed.
	Drift []string `json:"drift,omitempty"`
	// Gone names columns the plan has and the database no longer does.
	Gone []string `json:"gone,omitempty"`
	// Errors are per-row failures, collected rather than fatal.
	Errors []string `json:"errors,omitempty"`
}

// Act is one recorded decision of this package's, shaped for the ledger it
// becomes an entry in. It mirrors pkg/ontologies.Act deliberately: the server
// turns both into the same kind of thing.
type Act struct {
	ID      string         `json:"id"`
	Kind    string         `json:"kind"`
	Actor   string         `json:"actor"`
	At      time.Time      `json:"at"`
	Subject string         `json:"subject"`
	Note    string         `json:"note,omitempty"`
	Detail  map[string]any `json:"detail,omitempty"`
	// Premises are the SUBJECTS of the acts this one rests on — a run rests
	// on its plan, so a run's premise is the plan's id.
	//
	// Subjects rather than entry ids because this package does not know how
	// the ledger numbers its entries, and an id invented here to look like
	// one would be a premise that can never resolve. The ledger keys its
	// entries by subject already, so translating a subject into the entry
	// that holds it is one call on the side that owns the numbering.
	Premises []string `json:"premises,omitempty"`
}

// The kinds this package records.
const (
	ActPropose = "livedb.propose"
	ActSign    = "livedb.sign"
	ActRun     = "livedb.run"
)

// Ledger mirrors committed acts. Narrow on purpose, like the ontology
// store's: a Store with no ledger works and mirrors nothing, which is what
// lets every test here run with no brain.
type Ledger interface {
	// Record is called after the act has committed, never inside its
	// transaction.
	Record(ctx context.Context, act Act) error
}

// Store is the plans, the runs and the change-stream checkpoints, on the
// brain's own handle — pkg/ontologies' arrangement, for its reasons.
type Store struct {
	// unexported; see store.go
	impl *storeImpl
}

// Option configures a Store.
type Option func(*Store)

// WithLedger mirrors every act into the ledger.
func WithLedger(l Ledger) Option { return func(s *Store) { s.impl.ledger = l } }

// WithOpener replaces the dialer. The real one is used when this is not set.
func WithOpener(o Opener) Option { return func(s *Store) { s.impl.opener = o } }

// WithVault gives reversible treatments somewhere to put the original. With
// no vault, signing a plan that holds one is refused.
func WithVault(v connector.Vault, kp connector.KeyProvider, tenant string) Option {
	return func(s *Store) { s.impl.vault, s.impl.keys, s.impl.tenant = v, kp, tenant }
}

// WithClock fixes time, so two acts in one test are distinguishable.
func WithClock(now func() time.Time) Option { return func(s *Store) { s.impl.now = now } }

// WithImporter replaces the import engine. The real one is
// importflow.New(db); a test supplies one that counts what it was given.
func WithImporter(im Importer) Option { return func(s *Store) { s.impl.importer = im } }

// Importer is the half of importflow.Importer this package uses.
type Importer interface {
	Run(ctx context.Context, src importflow.Source, plan importflow.MappingPlan) (*importflow.Report, error)
}

// ListQuery filters List.
type ListQuery struct {
	SourceKey string `json:"source_key,omitempty"`
	State     State  `json:"state,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

// Propose reads the source and writes a draft plan.
func (s *Store) Propose(ctx context.Context, src Source, opts ProposeOptions) (Plan, error) {
	return s.impl.propose(ctx, src, opts)
}

// Amend applies a human's overrides to a draft, returning the plan with a new
// hash. A signed plan is refused with ErrNotDraft.
func (s *Store) Amend(ctx context.Context, id string, changes []Change, by string) (Plan, error) {
	return s.impl.amend(ctx, id, changes, by)
}

// Sign puts the plan in force. hash must be the hash the signer read.
func (s *Store) Sign(ctx context.Context, id, hash, by, note string) (Plan, error) {
	return s.impl.sign(ctx, id, hash, by, note)
}

// Get returns one plan.
func (s *Store) Get(ctx context.Context, id string) (Plan, error) { return s.impl.get(ctx, id) }

// List returns plans newest first.
func (s *Store) List(ctx context.Context, q ListQuery) ([]Plan, error) { return s.impl.list(ctx, q) }

// Current returns the signed plan in force for a source key, if there is one.
func (s *Store) Current(ctx context.Context, sourceKey string) (Plan, error) {
	return s.impl.current(ctx, sourceKey)
}

// Run imports under a signed plan.
func (s *Store) Run(ctx context.Context, req RunRequest, actor string) (RunReport, error) {
	return s.impl.run(ctx, req, actor)
}

// Runs returns a plan's runs, newest first.
func (s *Store) Runs(ctx context.Context, planID string, limit int) ([]RunReport, error) {
	return s.impl.runs(ctx, planID, limit)
}

// Checkpoints is the change stream's resume position, on this store's own
// dialect-aware table.
//
// It is here rather than connector.NewSQLiteCheckpointStore because that one
// is SQLite-only in fact as well as in name — it writes `?` placeholders and
// `datetime('now')`, both of which are errors on the PostgreSQL backend
// Athanor supports. Co-locating it with the plans has a second virtue: a
// backup of the brain carries the plan and the position it had reached, so a
// restore resumes instead of re-importing.
func (s *Store) Checkpoints() connector.CheckpointStore { return s.impl.checkpoints() }
