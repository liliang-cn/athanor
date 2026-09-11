package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liliang-cn/alchemy/pkg/alchemy"
	alchemyv1 "github.com/liliang-cn/alchemy/proto/alchemy/v1"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// The ledger is tested through the doors, not around them: a load is a POST,
// a review decision is an RPC, an ontology approve is a POST, and what is
// asserted afterwards is what the brain answers when asked.

// decisionsResponse is the shape of GET /athanor/decisions.
type decisionsResponse struct {
	Decisions []cortexdb.DecisionRecord `json:"decisions"`
	Count     int                       `json:"count"`
}

// chainResponse is the shape of GET /athanor/decisions/{id}.
type chainResponse struct {
	Chain cortexdb.DecisionChain `json:"chain"`
}

func (h *harness) decisions(t *testing.T, query, bearer string) decisionsResponse {
	t.Helper()
	resp, err := h.http(http.MethodGet, "/athanor/decisions"+query, bearer, "")
	if err != nil {
		t.Fatalf("decisions%s: %v", query, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("decisions%s answered %d: %s", query, resp.StatusCode, body)
	}
	var out decisionsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decisions%s: %v (%s)", query, err, body)
	}
	return out
}

func TestALoadRecordsItselfInTheLedger(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	jobID := uploadAndCreate(t, h)

	before, err := h.srv.db.ContractTally(context.Background())
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	resp, err := h.http(http.MethodPost, "/athanor/loads", "op-secret", `{"job":"`+jobID+`","load":"march"}`)
	if err != nil {
		t.Fatalf("loads: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loads answered %d: %s", resp.StatusCode, body)
	}
	var answer struct {
		Decision    string `json:"decision"`
		LedgerError string `json:"ledger_error"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("load answer: %v (%s)", err, body)
	}
	if answer.LedgerError != "" {
		t.Fatalf("the load recorded no decision: %s", answer.LedgerError)
	}
	if answer.Decision == "" {
		t.Fatalf("the load answer names no decision: %s", body)
	}

	// The chain names the job and the load.
	chain, err := h.srv.db.DecisionChain(context.Background(), answer.Decision, 0)
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	if len(chain.Decisions) == 0 {
		t.Fatal("the chain is empty")
	}
	root := chain.Decisions[0]
	if root.Kind != cortexdb.DecisionKindLoad {
		t.Errorf("kind = %q, want %q", root.Kind, cortexdb.DecisionKindLoad)
	}
	if root.Actor != "operator" {
		t.Errorf("actor = %q, want the key id", root.Actor)
	}
	if !strings.Contains(root.Note, jobID) {
		t.Errorf("the chain does not name the job %s: %s", jobID, root.Note)
	}
	if !strings.Contains(root.Note, "march") {
		t.Errorf("the chain does not name the load: %s", root.Note)
	}

	// A named person signed it, so the shelf gained one verified node.
	after, err := h.srv.db.ContractTally(context.Background())
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if got := after.Verified.Nodes - before.Verified.Nodes; got != 1 {
		t.Fatalf("verified nodes gained %d, want 1 (the decision); %+v", got, after)
	}
}

func TestALoadSurvivesALedgerWriteThatFails(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	jobID := uploadAndCreate(t, h)
	h.srv.ledger = brokenLedger{}

	resp, err := h.http(http.MethodPost, "/athanor/loads", "op-secret", `{"job":"`+jobID+`"}`)
	if err != nil {
		t.Fatalf("loads: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a ledger failure undid the load: %d %s", resp.StatusCode, body)
	}
	var answer struct {
		LedgerError string `json:"ledger_error"`
	}
	_ = json.Unmarshal(body, &answer)
	if answer.LedgerError == "" {
		t.Fatalf("the response says nothing about the ledger failure: %s", body)
	}
	// The graph is in the brain regardless — the load is what succeeded.
	tally, err := h.srv.db.ContractTally(context.Background())
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if tally.Asserted.Nodes+tally.Asserted.Edges != 3 {
		t.Fatalf("the load did not land: %+v", tally)
	}
}

// brokenLedger is the injected failure: a ledger that refuses every write.
type brokenLedger struct{}

func (brokenLedger) record(context.Context, ledgerEntry) (cortexdb.DecisionRecord, error) {
	return cortexdb.DecisionRecord{}, errors.New("the ledger is unavailable")
}

func TestADecideRecordsAReviewDecisionUnderTheKeyId(t *testing.T) {
	h, jobID, item := heldHarness(t)

	if _, err := alchemyv1.NewAlchemyClient(h.conn).Decide(asKey("op-secret"), &alchemyv1.DecideRequest{
		JobId: jobID,
		Decisions: []*alchemyv1.ReviewDecision{{
			JobId: jobID, ItemId: item.GetId(), Verb: alchemyv1.ReviewVerb_REVIEW_VERB_ACCEPT,
			By: "liliang", Note: "hp really is the primary",
		}},
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}

	got := h.decisions(t, "?kind=review", "op-secret")
	if len(got.Decisions) != 1 {
		t.Fatalf("review decisions recorded = %d, want 1: %+v", len(got.Decisions), got.Decisions)
	}
	d := got.Decisions[0]
	if d.Actor != "operator" {
		t.Errorf("actor = %q, want the key id that called", d.Actor)
	}
	if !strings.Contains(d.Note, "hp really is the primary") {
		t.Errorf("the reviewer's note was not preserved: %s", d.Note)
	}
	if !strings.Contains(d.Note, "liliang") {
		t.Errorf("the free-text by was not kept beside the key id: %s", d.Note)
	}
	if !strings.Contains(d.Note, item.GetSubject()) {
		t.Errorf("the conflict subject is not in the entry: %s", d.Note)
	}
}

func TestAnOntologyApproveReachesTheLedger(t *testing.T) {
	h := newHarness(t, fakeRunner{result: proposingResult()})
	jobID := uploadAndCreate(t, h)

	if code, body := h.do(http.MethodPost, "/athanor/ontologies", "op-secret", ontologyDoc); code != http.StatusCreated {
		t.Fatalf("draft: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, path("sds-demo@1", "propose"), "op-secret",
		`{"job":"`+jobID+`","part":"prose"}`); code != http.StatusOK {
		t.Fatalf("propose: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, path("sds-demo@1", "approve"), "op-secret",
		`{"accept":["Cluster"],"by":"liliang","note":"a cluster is a real thing here"}`); code != http.StatusCreated {
		t.Fatalf("approve: %d %s", code, body)
	}

	got := h.decisions(t, "?kind=ontology.approve", "op-secret")
	if len(got.Decisions) != 1 {
		t.Fatalf("ontology.approve entries = %d, want 1: %+v", len(got.Decisions), got.Decisions)
	}
	d := got.Decisions[0]
	if d.Actor != "operator" {
		t.Errorf("actor = %q, want the key id", d.Actor)
	}
	if !strings.Contains(d.Note, "liliang") {
		t.Errorf("the name the approval was signed with is missing: %s", d.Note)
	}
	// The ontology table stays the source of truth and still holds its own act.
	store, err := h.srv.ontologies()
	if err != nil {
		t.Fatalf("ontologies: %v", err)
	}
	acts, err := store.Acts(context.Background(), "")
	if err != nil {
		t.Fatalf("acts: %v", err)
	}
	if len(acts) < 3 {
		t.Fatalf("the workflow's own acts were not kept: %+v", acts)
	}
}

func TestPrecedentsByKindAreNewestFirst(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	first := uploadAndCreate(t, h)
	if code, body := h.do(http.MethodPost, "/athanor/loads", "op-secret", `{"job":"`+first+`","load":"one"}`); code != http.StatusOK {
		t.Fatalf("first load: %d %s", code, body)
	}
	// A second apart, so "newest first" is a claim about time and not about
	// whichever row the scan reached first.
	time.Sleep(1100 * time.Millisecond)
	second := uploadAndCreate(t, h)
	if code, body := h.do(http.MethodPost, "/athanor/loads", "op-secret", `{"job":"`+second+`","load":"two"}`); code != http.StatusOK {
		t.Fatalf("second load: %d %s", code, body)
	}

	got := h.decisions(t, "?kind=load", "op-secret")
	if len(got.Decisions) != 2 {
		t.Fatalf("load decisions = %d, want 2: %+v", len(got.Decisions), got.Decisions)
	}
	if !strings.Contains(got.Decisions[0].Note, "two") {
		t.Fatalf("newest first is not what came back: %+v", got.Decisions)
	}
}

func TestAReadOnlyKeyReadsTheLedgerAndCannotWriteToIt(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	jobID := uploadAndCreate(t, h)
	if code, body := h.do(http.MethodPost, "/athanor/loads", "op-secret", `{"job":"`+jobID+`"}`); code != http.StatusOK {
		t.Fatalf("load: %d %s", code, body)
	}

	got := h.decisions(t, "?kind=load", "ro-secret")
	if len(got.Decisions) != 1 {
		t.Fatalf("a reader could not read the ledger: %+v", got)
	}
	// There is no route that writes a decision, and the act that records one
	// is a write a reader is refused before anything happens.
	if code, _ := h.do(http.MethodPost, "/athanor/decisions", "ro-secret", `{}`); code != http.StatusMethodNotAllowed {
		t.Fatalf("the ledger has a write door: %d", code)
	}
	if code, _ := h.do(http.MethodPost, "/athanor/loads", "ro-secret", `{"job":"`+jobID+`"}`); code != http.StatusForbidden {
		t.Fatalf("a reader recorded a load: %d", code)
	}
	// No key at all reads nothing.
	resp, _ := h.http(http.MethodGet, "/athanor/decisions?kind=load", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the ledger answered a caller with no key: %d", resp.StatusCode)
	}
}

// confinedKeys adds a third key confined to one user_id, which is the row
// confinement auth.go's package note named as the gap this closes.
const confinedKeysJSON = `{"keys":[
  {"id":"operator","secret":"op-secret","clearance":"read-write"},
  {"id":"reader","secret":"ro-secret","clearance":"read-only"},
  {"id":"hermes","secret":"hermes-secret","clearance":"read-write","scope":{"user_id":"hermes"}}
]}`

func newConfinedHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(keyFile, []byte(confinedKeysJSON), 0o600); err != nil {
		t.Fatalf("keys: %v", err)
	}
	return newHarnessWith(t, fakeRunner{result: cannedResult()}, keyFile, dir)
}

func TestAConfinedKeySeesOnlyItsOwnLedgerEntries(t *testing.T) {
	h := newConfinedHarness(t)

	mine := uploadAndCreate(t, h)
	if code, body := h.do(http.MethodPost, "/athanor/loads", "hermes-secret", `{"job":"`+mine+`","load":"hermes-load"}`); code != http.StatusOK {
		t.Fatalf("confined load: %d %s", code, body)
	}
	theirs := uploadAndCreate(t, h)
	resp, err := h.http(http.MethodPost, "/athanor/loads", "op-secret", `{"job":"`+theirs+`","load":"operator-load"}`)
	if err != nil {
		t.Fatalf("operator load: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("operator load: %d %s", resp.StatusCode, body)
	}
	var answer struct {
		Decision string `json:"decision"`
	}
	_ = json.Unmarshal(body, &answer)

	// The operator sees both; hermes sees one, and it is hermes's own.
	if got := h.decisions(t, "?kind=load", "op-secret"); len(got.Decisions) != 2 {
		t.Fatalf("the operator sees %d loads, want 2", len(got.Decisions))
	}
	confined := h.decisions(t, "?kind=load", "hermes-secret")
	if len(confined.Decisions) != 1 || confined.Decisions[0].Actor != "hermes" {
		t.Fatalf("a confined key saw somebody else's ledger: %+v", confined.Decisions)
	}
	// And it cannot open the chain of a decision it may not see. The answer is
	// the one a missing decision gets, word for word — CortexDB's rule.
	code, chainBody := h.do(http.MethodGet, "/athanor/decisions/"+answer.Decision, "hermes-secret", "")
	if code != http.StatusNotFound {
		t.Fatalf("a confined key opened somebody else's chain: %d %s", code, chainBody)
	}
	missing, absentBody := h.do(http.MethodGet, "/athanor/decisions/decision:athanor:load:nobody:nothing", "hermes-secret", "")
	if missing != code || absentBody != chainBody {
		t.Fatalf("forbidden and missing answer differently:\n forbidden %d %s\n missing   %d %s",
			code, chainBody, missing, absentBody)
	}
}

func TestTheChainRouteAnswersTheDecisionAndItsPremises(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	jobID := uploadAndCreate(t, h)
	resp, err := h.http(http.MethodPost, "/athanor/loads", "op-secret", `{"job":"`+jobID+`"}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var answer struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("load answer: %v", err)
	}

	code, chainBody := h.do(http.MethodGet, "/athanor/decisions/"+answer.Decision, "ro-secret", "")
	if code != http.StatusOK {
		t.Fatalf("chain: %d %s", code, chainBody)
	}
	var out chainResponse
	if err := json.Unmarshal([]byte(chainBody), &out); err != nil {
		t.Fatalf("chain body: %v (%s)", err, chainBody)
	}
	if out.Chain.Root != answer.Decision || len(out.Chain.Decisions) == 0 {
		t.Fatalf("chain: %+v", out.Chain)
	}
}

func TestTheFrontPageShowsTheLedger(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	jobID := uploadAndCreate(t, h)
	if code, body := h.do(http.MethodPost, "/athanor/loads", "op-secret", `{"job":"`+jobID+`","load":"march"}`); code != http.StatusOK {
		t.Fatalf("load: %d %s", code, body)
	}
	// An agent's own decision, recorded the way agents record one, has to
	// appear beside Athanor's.
	if _, err := h.srv.db.RecordDecision(context.Background(), cortexdb.DecisionRecordRequest{
		Kind: cortexdb.DecisionKindAction, Actor: "hermes", Note: "held the release",
	}); err != nil {
		t.Fatalf("agent decision: %v", err)
	}

	// The ledger the interface reads is the JSON route, authenticated by the
	// same cookie a browser carries. Athanor renders no HTML of its own.
	req, _ := http.NewRequest(http.MethodGet, "http://"+h.httpAddr+"/athanor/decisions?limit=20", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "op-secret"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the ledger answered %d: %s", resp.StatusCode, page)
	}
	for _, want := range []string{"march", "held the release"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("the ledger does not report %q", want)
		}
	}
}

// heldHarness is a job the pipeline stopped on a conflict, with its first
// finding — the shape every review test starts from.
func heldHarness(t *testing.T) (*harness, string, *alchemyv1.ReviewItem) {
	t.Helper()
	held := cannedResult()
	held.Conflicts = []alchemy.Conflict{{
		Kind: alchemy.ConflictCardinality, Subject: "drbdresource:sds-meta",
		Detail: "two nodes claim to promote it",
	}}
	h := newHarness(t, fakeRunner{result: held})
	jobID := uploadAndCreate(t, h)
	findings, err := alchemyv1.NewAlchemyClient(h.conn).ListFindings(asKey("op-secret"),
		&alchemyv1.ListFindingsRequest{JobId: jobID})
	if err != nil {
		t.Fatalf("list findings: %v", err)
	}
	if len(findings.GetItems()) == 0 {
		t.Fatal("the held job produced no findings to decide")
	}
	return h, jobID, findings.GetItems()[0]
}

func TestAReviewOverTheStreamAlsoReachesTheLedger(t *testing.T) {
	h, jobID, item := heldHarness(t)

	stream, err := alchemyv1.NewAlchemyClient(h.conn).Review(asKey("op-secret"))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if err := stream.Send(&alchemyv1.ReviewDecision{
		JobId: jobID, ItemId: item.GetId(), Verb: alchemyv1.ReviewVerb_REVIEW_VERB_ACCEPT,
		By: "liliang", Note: "answered on the stream",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	got := h.decisions(t, "?kind=review", "op-secret")
	if len(got.Decisions) != 1 {
		t.Fatalf("the stream recorded %d entries, want 1: %+v", len(got.Decisions), got.Decisions)
	}
	if !strings.Contains(got.Decisions[0].Note, "answered on the stream") {
		t.Fatalf("the reviewer's note did not survive the stream: %s", got.Decisions[0].Note)
	}
}

func TestALoadRestsOnTheReviewThatUnblockedIt(t *testing.T) {
	h, jobID, item := heldHarness(t)

	if _, err := alchemyv1.NewAlchemyClient(h.conn).Decide(asKey("op-secret"), &alchemyv1.DecideRequest{
		JobId: jobID,
		Decisions: []*alchemyv1.ReviewDecision{{
			JobId: jobID, ItemId: item.GetId(), Verb: alchemyv1.ReviewVerb_REVIEW_VERB_ACCEPT,
			By: "liliang", Note: "hp really is the primary",
		}},
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	resp, err := h.http(http.MethodPost, "/athanor/loads", "op-secret", `{"job":"`+jobID+`"}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the answered job did not load: %d %s", resp.StatusCode, body)
	}
	var answer struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("load answer: %v", err)
	}

	chain, err := h.srv.db.DecisionChain(context.Background(), answer.Decision, 0)
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	if len(chain.Decisions) != 2 {
		t.Fatalf("the load's chain is %d decisions, want the load and the review it rests on: %+v",
			len(chain.Decisions), chain.Decisions)
	}
	var premise cortexdb.DecisionPremise
	for _, p := range chain.Decisions[0].Premises {
		if p.Decision {
			premise = p
		}
	}
	if premise.ID == "" || premise.Grade != "verified" {
		t.Fatalf("the load does not rest on a graded review decision: %+v", chain.Decisions[0].Premises)
	}
	if chain.Decisions[1].Kind != cortexdb.DecisionKindReview {
		t.Fatalf("what the load rests on is a %q", chain.Decisions[1].Kind)
	}
}
