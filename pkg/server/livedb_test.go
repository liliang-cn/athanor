package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/liliang-cn/athanor/pkg/livedb"
	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// The credential these tests hand the door, and the one substring that must
// never come back out of it. Every assertion below searches the raw response
// bytes for it rather than a decoded field, because a leak that mattered would
// be one nobody thought to decode.
const (
	livedbPassword_ = "hunter2-not-in-any-answer"
	livedbDSN       = "postgres://importer:" + livedbPassword_ + "@db.internal:5432/crm?sslmode=disable"
)

func proposeBody(dsn string) string {
	return `{"source":{"driver":"postgres","dsn":"` + dsn + `","tables":["customers"]}}`
}

func runBody(dsn string) string {
	return `{"plan":"plan-1","dsn":"` + dsn + `"}`
}

// livedbFake is the store the routes are tested against.
//
// The handlers depend on the livedbStore interface rather than the concrete
// store — pkg/server has no business knowing which one it holds — and that is
// what lets each of the store's documented refusals be produced on demand
// here. Reaching the real store would need a live Postgres to refuse
// anything, which would make this file a test of the network.
type livedbFake struct {
	mu     sync.Mutex
	plan   livedb.Plan
	report livedb.RunReport
	err    error

	calls  []string
	actor  string // the caller key the handler parked in the context
	source livedb.Source
	opts   livedb.ProposeOptions
	run    livedb.RunRequest
	runBy  string
	signBy string
	hash   string
	note   string
	id     string
	query  livedb.ListQuery
}

func (f *livedbFake) note_(ctx context.Context, call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	f.actor = callerKey(ctx).ID
}

func (f *livedbFake) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *livedbFake) Propose(ctx context.Context, src livedb.Source, opts livedb.ProposeOptions) (livedb.Plan, error) {
	f.note_(ctx, "propose")
	f.mu.Lock()
	f.source, f.opts = src, opts
	f.mu.Unlock()
	return f.plan, f.err
}

func (f *livedbFake) Amend(ctx context.Context, id string, changes []livedb.Change, by string) (livedb.Plan, error) {
	f.note_(ctx, "amend")
	f.mu.Lock()
	f.id, f.signBy = id, by
	f.mu.Unlock()
	return f.plan, f.err
}

func (f *livedbFake) Sign(ctx context.Context, id, hash, by, note string) (livedb.Plan, error) {
	f.note_(ctx, "sign")
	f.mu.Lock()
	f.id, f.hash, f.signBy, f.note = id, hash, by, note
	f.mu.Unlock()
	return f.plan, f.err
}

func (f *livedbFake) Get(ctx context.Context, id string) (livedb.Plan, error) {
	f.note_(ctx, "get")
	f.mu.Lock()
	f.id = id
	f.mu.Unlock()
	return f.plan, f.err
}

func (f *livedbFake) List(ctx context.Context, q livedb.ListQuery) ([]livedb.Plan, error) {
	f.note_(ctx, "list")
	f.mu.Lock()
	f.query = q
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return []livedb.Plan{f.plan}, nil
}

func (f *livedbFake) Current(ctx context.Context, sourceKey string) (livedb.Plan, error) {
	f.note_(ctx, "current")
	f.mu.Lock()
	f.id = sourceKey
	f.mu.Unlock()
	return f.plan, f.err
}

func (f *livedbFake) Run(ctx context.Context, req livedb.RunRequest, actor string) (livedb.RunReport, error) {
	f.note_(ctx, "run")
	f.mu.Lock()
	f.run, f.runBy = req, actor
	f.mu.Unlock()
	return f.report, f.err
}

func (f *livedbFake) Runs(ctx context.Context, planID string, limit int) ([]livedb.RunReport, error) {
	f.note_(ctx, "runs")
	f.mu.Lock()
	f.id = planID
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return []livedb.RunReport{f.report}, nil
}

// signedPlan is what a happy store hands back. Its Source carries the DSN,
// which the real store never does — the point being that the door clears it
// anyway rather than trusting the promise.
func signedPlan() livedb.Plan {
	return livedb.Plan{
		ID:        "plan-1",
		SourceKey: "crm@db.internal",
		Source: livedb.Source{
			Driver: "postgres", DSN: livedbDSN,
			Redacted: "postgres://importer@db.internal:5432/crm", Tables: []string{"customers"},
		},
		Hash:  "sha256:abcdef",
		State: livedb.Signed,
	}
}

// useLivedb points the routes at a fake.
func (h *harness) useLivedb(f *livedbFake) *livedbFake {
	h.srv.liveDB = func() (livedbStore, error) { return f, nil }
	return f
}

// livedbHarness is a server whose live-database store answers, holding a
// signed plan and a run report.
func livedbHarness(t *testing.T) (*harness, *livedbFake) {
	t.Helper()
	h := newHarness(t, fakeRunner{result: cannedResult()})
	f := h.useLivedb(&livedbFake{
		plan:   signedPlan(),
		report: livedb.RunReport{ID: "run-9", Plan: "plan-1", RowsRead: 12, Chunks: 3},
	})
	return h, f
}

// The four cells. A read-only key may read a plan and may do none of the three
// things that dial a database or put one in force; a read-write key may do all
// four. This is written out cell by cell because the equivalent classification
// has been got wrong twice nearby, and both times it was a table nobody tested.
func TestALivedbPlanIsReadByEitherKeyAndActedOnOnlyByAWriteKey(t *testing.T) {
	h, _ := livedbHarness(t)

	for _, act := range []struct {
		what   string
		method string
		path   string
		body   string
	}{
		{"propose", http.MethodPost, "/athanor/livedb/plans", proposeBody(livedbDSN)},
		{"sign", http.MethodPost, "/athanor/livedb/plans/plan-1/signature", `{"hash":"sha256:abcdef","by":"liliang"}`},
		{"run", http.MethodPost, "/athanor/livedb/runs", runBody(livedbDSN)},
	} {
		if code, body := h.do(act.method, act.path, "ro-secret", act.body); code != http.StatusForbidden {
			t.Errorf("a read-only key could %s: %d %s", act.what, code, body)
		}
	}
	if code, body := h.do(http.MethodGet, "/athanor/livedb/plans/plan-1", "ro-secret", ""); code != http.StatusOK {
		t.Errorf("a read-only key could not read a plan: %d %s", code, body)
	}

	// The same four with the key that may write.
	if code, body := h.do(http.MethodGet, "/athanor/livedb/plans/plan-1", "op-secret", ""); code != http.StatusOK {
		t.Errorf("get: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, "/athanor/livedb/plans", "op-secret", proposeBody(livedbDSN)); code != http.StatusCreated {
		t.Errorf("propose: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, "/athanor/livedb/plans/plan-1/signature", "op-secret", `{"hash":"sha256:abcdef","by":"liliang"}`); code != http.StatusOK {
		t.Errorf("sign: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, "/athanor/livedb/runs", "op-secret", runBody(livedbDSN)); code != http.StatusOK {
		t.Errorf("run: %d %s", code, body)
	}
}

// A refused key never reaches the store: the three writes above must not have
// dialled anything on their way to a 403.
func TestARefusedLivedbCallDoesNotReachTheStore(t *testing.T) {
	h, f := livedbHarness(t)
	h.do(http.MethodPost, "/athanor/livedb/plans", "ro-secret", proposeBody(livedbDSN))
	h.do(http.MethodPost, "/athanor/livedb/runs", "ro-secret", runBody(livedbDSN))
	if calls := f.seen(); len(calls) != 0 {
		t.Fatalf("a refused caller reached the store: %v", calls)
	}
}

// The rest of the doors: list, amend, current and runs, each on the clearance
// the table says.
func TestTheRemainingLivedbRoutesTakeTheClearanceTheyShould(t *testing.T) {
	h, f := livedbHarness(t)

	if code, body := h.do(http.MethodGet, "/athanor/livedb/plans?state=signed&limit=5", "ro-secret", ""); code != http.StatusOK {
		t.Errorf("list: %d %s", code, body)
	}
	if got := f.query; got.State != livedb.Signed || got.Limit != 5 {
		t.Errorf("the list query did not carry the filters: %+v", got)
	}
	if code, body := h.do(http.MethodGet, "/athanor/livedb/plans/current?source_key=crm@db.internal", "ro-secret", ""); code != http.StatusOK {
		t.Errorf("current: %d %s", code, body)
	}
	if code, body := h.do(http.MethodGet, "/athanor/livedb/plans/current", "ro-secret", ""); code != http.StatusBadRequest {
		t.Errorf("current without a source key: %d %s", code, body)
	}
	if code, body := h.do(http.MethodGet, "/athanor/livedb/runs?plan=plan-1", "ro-secret", ""); code != http.StatusOK {
		t.Errorf("runs: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPatch, "/athanor/livedb/plans/plan-1", "ro-secret", `{"changes":[]}`); code != http.StatusForbidden {
		t.Errorf("a read-only key amended a draft: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPatch, "/athanor/livedb/plans/plan-1", "op-secret", `{"changes":[{"table":"customers","column":"email","action":"hash"}]}`); code != http.StatusOK {
		t.Errorf("amend: %d %s", code, body)
	}
}

// Every sentinel in livedb.go, at the status the door owes it. By identity,
// through errors.Is, so a wrapped error maps the same and an edited message
// maps the same.
func TestEveryLivedbRefusalCarriesItsOwnStatus(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{livedb.ErrNoPlan, http.StatusNotFound},
		{livedb.ErrUnsigned, http.StatusConflict},
		{livedb.ErrNotDraft, http.StatusConflict},
		{livedb.ErrStaleHash, http.StatusConflict},
		{livedb.ErrWrongSource, http.StatusBadRequest},
		{livedb.ErrNoVault, http.StatusBadRequest},
		{errors.New("the database went away"), http.StatusInternalServerError},
		// Wrapped, because a store that adds context to its own refusal is
		// still refusing for the same reason.
		{fmtWrap(livedb.ErrNoPlan), http.StatusNotFound},
	} {
		h, f := livedbHarness(t)
		f.err = tc.err
		if code, body := h.do(http.MethodGet, "/athanor/livedb/plans/plan-1", "op-secret", ""); code != tc.want {
			t.Errorf("%v answered %d, want %d: %s", tc.err, code, tc.want, body)
		}
	}

	// A body the door cannot read is the caller's mistake, not the store's.
	h, _ := livedbHarness(t)
	for _, body := range []string{`{"source":`, `{"sauce":{"dsn":"x"}}`} {
		if code, answer := h.do(http.MethodPost, "/athanor/livedb/plans", "op-secret", body); code != http.StatusBadRequest {
			t.Errorf("a bad body answered %d, want 400: %s", code, answer)
		}
	}
}

func fmtWrap(err error) error { return errors.Join(errors.New("reading the schema"), err) }

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// The credential never comes back out of the door — not in an answer, not in
// a refusal, and not because the store happened to leave it somewhere.
//
// The fake is deliberately hostile on both counts: its plan carries the DSN
// the real store clears, and its error message quotes the whole connection
// string the way a driver's own error does.
func TestTheCredentialNeverComesBackOutOfTheLivedbDoor(t *testing.T) {
	check := func(t *testing.T, what string, body string) {
		t.Helper()
		if strings.Contains(body, livedbPassword_) {
			t.Fatalf("%s put the password in the answer: %s", what, body)
		}
		if strings.Contains(body, livedbDSN) {
			t.Fatalf("%s put the whole DSN in the answer: %s", what, body)
		}
	}

	// The happy path: a plan comes back, and the Source it carries is the
	// redacted one whatever the store put there.
	h, f := livedbHarness(t)
	code, body := h.do(http.MethodPost, "/athanor/livedb/plans", "op-secret", proposeBody(livedbDSN))
	if code != http.StatusCreated {
		t.Fatalf("propose: %d %s", code, body)
	}
	check(t, "a proposal", body)
	var plan livedb.Plan
	if err := json.Unmarshal([]byte(body), &plan); err != nil {
		t.Fatalf("the answer is not a plan: %v (%s)", err, body)
	}
	if plan.Source.DSN != "" {
		t.Fatalf("the plan came back holding a credential: %q", plan.Source.DSN)
	}
	if plan.Source.Redacted == "" {
		t.Errorf("the redacted form was dropped too; a reader cannot tell which database this was: %+v", plan.Source)
	}

	// The listing, which is the same plan by a different door.
	code, body = h.do(http.MethodGet, "/athanor/livedb/plans", "op-secret", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	check(t, "a listing", body)

	// The error paths, which is where an echoed body usually escapes.
	for _, tc := range []struct {
		what   string
		method string
		path   string
		body   string
	}{
		{"a failed proposal", http.MethodPost, "/athanor/livedb/plans", proposeBody(livedbDSN)},
		{"a failed run", http.MethodPost, "/athanor/livedb/runs", runBody(livedbDSN)},
	} {
		h, f := livedbHarness(t)
		f.err = errors.New(`dial "` + livedbDSN + `": connection refused`)
		code, body := h.do(tc.method, tc.path, "op-secret", tc.body)
		if code != http.StatusInternalServerError {
			t.Fatalf("%s answered %d, want 500: %s", tc.what, code, body)
		}
		check(t, tc.what, body)
	}

	// A body the decoder chokes on, with the credential in it. Go's decoder
	// quotes field names and never values, but the assertion is cheap and the
	// day that changes is the day this route leaks.
	h, _ = livedbHarness(t)
	code, body = h.do(http.MethodPost, "/athanor/livedb/plans", "op-secret",
		`{"source":{"dsn":"`+livedbDSN+`","driver":7}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("a malformed body answered %d, want 400: %s", code, body)
	}
	check(t, "a decode failure", body)
	_ = f
}

// The allow-list allows, refuses, and refuses an unreadable DSN in exactly the
// same words — so a caller cannot use the difference to read the list back.
func TestTheLivedbAllowListRefusesUnknownAndUnreadableAlike(t *testing.T) {
	h, f := livedbHarness(t)
	h.srv.opts.LiveDBHosts = []string{"db.internal", "reports.example:5432"}

	if code, body := h.do(http.MethodPost, "/athanor/livedb/plans", "op-secret", proposeBody(livedbDSN)); code != http.StatusCreated {
		t.Fatalf("a host on the list was refused: %d %s", code, body)
	}

	elsewhere := "postgres://importer:" + livedbPassword_ + "@somewhere.else:5432/crm"
	unreadable := "this is not a connection string"
	code, refusedHost := h.do(http.MethodPost, "/athanor/livedb/plans", "op-secret", proposeBody(elsewhere))
	if code != http.StatusForbidden {
		t.Fatalf("a host off the list answered %d, want 403: %s", code, refusedHost)
	}
	code, refusedGarbage := h.do(http.MethodPost, "/athanor/livedb/plans", "op-secret", proposeBody(unreadable))
	if code != http.StatusForbidden {
		t.Fatalf("an unreadable DSN answered %d, want 403: %s", code, refusedGarbage)
	}
	if refusedHost != refusedGarbage {
		t.Fatalf("the two refusals differ, so the list is an oracle:\n  %s\n  %s", refusedHost, refusedGarbage)
	}
	if strings.Contains(refusedHost, livedbPassword_) {
		t.Fatalf("the refusal quoted the credential: %s", refusedHost)
	}

	// And the run route, which is the one that reads rows.
	if code, body := h.do(http.MethodPost, "/athanor/livedb/runs", "op-secret", runBody(elsewhere)); code != http.StatusForbidden {
		t.Fatalf("a run to a host off the list answered %d, want 403: %s", code, body)
	}

	// Nothing refused reached the store: the point of the list is that the
	// dial does not happen, not that it fails politely.
	if calls := f.seen(); len(calls) != 1 || calls[0] != "propose" {
		t.Fatalf("the store saw %v, want the one allowed proposal", calls)
	}
}

// Host reading, on the shapes the two drivers accept. A host it cannot read
// is not a host it may dial.
func TestLivedbDialHostReadsTheShapesTheDriversAccept(t *testing.T) {
	for _, tc := range []struct {
		dsn  string
		host string
		port string
		ok   bool
	}{
		{"postgres://u:p@db.internal:5432/crm?sslmode=disable", "db.internal", "5432", true},
		{"postgres://u:p@db.internal/crm", "db.internal", "", true},
		{"u:p@tcp(mysql.internal:3306)/crm", "mysql.internal", "3306", true},
		{"u:p@unix(/var/run/mysqld.sock)/crm", "", "", false},
		{"host=db.internal port=5432 user=u password=p dbname=crm", "db.internal", "5432", true},
		{"", "", "", false},
		{"nonsense", "", "", false},
	} {
		host, port, ok := livedbDialHost(tc.dsn)
		if ok != tc.ok || host != tc.host || port != tc.port {
			t.Errorf("livedbDialHost(%q) = %q, %q, %v; want %q, %q, %v", tc.dsn, host, port, ok, tc.host, tc.port, tc.ok)
		}
	}
}

func TestTheAllowListMatchesAPortOnlyWhenTheEntryNamesOne(t *testing.T) {
	if !livedbHostAllowed([]string{"DB.Internal"}, "db.internal", "5432") {
		t.Error("a bare entry should allow any port on that host, and case is not identity here")
	}
	if !livedbHostAllowed([]string{"db.internal:5432"}, "db.internal", "5432") {
		t.Error("an entry naming the port should allow it")
	}
	if livedbHostAllowed([]string{"db.internal:5432"}, "db.internal", "6432") {
		t.Error("an entry naming a port allowed a different one")
	}
	if livedbHostAllowed([]string{"db.internal"}, "", "5432") {
		t.Error("an empty host matched something")
	}
}

// spyLedger keeps what it was handed, so the entries an act becomes can be
// read back without a brain deciding which of them it holds.
type spyLedger struct {
	mu      sync.Mutex
	entries []ledgerEntry
}

func (l *spyLedger) record(_ context.Context, entry ledgerEntry) (cortexdb.DecisionRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
	return cortexdb.DecisionRecord{ID: cortexdb.DecisionID(entry.ID)}, nil
}

// A proposal and its signature are one entry that changes its verdict; a run
// is its own entry, resting on the plan's. The actor is the key throughout,
// including when the body names somebody else.
func TestTheLivedbLedgerRecordsOnePlanEntryAndOneRunEntry(t *testing.T) {
	h, _ := livedbHarness(t)
	spy := &spyLedger{}
	h.srv.ledger = spy
	mirror := livedbLedger{srv: h.srv}
	ctx := withCallerKey(context.Background(), authz.Key{ID: "operator"})

	// Shaped the way pkg/livedb emits them: the act's own id is minted per
	// act and is not what the act is about, the subject is — the plan for a
	// proposal and a signature, the run for a run — and a run offers the
	// signing act's id as its premise, which is not an entry anybody wrote.
	acts := []livedb.Act{
		{ID: "act_aaa", Kind: livedb.ActPropose, Actor: "operator", Subject: "plan-1", Note: "12 columns, 3 personal"},
		// The signature names a person. That name is a claim; the actor is
		// still the credential that presented itself.
		{ID: "act_bbb", Kind: livedb.ActSign, Actor: "liliang", Subject: "plan-1", Note: "read it"},
		{ID: "act_ccc", Kind: livedb.ActRun, Actor: "liliang", Subject: "run-9", Note: "1200 rows",
			Detail:   map[string]any{"plan": "plan-1", "rows_read": 1200},
			Premises: []string{"plan-1"}},
	}
	for _, act := range acts {
		if err := mirror.Record(ctx, act); err != nil {
			t.Fatalf("record %s: %v", act.Kind, err)
		}
	}
	if len(spy.entries) != 3 {
		t.Fatalf("recorded %d entries, want 3", len(spy.entries))
	}
	propose, sign, run := spy.entries[0], spy.entries[1], spy.entries[2]

	if propose.ID != "athanor:livedb:plan:plan-1" || sign.ID != propose.ID {
		t.Errorf("a signature grew a second entry instead of updating the plan's: %q then %q", propose.ID, sign.ID)
	}
	if propose.Verdict != "proposed" || sign.Verdict != "signed" {
		t.Errorf("verdicts = %q then %q, want proposed then signed", propose.Verdict, sign.Verdict)
	}
	if propose.Kind != livedb.ActPropose || sign.Kind != livedb.ActSign || run.Kind != livedb.ActRun {
		t.Errorf("kinds = %q, %q, %q", propose.Kind, sign.Kind, run.Kind)
	}
	if run.ID != "athanor:livedb:run:run-9" {
		t.Errorf("the run's entry id = %q", run.ID)
	}
	if run.Verdict != "ran" {
		t.Errorf("the run's verdict = %q", run.Verdict)
	}
	for _, entry := range spy.entries {
		if entry.Actor != "operator" {
			t.Errorf("%s was signed by %q, not the key that called", entry.Kind, entry.Actor)
		}
	}
	// The name in the body survives, beside the actor rather than as it.
	if by, _ := sign.Detail["by"].(string); by != "liliang" {
		t.Errorf("the signer's own name was dropped: %+v", sign.Detail)
	}
	if _, named := propose.Detail["by"]; named {
		t.Errorf("a proposal by the key itself invented a second name: %+v", propose.Detail)
	}
	// A run rests on the signature that permitted it — which is the plan's
	// entry. livedb names it by subject, this side turns the subject into the
	// entry id and prefixes it the way loads.go prefixes the review decisions
	// a load rests on.
	if !contains(run.Premises, "decision:athanor:livedb:plan:plan-1") {
		t.Errorf("the run does not rest on its signature's entry: %v", run.Premises)
	}
	for _, p := range run.Premises {
		if !strings.HasPrefix(p, "decision:") {
			t.Errorf("a premise reached the ledger unprefixed, so nothing will match it: %q", p)
		}
	}
	// A proposal rests on nothing: it is the first act of the workflow.
	if len(propose.Premises) != 0 {
		t.Errorf("a proposal claimed premises: %v", propose.Premises)
	}
	// An act this package does not know is a refusal rather than an entry
	// under a guessed id.
	if err := mirror.Record(ctx, livedb.Act{ID: "x", Kind: "livedb.something", Subject: "plan-1"}); err == nil {
		t.Error("an unknown act was recorded anyway")
	}
}

// A store that could not be built is a 503, and the server keeps serving.
func TestALivedbRouteAnswers503WhenTheStoreIsUnavailable(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	h.srv.liveDB = func() (livedbStore, error) { return nil, errors.New("livedb: not implemented") }

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/athanor/livedb/plans/plan-1", ""},
		{http.MethodGet, "/athanor/livedb/plans", ""},
		{http.MethodPost, "/athanor/livedb/plans", proposeBody(livedbDSN)},
		{http.MethodPost, "/athanor/livedb/plans/plan-1/signature", `{"hash":"h"}`},
		{http.MethodPost, "/athanor/livedb/runs", runBody(livedbDSN)},
		{http.MethodGet, "/athanor/livedb/runs?plan=plan-1", ""},
		{http.MethodGet, "/athanor/livedb/plans/current?source_key=k", ""},
	} {
		code, body := h.do(tc.method, tc.path, "op-secret", tc.body)
		if code != http.StatusServiceUnavailable {
			t.Errorf("%s %s answered %d, want 503: %s", tc.method, tc.path, code, body)
		}
		if strings.Contains(body, livedbPassword_) {
			t.Errorf("the 503 quoted the credential: %s", body)
		}
	}
	// Still alive: the point of 503 rather than a panic.
	if code, _ := h.do(http.MethodGet, "/healthz", "op-secret", ""); code != http.StatusOK {
		t.Fatalf("the server did not survive an unavailable store: %d", code)
	}
}

// The handlers park the key they authorized where the store's ledger can read
// it, and they do not let the body name the proposer.
func TestALivedbHandlerCarriesTheKeyItAuthorized(t *testing.T) {
	h, f := livedbHarness(t)
	if code, body := h.do(http.MethodPost, "/athanor/livedb/plans", "op-secret",
		`{"source":{"driver":"postgres","dsn":"`+livedbDSN+`"},"options":{"by":"somebody else","note":"nightly"}}`); code != http.StatusCreated {
		t.Fatalf("propose: %d %s", code, body)
	}
	if f.actor != "operator" {
		t.Errorf("the store was called under %q, not the key that was authorized", f.actor)
	}
	if f.opts.By != "operator" {
		t.Errorf("the body named the proposer: %q", f.opts.By)
	}
	if f.opts.Note != "nightly" {
		t.Errorf("the caller's note was dropped: %q", f.opts.Note)
	}
	if code, body := h.do(http.MethodPost, "/athanor/livedb/runs", "op-secret", runBody(livedbDSN)); code != http.StatusOK {
		t.Fatalf("run: %d %s", code, body)
	}
	if f.runBy != "operator" {
		t.Errorf("the run was attributed to %q", f.runBy)
	}
	if f.run.DSN != livedbDSN {
		t.Errorf("the credential did not reach the store, so nothing could be imported: %q", f.run.DSN)
	}
}

// Follow over HTTP would run until the connection dropped. The door says so
// rather than accepting a flag it cannot honour.
func TestALivedbRunRefusesToFollowOverHTTP(t *testing.T) {
	h, f := livedbHarness(t)
	code, body := h.do(http.MethodPost, "/athanor/livedb/runs", "op-secret",
		`{"plan":"plan-1","dsn":"`+livedbDSN+`","follow":true}`)
	if code != http.StatusBadRequest {
		t.Fatalf("follow answered %d, want 400: %s", code, body)
	}
	if calls := f.seen(); len(calls) != 0 {
		t.Fatalf("the store was asked to follow anyway: %v", calls)
	}
}

// A signature with no hash is a checkbox, and the store is never asked.
func TestASignatureMustNameTheHashItRead(t *testing.T) {
	h, f := livedbHarness(t)
	if code, body := h.do(http.MethodPost, "/athanor/livedb/plans/plan-1/signature", "op-secret", `{"by":"liliang"}`); code != http.StatusBadRequest {
		t.Fatalf("a signature without a hash answered %d, want 400: %s", code, body)
	}
	if calls := f.seen(); len(calls) != 0 {
		t.Fatalf("the store was asked to sign anyway: %v", calls)
	}
	if code, body := h.do(http.MethodPost, "/athanor/livedb/plans/plan-1/signature", "op-secret", `{"hash":"sha256:abcdef"}`); code != http.StatusOK {
		t.Fatalf("sign: %d %s", code, body)
	}
	if f.signBy != "operator" {
		t.Errorf("an unsigned name defaulted to %q, want the key's id", f.signBy)
	}
	if f.hash != "sha256:abcdef" {
		t.Errorf("the hash did not reach the store: %q", f.hash)
	}
}
