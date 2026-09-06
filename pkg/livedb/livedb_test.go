package livedb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liliang-cn/cortexdb/v2/pkg/connector"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
	"github.com/liliang-cn/cortexdb/v2/pkg/importflow"
)

// The whole suite runs with no live database and no brain beyond a temp-file
// CortexDB. The fake below is not a convenience: it is what makes the drift
// rule testable at all, because drift needs a schema that CHANGES between the
// propose call and the run call, and no fixture over a real database can do
// that without a migration in the middle of a test.
//
// PostgreSQL parity: pkg/ontologies runs its store suite on both backends
// behind ATHANOR_TEST_POSTGRES. This one runs on SQLite only. Everything it
// exercises goes through the same Rebind-on-the-way-past helpers and the
// schema is dialect-chosen, but that is an argument, not a test, and it is
// said here rather than left to be discovered.

// ---------------------------------------------------------------- the fakes

type fakeLive struct {
	schemas []importflow.Schema
	keys    map[string][]string
	rels    map[string][]Relation
	rows    []importflow.Record
	closed  atomic.Int32
}

func (f *fakeLive) Schemas(context.Context) ([]importflow.Schema, error) {
	out := make([]importflow.Schema, len(f.schemas))
	copy(out, f.schemas)
	return out, nil
}

func (f *fakeLive) Records(ctx context.Context, fn func(importflow.Record) error) error {
	for _, r := range f.rows {
		if err := fn(cloneRecord(r)); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeLive) Close() error { f.closed.Add(1); return nil }

func (f *fakeLive) Keys(_ context.Context, table string) ([]string, error) {
	return f.keys[table], nil
}

func (f *fakeLive) Relations(_ context.Context, table string) ([]Relation, error) {
	return f.rels[table], nil
}

func cloneRecord(r importflow.Record) importflow.Record {
	out := importflow.Record{Table: r.Table, Row: r.Row, Values: map[string]string{}, Nulls: map[string]bool{}}
	for k, v := range r.Values {
		out.Values[k] = v
	}
	for k, v := range r.Nulls {
		out.Nulls[k] = v
	}
	return out
}

// fakeOpener hands out whatever live source the test currently wants, which is
// how a schema changes between propose and run.
type fakeOpener struct {
	live    *fakeLive
	changes connector.ChangeSource
	opens   atomic.Int32
}

func (f *fakeOpener) Open(context.Context, Source) (Live, error) {
	f.opens.Add(1)
	return f.live, nil
}

func (f *fakeOpener) Changes(context.Context, Source, connector.Checkpoint) (connector.ChangeSource, error) {
	if f.changes == nil {
		return nil, errors.New("no change stream in this test")
	}
	return f.changes, nil
}

// fakeImporter counts what it was given and keeps it, so a test can assert on
// the records that actually reached the import rather than on a report's idea
// of them.
type fakeImporter struct {
	plans []importflow.MappingPlan
	got   []importflow.Record
	runs  int
}

func (f *fakeImporter) Run(ctx context.Context, src importflow.Source, plan importflow.MappingPlan) (*importflow.Report, error) {
	f.runs++
	f.plans = append(f.plans, plan)
	rep := &importflow.Report{}
	err := src.Records(ctx, func(r importflow.Record) error {
		f.got = append(f.got, r)
		rep.RowsRead++
		rep.ChunksIndexed++
		return nil
	})
	return rep, err
}

func (f *fakeImporter) columns(table string) map[string]bool {
	out := map[string]bool{}
	for _, r := range f.got {
		if r.Table != table {
			continue
		}
		for c := range r.Values {
			out[c] = true
		}
		for c := range r.Nulls {
			out[c] = true
		}
	}
	return out
}

type fakeLedger struct{ acts []Act }

func (l *fakeLedger) Record(_ context.Context, a Act) error {
	l.acts = append(l.acts, a)
	return nil
}

func (l *fakeLedger) of(kind string) []Act {
	var out []Act
	for _, a := range l.acts {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

// memVault is the smallest thing that satisfies connector.Vault. The point of
// having one in this suite is only that signing a pseudonymizing plan is
// refused WITHOUT it, so what it does with the value does not matter.
type memVault struct {
	n    int
	back map[string]string
}

func (v *memVault) Put(_ context.Context, _ string, _ connector.PiiKind, original string, _ connector.KeyProvider) (string, error) {
	if v.back == nil {
		v.back = map[string]string{}
	}
	v.n++
	tok := fmt.Sprintf("tok_%d", v.n)
	v.back[tok] = original
	return tok, nil
}

func (v *memVault) Resolve(_ context.Context, _ string, tokens []string, _ connector.KeyProvider) (map[string]string, error) {
	out := map[string]string{}
	for _, t := range tokens {
		out[t] = v.back[t]
	}
	return out, nil
}

func (v *memVault) Close() error { return nil }

// ------------------------------------------------------------- the fixtures

const (
	dsnLive    = "postgres://app:s3cr3t@db.internal:5432/sales?sslmode=require"
	dsnRotated = "postgres://app:newp4ss@db.internal:5432/sales?sslmode=require"
)

// schemaV1 is the database as the plan is proposed against it: a customers
// table with a national id (dropped), an email (masked), a free column
// (redacted by default-deny) and a key; an orders table whose foreign key
// points at it; and an events table with no primary key at all.
func schemaV1() []importflow.Schema {
	return []importflow.Schema{
		{
			Table: "customers",
			Columns: []importflow.Column{
				{Name: "id", Type: "integer"},
				{Name: "email", Type: "text"},
				{Name: "id_card", Type: "text"},
				{Name: "city", Type: "text"},
				{Name: "status", Type: "text"},
			},
			Sample: []importflow.Record{{Table: "customers", Values: map[string]string{
				"id": "7", "email": "ada@example.com", "id_card": "11010119900307123X",
				"city": "Cambridge", "status": "active",
			}, Nulls: map[string]bool{}}},
		},
		{
			Table: "orders",
			Columns: []importflow.Column{
				{Name: "id", Type: "integer"},
				{Name: "customer_id", Type: "integer"},
				{Name: "status", Type: "text"},
			},
			Sample: []importflow.Record{{Table: "orders", Values: map[string]string{
				"id": "1", "customer_id": "7", "status": "shipped",
			}, Nulls: map[string]bool{}}},
		},
		{
			Table: "events",
			Columns: []importflow.Column{
				{Name: "kind", Type: "text"},
				{Name: "status", Type: "text"},
			},
			Sample: []importflow.Record{{Table: "events", Values: map[string]string{
				"kind": "login", "status": "ok",
			}, Nulls: map[string]bool{}}},
		},
	}
}

// schemaV2 is the same database a week later: customers gained loyalty_tier
// and lost city. Neither is in the signed plan.
func schemaV2() []importflow.Schema {
	out := schemaV1()
	out[0].Columns = []importflow.Column{
		{Name: "id", Type: "integer"},
		{Name: "email", Type: "text"},
		{Name: "id_card", Type: "text"},
		{Name: "loyalty_tier", Type: "text"},
		{Name: "status", Type: "text"},
	}
	out[0].Sample = []importflow.Record{{Table: "customers", Values: map[string]string{
		"id": "7", "email": "ada@example.com", "id_card": "11010119900307123X",
		"loyalty_tier": "gold", "status": "active",
	}, Nulls: map[string]bool{}}}
	return out
}

func rowsV1() []importflow.Record {
	return []importflow.Record{
		{Table: "customers", Row: 0, Values: map[string]string{
			"id": "7", "email": "ada@example.com", "id_card": "11010119900307123X",
			"city": "Cambridge", "status": "active",
		}, Nulls: map[string]bool{}},
		{Table: "orders", Row: 0, Values: map[string]string{
			"id": "1", "customer_id": "7", "status": "shipped",
		}, Nulls: map[string]bool{}},
		{Table: "events", Row: 0, Values: map[string]string{
			"kind": "login", "status": "ok",
		}, Nulls: map[string]bool{}},
	}
}

func rowsV2() []importflow.Record {
	return []importflow.Record{
		{Table: "customers", Row: 0, Values: map[string]string{
			"id": "7", "email": "ada@example.com", "id_card": "11010119900307123X",
			"loyalty_tier": "gold", "status": "active",
		}, Nulls: map[string]bool{}},
	}
}

func liveV1() *fakeLive {
	return &fakeLive{
		schemas: schemaV1(),
		rows:    rowsV1(),
		keys: map[string][]string{
			"customers": {"id"},
			"orders":    {"id"},
			"events":    nil,
		},
		rels: map[string][]Relation{
			"orders": {{
				Table: "orders", Columns: []string{"customer_id"},
				Predicate: "customer", Target: "customers", TargetKeys: []string{"id"},
			}},
		},
	}
}

// ---------------------------------------------------------------- the harness

type harness struct {
	store    *Store
	opener   *fakeOpener
	importer *fakeImporter
	ledger   *fakeLedger
	clock    *time.Time
}

func newHarness(t *testing.T, opts ...Option) *harness {
	t.Helper()
	cfg := cortexdb.DefaultConfig(filepath.Join(t.TempDir(), "brain.db"))
	cfg.Dimensions = 4
	db, err := cortexdb.Open(cfg)
	if err != nil {
		t.Fatalf("open brain: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	h := &harness{
		opener:   &fakeOpener{live: liveV1()},
		importer: &fakeImporter{},
		ledger:   &fakeLedger{},
		clock:    &at,
	}
	all := append([]Option{
		WithOpener(h.opener),
		WithImporter(h.importer),
		WithLedger(h.ledger),
		WithClock(func() time.Time {
			*h.clock = h.clock.Add(time.Second)
			return *h.clock
		}),
	}, opts...)
	s, err := New(db, all...)
	if err != nil {
		t.Fatalf("livedb.New: %v", err)
	}
	h.store = s
	return h
}

// propose is the shape almost every test starts from.
func (h *harness) propose(t *testing.T) Plan {
	t.Helper()
	p, err := h.store.Propose(context.Background(), Source{Driver: "postgres", DSN: dsnLive},
		ProposeOptions{By: "operator", Note: "the sales database"})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	return p
}

// keepable makes the plan usable end to end: the foreign key and the id are
// kept, so the derived mapping has something to point with. Default-deny
// redacts customer_id, which is correct and is exactly the kind of thing a
// human amends.
func (h *harness) keepFK(t *testing.T, id string) Plan {
	t.Helper()
	p, err := h.store.Amend(context.Background(), id, []Change{
		{Table: "orders", Column: "customer_id", Action: connector.ActionKeep, Reason: "it is a key, not a fact about a person"},
	}, "liliang")
	if err != nil {
		t.Fatalf("amend: %v", err)
	}
	return p
}

func (h *harness) sign(t *testing.T, p Plan) Plan {
	t.Helper()
	signed, err := h.store.Sign(context.Background(), p.ID, p.Hash, "liliang", "reviewed")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func treatmentOf(t *testing.T, p Plan, table, column string) Treatment {
	t.Helper()
	i := indexOf(p.Columns, table, column)
	if i < 0 {
		t.Fatalf("plan does not cover %s.%s", table, column)
	}
	return p.Columns[i]
}

// ------------------------------------------------------------------- the DSN

func TestTheCredentialReachesNeitherTheRowNorTheReply(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	p := h.propose(t)

	if p.Source.DSN != "" {
		t.Fatalf("the returned plan carries a DSN: %q", p.Source.DSN)
	}
	if strings.Contains(p.Source.Redacted, "s3cr3t") {
		t.Fatalf("the redaction still holds the password: %q", p.Source.Redacted)
	}
	if !strings.Contains(p.Source.Redacted, "db.internal") {
		t.Fatalf("the redaction lost the hostname, which is the half a reader needs: %q", p.Source.Redacted)
	}
	if got := rowText(t, h, p.ID); strings.Contains(got, "s3cr3t") {
		t.Fatalf("the stored row holds the password: %s", got)
	}

	// A DSN nothing here can parse is redacted MORE, not less: what comes back
	// says only that it could not be read.
	weird, err := h.store.Propose(ctx, Source{Driver: "postgres", DSN: "!!weird!! password=hunter2 &&&"},
		ProposeOptions{By: "operator"})
	if err != nil {
		t.Fatalf("propose an unparsable dsn: %v", err)
	}
	if strings.Contains(weird.Source.Redacted, "hunter2") || strings.Contains(weird.Source.Redacted, "weird") {
		t.Fatalf("an unparsable dsn leaked through the redaction: %q", weird.Source.Redacted)
	}
	if !strings.Contains(weird.Source.Redacted, "unparsed") {
		t.Fatalf("an unparsable dsn should say so: %q", weird.Source.Redacted)
	}
	if got := rowText(t, h, weird.ID); strings.Contains(got, "hunter2") {
		t.Fatalf("the stored row holds an unparsable dsn's password: %s", got)
	}
}

// rowText is every textual column of a plan row, joined — so a test asking
// "is the password anywhere in here" does not have to name the column it
// would have leaked through.
func rowText(t *testing.T, h *harness, id string) string {
	t.Helper()
	const q = `SELECT id, source_key, driver, redacted, db_schema, tables, treatments,
		hash, state, counts, created_by, signed_by, sign_act, supersedes, note
		FROM athanor_livedb_plans WHERE id = ?`
	cols := make([]string, 15)
	dest := make([]any, len(cols))
	for i := range cols {
		dest[i] = &cols[i]
	}
	if err := h.store.impl.queryRow(context.Background(), q, id).Scan(dest...); err != nil {
		t.Fatalf("read the stored row: %v", err)
	}
	return strings.Join(cols, "\x00")
}

func TestTheSourceKeySurvivesARotationAndNothingElse(t *testing.T) {
	base := Source{Driver: "postgres", DSN: dsnLive, Tables: []string{"customers", "orders"}}
	rotated := base
	rotated.DSN = dsnRotated

	if base.Key() != rotated.Key() {
		t.Fatalf("a rotated password moved the source key: %s vs %s", base.Key(), rotated.Key())
	}
	// The same plan read back out of a table has only the redaction, and it
	// has to key the same or Current would never match a run.
	red := Source{Driver: "postgres", Redacted: "postgres://app@db.internal:5432/sales?sslmode=require", Tables: base.Tables}
	if red.Key() != base.Key() {
		t.Fatalf("the redacted source keys differently from the live one: %s vs %s", red.Key(), base.Key())
	}

	for _, c := range []struct {
		name string
		src  Source
	}{
		{"another database", Source{Driver: "postgres", DSN: "postgres://app:s3cr3t@db.internal:5432/other", Tables: base.Tables}},
		{"another host", Source{Driver: "postgres", DSN: "postgres://app:s3cr3t@elsewhere:5432/sales", Tables: base.Tables}},
		{"another schema", Source{Driver: "postgres", DSN: dsnLive, Schema: "archive", Tables: base.Tables}},
		{"another tableset", Source{Driver: "postgres", DSN: dsnLive, Tables: []string{"customers"}}},
		{"another driver", Source{Driver: "mysql", DSN: "app:s3cr3t@tcp(db.internal:3306)/sales", Tables: base.Tables}},
	} {
		if c.src.Key() == base.Key() {
			t.Errorf("%s keys the same as the original", c.name)
		}
	}

	// Order is not identity: an allow-list written the other way round is the
	// same allow-list.
	shuffled := base
	shuffled.Tables = []string{"orders", "customers"}
	if shuffled.Key() != base.Key() {
		t.Fatalf("the table order changed the key")
	}
}

// ------------------------------------------------------------------ the hash

func TestTheHashIsTheThingSigned(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t)

	if planHash(p) != p.Hash || planHash(p) != planHash(p) {
		t.Fatalf("the hash is not a function of the plan")
	}
	amended := h.keepFK(t, p.ID)
	if amended.Hash == p.Hash {
		t.Fatalf("changing an action did not change the hash")
	}
	if amended.Counts.Passed != p.Counts.Passed+1 || amended.Counts.Redacted != p.Counts.Redacted-1 {
		t.Fatalf("the counts did not follow the amendment: %+v -> %+v", p.Counts, amended.Counts)
	}
	if got := treatmentOf(t, amended, "orders", "customer_id"); got.By != "liliang" {
		t.Fatalf("an amended treatment should name who amended it, got %q", got.By)
	}
	if got := treatmentOf(t, amended, "customers", "email"); got.By != "rule" {
		t.Fatalf("an untouched treatment should still name the classifier, got %q", got.By)
	}
}

func TestAnAmendmentNamingNothingIsRefused(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t)
	_, err := h.store.Amend(context.Background(), p.ID, []Change{
		{Table: "customers", Column: "no_such_column", Action: connector.ActionDrop},
	}, "liliang")
	if err == nil {
		t.Fatalf("an amendment naming a column the plan does not hold must be an error, not a no-op")
	}
	if !strings.Contains(err.Error(), "no_such_column") {
		t.Fatalf("the error should name the column: %v", err)
	}
}

// -------------------------------------------------------------- the proposal

func TestWhatTheReviewerIsShown(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t)

	email := treatmentOf(t, p, "customers", "email")
	if email.Kind != connector.PiiEmail || email.Action != connector.ActionMask {
		t.Fatalf("an email column classified as %+v", email)
	}
	if email.Sample == "ada@example.com" {
		t.Fatalf("a personal column's sample is shown raw: %q", email.Sample)
	}
	if !strings.Contains(email.Sample, "@example.com") {
		t.Fatalf("a masked email should still read as an email: %q", email.Sample)
	}

	city := treatmentOf(t, p, "customers", "city")
	if city.Kind != connector.PiiNone {
		t.Fatalf("city should not classify as personal, got %q", city.Kind)
	}
	if city.Sample != "Cambridge" {
		t.Fatalf("a column the classifier calls impersonal must be shown as it is, got %q", city.Sample)
	}
	if city.Action != connector.ActionRedact || city.Enters != "[REDACTED]" {
		t.Fatalf("default-deny should redact city, got %q -> %q", city.Action, city.Enters)
	}

	idCard := treatmentOf(t, p, "customers", "id_card")
	if idCard.Action != connector.ActionDrop || idCard.Enters != "" {
		t.Fatalf("a national id should be dropped and enter nothing, got %q -> %q", idCard.Action, idCard.Enters)
	}

	// An empty allow-list is resolved, so the plan names what it covers.
	if got := strings.Join(p.Source.Tables, ","); got != "customers,events,orders" {
		t.Fatalf("the plan should name the real tables, got %q", got)
	}
	if p.State != Draft || p.SignedBy != "" {
		t.Fatalf("a proposal is a draft nobody signed: %+v", p)
	}
	if p.Counts.Columns != 10 || p.Counts.Personal != 2 {
		t.Fatalf("counts: %+v", p.Counts)
	}
}

// -------------------------------------------------------------- the signature

func TestSigningRefusesWhatItShould(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	p := h.propose(t)

	if _, err := h.store.Sign(ctx, p.ID, "not-the-hash", "liliang", ""); !errors.Is(err, ErrStaleHash) {
		t.Fatalf("a stale hash should be ErrStaleHash, got %v", err)
	}
	if _, err := h.store.Sign(ctx, "plan_nothing", p.Hash, "liliang", ""); !errors.Is(err, ErrNoPlan) {
		t.Fatalf("an unknown plan should be ErrNoPlan, got %v", err)
	}
	signed := h.sign(t, p)
	if signed.State != Signed || signed.SignedBy != "liliang" || signed.SignedAt.IsZero() {
		t.Fatalf("signed as %+v", signed)
	}
	if _, err := h.store.Sign(ctx, p.ID, signed.Hash, "liliang", ""); !errors.Is(err, ErrNotDraft) {
		t.Fatalf("signing twice should be ErrNotDraft, got %v", err)
	}
	if _, err := h.store.Amend(ctx, p.ID, []Change{
		{Table: "customers", Column: "city", Action: connector.ActionDrop},
	}, "liliang"); !errors.Is(err, ErrNotDraft) {
		t.Fatalf("amending a signed plan should be ErrNotDraft, got %v", err)
	}
}

func TestAReversibleTreatmentNeedsSomewhereToPutTheOriginal(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	p := h.propose(t)
	// Pseudonymize is the only reversible action, and nothing in the fixture
	// classifies as a name — so the human asks for it, which is the case that
	// matters: the refusal has to survive an override.
	p, err := h.store.Amend(ctx, p.ID, []Change{
		{Table: "customers", Column: "city", Action: connector.ActionPseudonymize},
	}, "liliang")
	if err != nil {
		t.Fatalf("amend: %v", err)
	}
	if p.Counts.Reversible != 1 {
		t.Fatalf("counts should have noticed the reversible column: %+v", p.Counts)
	}
	if _, err := h.store.Sign(ctx, p.ID, p.Hash, "liliang", ""); !errors.Is(err, ErrNoVault) {
		t.Fatalf("signing a pseudonymizing plan with no vault should be ErrNoVault, got %v", err)
	}

	// With a vault it signs, and the refusal was about the vault and nothing
	// else.
	withVault := newHarness(t, WithVault(&memVault{}, connector.StaticKeyProvider(make([]byte, 32)), "acme"))
	q := withVault.propose(t)
	q, err = withVault.store.Amend(ctx, q.ID, []Change{
		{Table: "customers", Column: "city", Action: connector.ActionPseudonymize},
	}, "liliang")
	if err != nil {
		t.Fatalf("amend: %v", err)
	}
	if _, err := withVault.store.Sign(ctx, q.ID, q.Hash, "liliang", ""); err != nil {
		t.Fatalf("sign with a vault: %v", err)
	}
}

func TestASignatureSupersedesAndOnlyOneStands(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	first := h.sign(t, h.propose(t))
	second := h.sign(t, h.propose(t))

	if second.Supersedes != first.ID {
		t.Fatalf("the second signature should name the first: %q", second.Supersedes)
	}
	back, err := h.store.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if back.State != Superseded {
		t.Fatalf("the first plan should be superseded, is %q", back.State)
	}
	cur, err := h.store.Current(ctx, second.SourceKey)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if cur.ID != second.ID {
		t.Fatalf("current is %s, want %s", cur.ID, second.ID)
	}
	// A plan kept after being replaced, because a run that happened under it
	// is only explicable if it still exists.
	all, err := h.store.List(ctx, ListQuery{SourceKey: second.SourceKey})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("both plans should still be there, got %d", len(all))
	}

	// And the rule is in the database, not only in the transaction that means
	// it: a second path that forgot to supersede is refused outright.
	_, err = h.store.impl.exec(ctx,
		`UPDATE athanor_livedb_plans SET state = ? WHERE id = ?`, string(Signed), first.ID)
	if err == nil {
		t.Fatalf("the partial unique index let a second plan be signed for one source")
	}
}

// ---------------------------------------------------------------- the run

func TestARunNeedsASignatureAndTheRightDatabase(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	p := h.propose(t)

	if _, err := h.store.Run(ctx, RunRequest{Plan: p.ID, DSN: dsnLive}, "job"); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("running a draft should be ErrUnsigned, got %v", err)
	}
	if _, err := h.store.Run(ctx, RunRequest{Plan: "plan_nothing", DSN: dsnLive}, "job"); !errors.Is(err, ErrNoPlan) {
		t.Fatalf("running an unknown plan should be ErrNoPlan, got %v", err)
	}
	signed := h.sign(t, p)

	if _, err := h.store.Run(ctx, RunRequest{
		Plan: signed.ID, DSN: "postgres://app:s3cr3t@db.internal:5432/staging",
	}, "job"); !errors.Is(err, ErrWrongSource) {
		t.Fatalf("a credential for another database should be ErrWrongSource, got %v", err)
	}
	// The rotated password is the same database and is accepted.
	if _, err := h.store.Run(ctx, RunRequest{Plan: signed.ID, DSN: dsnRotated}, "job"); err != nil {
		t.Fatalf("a rotated credential for the same database should run: %v", err)
	}
	if h.importer.runs != 1 {
		t.Fatalf("the importer ran %d times", h.importer.runs)
	}
}

func TestDriftIsReportedByNameAndTheColumnNeverArrives(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	signed := h.sign(t, h.keepFK(t, h.propose(t).ID))

	// A week passes. customers gained loyalty_tier and lost city; neither is
	// in the plan somebody signed.
	h.opener.live = &fakeLive{
		schemas: schemaV2(), rows: rowsV2(),
		keys: map[string][]string{"customers": {"id"}},
	}

	rep, err := h.store.Run(ctx, RunRequest{Plan: signed.ID, DSN: dsnLive}, "job")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Join(rep.Drift, ",") != "customers.loyalty_tier" {
		t.Fatalf("drift = %v", rep.Drift)
	}
	if strings.Join(rep.Gone, ",") != "customers.city" {
		t.Fatalf("gone = %v", rep.Gone)
	}

	// The report is the operator's half. This is the database's: the drifted
	// column is not in what the importer was handed, and neither is the one
	// the plan drops.
	got := h.importer.columns("customers")
	if got["loyalty_tier"] {
		t.Fatalf("the drifted column reached the import: %v", got)
	}
	if got["id_card"] {
		t.Fatalf("a dropped column reached the import: %v", got)
	}
	if !got["email"] || !got["id"] {
		t.Fatalf("the kept columns did not reach the import: %v", got)
	}
	for _, r := range h.importer.got {
		if r.Values["email"] == "ada@example.com" {
			t.Fatalf("a masked column arrived raw: %+v", r.Values)
		}
	}
}

func TestTheDerivedMappingNamesNothingThePlanRemoved(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	signed := h.sign(t, h.keepFK(t, h.propose(t).ID))
	if _, err := h.store.Run(ctx, RunRequest{Plan: signed.ID, DSN: dsnLive}, "job"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.importer.plans) != 1 {
		t.Fatalf("the importer got %d plans", len(h.importer.plans))
	}
	mapping := h.importer.plans[0]

	customers := mapping.Tables["customers"]
	if customers.RAG == nil {
		t.Fatalf("customers got no RAG plan")
	}
	if customers.RAG.IDColumn != "id" {
		t.Fatalf("the primary key should address the chunk, got %q", customers.RAG.IDColumn)
	}
	if customers.RAG.Namespace != signed.ID {
		t.Fatalf("an empty namespace should take the plan id, got %q", customers.RAG.Namespace)
	}
	blob, _ := json.Marshal(customers)
	if strings.Contains(string(blob), "id_card") {
		t.Fatalf("a dropped column is named in the mapping: %s", blob)
	}
	if !strings.Contains(customers.RAG.ContentTmpl, "{email}") {
		t.Fatalf("the content template lost a kept column: %q", customers.RAG.ContentTmpl)
	}
	if customers.KG == nil || len(customers.KG.Entities) != 1 || customers.KG.Entities[0].IDTmpl != "customers:{id}" {
		t.Fatalf("customers entity: %+v", customers.KG)
	}

	// A table with no primary key still gets a chunk — importflow synthesizes
	// "table:row" — but no entity, because a node with no stable id is a node
	// that duplicates on every re-run.
	events := mapping.Tables["events"]
	if events.RAG == nil {
		t.Fatalf("a keyless table should still be retrievable")
	}
	if events.RAG.IDColumn != "" {
		t.Fatalf("a keyless table has no id column, got %q", events.RAG.IDColumn)
	}
	if events.KG != nil {
		t.Fatalf("a keyless table must not become an entity: %+v", events.KG)
	}

	// A foreign key becomes an edge, and the far end is emitted as a second
	// entity keyed on the FK value so the two tables mint one node.
	orders := mapping.Tables["orders"]
	if orders.KG == nil || len(orders.KG.Relations) != 1 {
		t.Fatalf("orders relations: %+v", orders.KG)
	}
	rel := orders.KG.Relations[0]
	if rel.Subject != "orders" || rel.Predicate != "customer" {
		t.Fatalf("relation: %+v", rel)
	}
	var target *importflow.EntityMap
	for i := range orders.KG.Entities {
		if orders.KG.Entities[i].Ref == rel.Object {
			target = &orders.KG.Entities[i]
		}
	}
	if target == nil {
		t.Fatalf("the edge points at a ref no entity declares: %+v", orders.KG)
	}
	if target.Type != "customers" || target.IDTmpl != "customers:{customer_id}" {
		t.Fatalf("the far end does not mint the customers node: %+v", target)
	}
}

func TestADryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	signed := h.sign(t, h.propose(t))

	rep, err := h.store.Run(ctx, RunRequest{Plan: signed.ID, DSN: dsnLive, DryRun: true}, "job")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if h.importer.runs != 0 {
		t.Fatalf("a dry run reached the importer")
	}
	if !rep.DryRun || rep.RowsRead != 3 || rep.Chunks != 0 {
		t.Fatalf("dry run report: %+v", rep)
	}
	runs, err := h.store.Runs(ctx, signed.ID, 0)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 1 || !runs[0].DryRun || runs[0].RowsRead != 3 {
		t.Fatalf("the dry run should still be recorded: %+v", runs)
	}
	if runs[0].ID != rep.ID {
		t.Fatalf("the recorded run is not the one reported")
	}
	// Two opens — the proposal and the run — and both closed behind them.
	if got, want := h.opener.live.closed.Load(), h.opener.opens.Load(); got != want {
		t.Fatalf("opened %d sources and closed %d", want, got)
	}
}

// ------------------------------------------------------------------ the chain

func TestTheRunRestsOnTheSignature(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	signed := h.sign(t, h.propose(t))
	if _, err := h.store.Run(ctx, RunRequest{Plan: signed.ID, DSN: dsnLive}, "job-7"); err != nil {
		t.Fatalf("run: %v", err)
	}

	signs := h.ledger.of(ActSign)
	if len(signs) != 1 {
		t.Fatalf("the ledger got %d signatures", len(signs))
	}
	sign := signs[0]
	if sign.Actor != "liliang" || sign.Subject != signed.ID {
		t.Fatalf("sign act: %+v", sign)
	}
	if sign.Detail["hash"] != signed.Hash || sign.Detail["source_key"] != signed.SourceKey {
		t.Fatalf("the sign act should carry what was signed: %+v", sign.Detail)
	}
	if red, _ := sign.Detail["redacted"].(string); strings.Contains(red, "s3cr3t") {
		t.Fatalf("the ledger got the password")
	}
	if _, ok := sign.Detail["counts"].(Counts); !ok {
		t.Fatalf("the sign act should carry the counts, got %T", sign.Detail["counts"])
	}

	runs := h.ledger.of(ActRun)
	if len(runs) != 1 {
		t.Fatalf("the ledger got %d runs", len(runs))
	}
	// A premise is a SUBJECT, not an act id: this package does not know how
	// the ledger numbers its entries, and the subject the signature was about
	// is the plan. The ledger turns that into the entry the signature wrote.
	if got := runs[0].Premises; len(got) != 1 || got[0] != signed.ID {
		t.Fatalf("the run should rest on the plan %s, got %v", signed.ID, got)
	}
	if runs[0].Actor != "job-7" {
		t.Fatalf("the run's actor is %q", runs[0].Actor)
	}
	if len(h.ledger.of(ActPropose)) != 1 {
		t.Fatalf("the proposal should be recorded too")
	}
}

func TestAStoreWithNoLedgerWorks(t *testing.T) {
	ctx := context.Background()
	cfg := cortexdb.DefaultConfig(filepath.Join(t.TempDir(), "brain.db"))
	cfg.Dimensions = 4
	db, err := cortexdb.Open(cfg)
	if err != nil {
		t.Fatalf("open brain: %v", err)
	}
	defer func() { _ = db.Close() }()

	s, err := New(db, WithOpener(&fakeOpener{live: liveV1()}), WithImporter(&fakeImporter{}))
	if err != nil {
		t.Fatalf("livedb.New: %v", err)
	}
	p, err := s.Propose(ctx, Source{Driver: "postgres", DSN: dsnLive}, ProposeOptions{By: "operator"})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	signed, err := s.Sign(ctx, p.ID, p.Hash, "liliang", "")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := s.Run(ctx, RunRequest{Plan: signed.ID, DSN: dsnLive}, "job"); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// -------------------------------------------------------------- checkpoints

func TestCheckpointsRoundTrip(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	cp := h.store.Checkpoints()

	if _, found, err := cp.Load(ctx, "src_nothing"); err != nil || found {
		t.Fatalf("an unknown key should be (zero, false, nil), got found=%v err=%v", found, err)
	}
	if err := cp.Save(ctx, "src_a", connector.Checkpoint{Cursor: `{"customers":"7"}`}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := cp.Save(ctx, "src_a", connector.Checkpoint{Position: "0/16B3748"}); err != nil {
		t.Fatalf("save again: %v", err)
	}
	got, found, err := cp.Load(ctx, "src_a")
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	if got.Position != "0/16B3748" || got.Cursor != "" {
		t.Fatalf("the second save should have replaced the first: %+v", got)
	}
}

// ------------------------------------------------------------------ masking

func TestMaskingRendersSignedOnlyWhenSigned(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t)

	if p.Masking().IsSigned() {
		t.Fatalf("a draft must render unsigned")
	}
	if _, err := connector.NewDesensitizer(p.Masking(), connector.DesensitizerOptions{}); err == nil {
		t.Fatalf("the connector should refuse a draft's masking plan")
	}
	signed := h.sign(t, p)
	mp := signed.Masking()
	if !mp.IsSigned() || mp.SignedBy != "liliang" {
		t.Fatalf("a signed plan should render signed: %+v", mp)
	}
	if len(mp.Columns) != len(signed.Columns) {
		t.Fatalf("the rendering lost columns: %d of %d", len(mp.Columns), len(signed.Columns))
	}
	rule, ok := mp.RuleFor("customers", "id_card")
	if !ok || rule.Action != connector.ActionDrop {
		t.Fatalf("the rendering lost a decision: %+v", rule)
	}
	if _, err := connector.NewDesensitizer(mp, connector.DesensitizerOptions{}); err != nil {
		t.Fatalf("the connector should accept a signed plan: %v", err)
	}
}

// ------------------------------------------------------------------- follow

func TestATableWithNoKeyCannotBeFollowed(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	signed := h.sign(t, h.propose(t))

	_, err := h.store.Run(ctx, RunRequest{Plan: signed.ID, DSN: dsnLive, Follow: true}, "job")
	if err == nil {
		t.Fatalf("following a keyless table should be refused")
	}
	if !strings.Contains(err.Error(), "events") {
		t.Fatalf("the refusal should name the table it is about: %v", err)
	}
}

// ----------------------------------------------------------- the redaction

func TestRedactionByShape(t *testing.T) {
	for _, c := range []struct {
		name, driver, dsn string
		wantHost          string
		wantDB            string
		absent            []string
	}{
		{"url", "postgres", "postgres://app:s3cr3t@db.internal:5432/sales?sslmode=require", "db.internal", "sales", []string{"s3cr3t"}},
		{"url with a password parameter", "postgres", "postgres://app@db.internal/sales?password=s3cr3t", "db.internal", "sales", []string{"s3cr3t"}},
		{"keyword and value", "postgres", "host=db.internal port=5432 dbname=sales user=app password=s3cr3t", "db.internal", "sales", []string{"s3cr3t"}},
		{"mysql", "mysql", "app:s3cr3t@tcp(db.internal:3306)/sales?parseTime=true", "db.internal", "sales", []string{"s3cr3t"}},
		{"mysql with no credential", "mysql", "tcp(db.internal:3306)/sales", "db.internal", "sales", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			facts, red := redactDSN(c.driver, c.dsn)
			if facts.Host != c.wantHost || facts.Database != c.wantDB {
				t.Fatalf("facts = %+v", facts)
			}
			for _, s := range c.absent {
				if strings.Contains(red, s) {
					t.Fatalf("%q survived the redaction: %q", s, red)
				}
			}
			// The redaction re-reads to the same facts, which is what lets a
			// stored plan key the same as the run that supplies a credential.
			again, _ := redactDSN(c.driver, red)
			if again.Host != facts.Host || again.Database != facts.Database || again.Opaque != facts.Opaque {
				t.Fatalf("the redaction does not round trip: %+v vs %+v", again, facts)
			}
		})
	}

	t.Run("unparsable", func(t *testing.T) {
		facts, red := redactDSN("postgres", "!!weird!! password=hunter2 &&&")
		if facts.Opaque == "" {
			t.Fatalf("an unparsable dsn still needs an identity")
		}
		if strings.Contains(red, "hunter2") {
			t.Fatalf("redaction: %q", red)
		}
		again, _ := redactDSN("postgres", red)
		if again.Opaque != facts.Opaque {
			t.Fatalf("the opaque redaction does not round trip: %q vs %q", again.Opaque, facts.Opaque)
		}
		// Two unparsable strings are still two sources, and a rotated
		// password inside one of them is not a third.
		other, _ := redactDSN("postgres", "!!weirder!! password=hunter2 &&&")
		if other.Opaque == facts.Opaque {
			t.Fatalf("two unparsable dsns collapsed onto one identity")
		}
		rotated, _ := redactDSN("postgres", "!!weird!! password=different &&&")
		if rotated.Opaque != facts.Opaque {
			t.Fatalf("a rotated password moved an unparsable dsn's identity")
		}
	})
}

func TestDriftOfIsSortedAndByName(t *testing.T) {
	p := Plan{Columns: []Treatment{
		{Table: "t", Column: "b"}, {Table: "t", Column: "a"}, {Table: "t", Column: "z"},
	}}
	drift, gone := driftOf(p, []importflow.Schema{{Table: "t", Columns: []importflow.Column{
		{Name: "a"}, {Name: "y"}, {Name: "x"},
	}}})
	if strings.Join(drift, ",") != "t.x,t.y" {
		t.Fatalf("drift = %v", drift)
	}
	if strings.Join(gone, ",") != "t.b,t.z" {
		t.Fatalf("gone = %v", gone)
	}
	if !sort.StringsAreSorted(drift) || !sort.StringsAreSorted(gone) {
		t.Fatalf("both lists are sorted so two reports are comparable")
	}
}

// A free-text scan is part of what was signed, so it has to survive into the
// rendering an auditor reads.
//
// It used to live in a column beside the plan, which worked — the desensitizer
// got it — and was wrong in the one way this package cannot afford: Masking()
// is what an auditor reads back, and it rendered a plan with no scan rules on
// a source that was being scanned. A review screen that shows less than what
// ran is the failure the whole package exists to prevent.
func TestAFreeTextScanIsOnThePlanAndInItsRendering(t *testing.T) {
	plain := newHarness(t)
	quiet, err := plain.store.Propose(context.Background(),
		Source{Driver: "postgres", DSN: dsnLive}, ProposeOptions{By: "operator"})
	if err != nil {
		t.Fatalf("propose without scanning: %v", err)
	}

	h := newHarness(t)
	scanned, err := h.store.Propose(context.Background(),
		Source{Driver: "postgres", DSN: dsnLive}, ProposeOptions{By: "operator", ScanText: true})
	if err != nil {
		t.Fatalf("propose with scanning: %v", err)
	}

	var marked []string
	for _, tr := range scanned.Columns {
		if tr.Scan {
			marked = append(marked, tr.Table+"."+tr.Column)
		}
	}
	if len(marked) == 0 {
		t.Fatalf("ScanText marked no column: %+v", scanned.Columns)
	}

	// The rendering the desensitizer is built from, and the one an auditor
	// reads, are the same object — so the rules are in it.
	mp := scanned.Masking()
	if len(mp.TextScan) != len(marked) {
		t.Fatalf("the rendering carries %d scan rules for %d marked columns: %+v",
			len(mp.TextScan), len(marked), mp.TextScan)
	}
	for _, name := range marked {
		table, column, _ := strings.Cut(name, ".")
		if !mp.TextScanFor(table, column) {
			t.Errorf("%s is marked on the plan and absent from its rendering", name)
		}
	}

	// And it changed what leaves the database, so it changed what was signed.
	if scanned.Hash == quiet.Hash {
		t.Errorf("scanning did not move the hash, so a plan could gain it after a signature")
	}
}
