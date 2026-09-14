package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/liliang-cn/alchemy/pkg/alchemy"
	"github.com/liliang-cn/athanor/pkg/rules"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// The rule engine is tested through the doors and against a real brain: the
// graph is put there by a load, the rule is declared and published over HTTP,
// and what is asserted afterwards is what CortexDB answers when asked. The
// engine is CortexDB's own — faking it here would prove nothing about the one
// thing this file exists to check, which is that a rule fired through Athanor
// reaches the brain graded and explicable.

// chain is the rule: a manager's manager manages you.
const chainRule = `{"id":"chain@1","name":"the management chain",
  "text":"IF manages(?a, ?b) AND manages(?b, ?c) THEN manages_chain(?a, ?c)",
  "note":"a manager's manager manages you"}`

// chainResult is what the fake pipeline "extracted": three people and the two
// edges a two-hop rule needs to have something to chain.
func chainResult() alchemy.Result {
	prov := alchemy.Provenance{Source: "orgchart.md", Chunk: 0, Producer: alchemy.ProducerLLMExtract, Ontology: "sds-demo@1", Model: "fake"}
	return alchemy.Result{
		Entities: []alchemy.Entity{
			{ID: "person:ana", Type: "Node", Name: "ana", Provenance: prov},
			{ID: "person:bo", Type: "Node", Name: "bo", Provenance: prov},
			{ID: "person:cy", Type: "Node", Name: "cy", Provenance: prov},
		},
		Relations: []alchemy.Relation{
			{From: "person:ana", To: "person:bo", Type: "manages", Provenance: prov},
			{From: "person:bo", To: "person:cy", Type: "manages", Provenance: prov},
		},
	}
}

// rulePath escapes an id for a URL. An id carries an "@" and an act carries a
// ":", both legal in a path segment; this is here so a test cannot pass by
// accidentally agreeing with the handler about escaping.
func rulePath(id, verb string) string {
	seg := id
	if verb != "" {
		seg += ":" + verb
	}
	return "/athanor/rules/" + url.PathEscape(seg)
}

// chained is a harness with the two manages edges already in the brain, which
// is what every rule below has to have something to fire over.
func chained(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, &fakeRunner{result: chainResult()})
	jobID := uploadAndCreate(t, h)
	if code, body := h.do(http.MethodPost, "/athanor/loads", "op-secret", `{"job":"`+jobID+`","load":"orgchart"}`); code != http.StatusOK {
		t.Fatalf("load: %d %s", code, body)
	}
	return h
}

func decodeFiring(t *testing.T, body string) rules.Firing {
	t.Helper()
	var f rules.Firing
	if err := json.Unmarshal([]byte(body), &f); err != nil {
		t.Fatalf("firing: %v (%s)", err, body)
	}
	return f
}

func TestARuleIsDeclaredPutInForceAndFiredIntoTheBrain(t *testing.T) {
	h := chained(t)

	// 1. declared: the document, validated by CortexDB's own parser.
	code, body := h.do(http.MethodPost, "/athanor/rules?note=the+first+one", "op-secret", chainRule)
	if code != http.StatusCreated {
		t.Fatalf("declare: %d %s", code, body)
	}
	var declared rules.Rule
	if err := json.Unmarshal([]byte(body), &declared); err != nil {
		t.Fatalf("declare: %v (%s)", err, body)
	}
	if declared.ID != "chain@1" || declared.State != rules.Draft || declared.Lineage != "chain" {
		t.Fatalf("the rule landed as %+v", declared)
	}

	// 2. a draft writes nothing, but it can be read: the dry run is how a
	// person sees what publishing would let loose.
	code, body = h.do(http.MethodPost, rulePath("chain@1", "apply"), "op-secret", `{"by":"liliang"}`)
	if code != http.StatusConflict {
		t.Fatalf("a draft fired for real: %d %s", code, body)
	}
	code, body = h.do(http.MethodPost, rulePath("chain@1", "apply"), "op-secret", `{"by":"liliang","dry_run":true}`)
	if code != http.StatusOK {
		t.Fatalf("dry run: %d %s", code, body)
	}
	if dry := decodeFiring(t, body); len(dry.Created) != 1 || !dry.DryRun {
		t.Fatalf("the dry run did not say what it would derive: %+v", dry)
	}
	if tally, err := h.srv.db.ContractTally(context.Background()); err != nil || tally.SelfConsistent.Edges != 0 {
		t.Fatalf("a dry run wrote to the graph: %+v (%v)", tally, err)
	}

	// 3. in force.
	if code, body := h.do(http.MethodPost, rulePath("chain@1", "publish"), "op-secret", `{"by":"liliang"}`); code != http.StatusOK {
		t.Fatalf("publish: %d %s", code, body)
	}

	// 4. fired: the edge is derived, and it names the two edges under it.
	code, body = h.do(http.MethodPost, rulePath("chain@1", "apply"), "op-secret",
		`{"by":"liliang","note":"after the orgchart import"}`)
	if code != http.StatusOK {
		t.Fatalf("apply: %d %s", code, body)
	}
	firing := decodeFiring(t, body)
	if firing.LedgerError != "" || firing.Act == "" {
		t.Fatalf("the firing recorded nothing: %+v", firing)
	}
	if firing.By != "liliang" || firing.Actor != "operator" {
		t.Fatalf("the firing names %q under %q", firing.By, firing.Actor)
	}
	if len(firing.Created) != 1 || len(firing.Edges) != 1 {
		t.Fatalf("the firing derived %d edges: %+v", len(firing.Created), firing)
	}
	derived := firing.Edges[0]
	if len(derived.Supports) != 2 {
		t.Fatalf("the inference does not name its premises: %+v", derived)
	}

	// The brain has it, graded: derived deterministically from what was
	// already stated, and checked against nothing in the world.
	tally, err := h.srv.db.ContractTally(context.Background())
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if tally.SelfConsistent.Edges != 1 {
		t.Fatalf("the derived edge did not reach the shelf graded: %+v", tally)
	}
	if tally.Untagged.Nodes+tally.Untagged.Edges != 0 {
		t.Fatalf("a record arrived without a contract: %+v", tally)
	}

	// And it can say how it knows: back to the rule, and back to the facts the
	// rule rested on.
	prov, err := h.srv.db.FactProvenanceFor(context.Background(), derived.EdgeID, false)
	if err != nil {
		t.Fatalf("fact provenance: %v", err)
	}
	if !prov.Inferred || prov.Rule != "chain@1" || !prov.Cited() {
		t.Fatalf("the derived edge cannot say where it came from: %+v", prov)
	}
	explained, err := h.srv.db.ExplainInference(context.Background(), cortexdb.InferenceExplainRequest{EdgeID: derived.EdgeID, Depth: 2})
	if err != nil {
		t.Fatalf("inference explain: %v", err)
	}
	if !strings.Contains(explained.Explanation.RuleText, "manages_chain") || len(explained.Explanation.SupportEdgeIDs) != 2 {
		t.Fatalf("the explanation is %+v", explained.Explanation)
	}

	// `current` is the JSON a client pastes anywhere a rule is accepted.
	resp, err := h.http(http.MethodGet, "/athanor/rules/current?lineage=chain", "op-secret", "")
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	version := resp.Header.Get("Athanor-Rule-Version")
	resp.Body.Close()
	if version != "chain@1" {
		t.Fatalf("current names version %q", version)
	}

	// The firings are readable back: the dry run and the real one.
	code, body = h.do(http.MethodGet, "/athanor/rules/firings?rule=chain@1", "op-secret", "")
	if code != http.StatusOK {
		t.Fatalf("firings: %d %s", code, body)
	}
	var listed struct {
		Firings []rules.Firing `json:"firings"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil || len(listed.Firings) != 2 {
		t.Fatalf("firings = %s (%v)", body, err)
	}

	// Retiring is not deleting: the rule stops firing and what it derived
	// stays, still naming the version that derived it.
	if code, body := h.do(http.MethodPost, rulePath("chain@1", "retire"), "op-secret", `{"by":"liliang"}`); code != http.StatusOK {
		t.Fatalf("retire: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, rulePath("chain@1", "apply"), "op-secret", `{"by":"liliang"}`); code != http.StatusConflict {
		t.Fatalf("a retired rule fired: %d %s", code, body)
	}
	after, err := h.srv.db.FactProvenanceFor(context.Background(), derived.EdgeID, false)
	if err != nil || after.Rule != "chain@1" {
		t.Fatalf("retiring the rule orphaned what it derived: %+v (%v)", after, err)
	}
}

func TestAFiringNobodyIsNamedForIsRefused(t *testing.T) {
	h := chained(t)
	if code, body := h.do(http.MethodPost, "/athanor/rules", "op-secret", chainRule); code != http.StatusCreated {
		t.Fatalf("declare: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, rulePath("chain@1", "publish"), "op-secret", `{"by":"liliang"}`); code != http.StatusOK {
		t.Fatalf("publish: %d %s", code, body)
	}

	for _, body := range []string{`{}`, `{"by":""}`, `{"by":"   "}`} {
		if code, answer := h.do(http.MethodPost, rulePath("chain@1", "apply"), "op-secret", body); code != http.StatusBadRequest {
			t.Fatalf("a firing signed %s was accepted: %d %s", body, code, answer)
		}
	}
	// Nothing was derived by any of that, and nothing was recorded.
	tally, err := h.srv.db.ContractTally(context.Background())
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if tally.SelfConsistent.Edges != 0 {
		t.Fatalf("an unsigned firing changed the graph: %+v", tally)
	}
	if got := h.decisions(t, "?kind=rule.apply", "op-secret"); len(got.Decisions) != 0 {
		t.Fatalf("an unsigned firing reached the ledger: %+v", got.Decisions)
	}
}

func TestAReaderMayReadEveryRuleAndFireNone(t *testing.T) {
	h := chained(t)
	if code, body := h.do(http.MethodPost, "/athanor/rules", "op-secret", chainRule); code != http.StatusCreated {
		t.Fatalf("declare: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, rulePath("chain@1", "publish"), "op-secret", `{"by":"liliang"}`); code != http.StatusOK {
		t.Fatalf("publish: %d %s", code, body)
	}

	writes := []struct{ path, body string }{
		{"/athanor/rules", chainRule},
		{rulePath("chain@1", "publish"), `{"by":"liliang"}`},
		{rulePath("chain@1", "retire"), `{"by":"liliang"}`},
		{rulePath("chain@1", "apply"), `{"by":"liliang"}`},
	}
	for _, w := range writes {
		if code, body := h.do(http.MethodPost, w.path, "ro-secret", w.body); code != http.StatusForbidden {
			t.Fatalf("a reader wrote to %s: %d %s", w.path, code, body)
		}
		if code, body := h.do(http.MethodPost, w.path, "", w.body); code != http.StatusUnauthorized {
			t.Fatalf("no key wrote to %s: %d %s", w.path, code, body)
		}
	}
	reads := []string{
		"/athanor/rules",
		rulePath("chain@1", ""),
		"/athanor/rules/current?lineage=chain",
		"/athanor/rules/firings",
	}
	for _, p := range reads {
		if code, body := h.do(http.MethodGet, p, "ro-secret", ""); code != http.StatusOK {
			t.Fatalf("a reader was refused %s: %d %s", p, code, body)
		}
		if code, _ := h.do(http.MethodGet, p, "", ""); code != http.StatusUnauthorized {
			t.Fatalf("%s answered a caller with no key", p)
		}
	}
	// A reader's refusal never reached the graph either.
	if tally, err := h.srv.db.ContractTally(context.Background()); err != nil || tally.SelfConsistent.Edges != 0 {
		t.Fatalf("a refused firing derived something: %+v (%v)", tally, err)
	}
}

func TestFiringTheSameRuleTwiceIsOneLedgerEntry(t *testing.T) {
	h := chained(t)
	if code, body := h.do(http.MethodPost, "/athanor/rules", "op-secret", chainRule); code != http.StatusCreated {
		t.Fatalf("declare: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, rulePath("chain@1", "publish"), "op-secret", `{"by":"liliang"}`); code != http.StatusOK {
		t.Fatalf("publish: %d %s", code, body)
	}
	for i := 0; i < 2; i++ {
		if code, body := h.do(http.MethodPost, rulePath("chain@1", "apply"), "op-secret", `{"by":"liliang"}`); code != http.StatusOK {
			t.Fatalf("apply %d: %d %s", i, code, body)
		}
	}
	got := h.decisions(t, "?kind=rule.apply", "op-secret")
	if len(got.Decisions) != 1 {
		t.Fatalf("two runs of one rule are %d entries, want 1: %+v", len(got.Decisions), got.Decisions)
	}
	// A dry run keeps its own entry: it derived nothing, and overwriting the
	// record of a firing that really did write edges would lose the only entry
	// that said so.
	if code, body := h.do(http.MethodPost, rulePath("chain@1", "apply"), "op-secret", `{"by":"liliang","dry_run":true}`); code != http.StatusOK {
		t.Fatalf("dry run: %d %s", code, body)
	}
	if got := h.decisions(t, "?kind=rule.apply", "op-secret"); len(got.Decisions) != 2 {
		t.Fatalf("a dry run overwrote the firing: %+v", got.Decisions)
	}

	// The chain walks back from the firing to who put the rule in force, and
	// from there to who declared it.
	code, body := h.do(http.MethodGet, "/athanor/decisions/decision:athanor:rule:apply:chain@1:graph", "op-secret", "")
	if code != http.StatusOK {
		t.Fatalf("chain: %d %s", code, body)
	}
	var chain chainResponse
	if err := json.Unmarshal([]byte(body), &chain); err != nil {
		t.Fatalf("chain: %v (%s)", err, body)
	}
	kinds := map[string]bool{}
	for _, d := range chain.Chain.Decisions {
		kinds[d.Kind] = true
	}
	for _, want := range []string{"rule.apply", "rule.publish", "rule.draft"} {
		if !kinds[want] {
			t.Fatalf("the firing's chain does not reach %s: %+v", want, chain.Chain.Decisions)
		}
	}
	// The actor is the key, and the name the firing was signed with is kept
	// beside it rather than instead of it.
	root := chain.Chain.Decisions[0]
	if root.Actor != "operator" || !strings.Contains(root.Note, "liliang") {
		t.Fatalf("the firing is recorded as %+v", root)
	}
}

func TestAFiringSurvivesALedgerWriteThatFails(t *testing.T) {
	h := chained(t)
	if code, body := h.do(http.MethodPost, "/athanor/rules", "op-secret", chainRule); code != http.StatusCreated {
		t.Fatalf("declare: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, rulePath("chain@1", "publish"), "op-secret", `{"by":"liliang"}`); code != http.StatusOK {
		t.Fatalf("publish: %d %s", code, body)
	}
	h.srv.ledger = brokenLedger{}

	code, body := h.do(http.MethodPost, rulePath("chain@1", "apply"), "op-secret", `{"by":"liliang"}`)
	if code != http.StatusOK {
		t.Fatalf("a ledger failure undid the firing: %d %s", code, body)
	}
	firing := decodeFiring(t, body)
	if firing.LedgerError == "" {
		t.Fatalf("the answer says nothing about the ledger failure: %s", body)
	}
	// The edges are in the brain regardless — the firing is what succeeded.
	if tally, err := h.srv.db.ContractTally(context.Background()); err != nil || tally.SelfConsistent.Edges != 1 {
		t.Fatalf("the derivation did not land: %+v (%v)", tally, err)
	}
	// And this server's own record of it is there, which is the part the
	// ledger is only a view of.
	store, err := h.srv.rules()
	if err != nil {
		t.Fatalf("rules: %v", err)
	}
	if firings, err := store.Firings(context.Background(), "chain@1", 10); err != nil || len(firings) != 1 {
		t.Fatalf("the firing is not in the record: %d (%v)", len(firings), err)
	}
}

func TestAConfinedKeySeesOnlyItsOwnFirings(t *testing.T) {
	h := newConfinedHarness(t)
	// A rule each, so that two keys can fire without one of them re-running
	// the other's entry.
	for _, id := range []string{"chain@1", "second@1"} {
		declaration := strings.Replace(chainRule, "chain@1", id, 1)
		if code, body := h.do(http.MethodPost, "/athanor/rules", "op-secret", declaration); code != http.StatusCreated {
			t.Fatalf("declare %s: %d %s", id, code, body)
		}
		if code, body := h.do(http.MethodPost, rulePath(id, "publish"), "op-secret", `{"by":"liliang"}`); code != http.StatusOK {
			t.Fatalf("publish %s: %d %s", id, code, body)
		}
	}
	if code, body := h.do(http.MethodPost, rulePath("chain@1", "apply"), "hermes-secret", `{"by":"hermes"}`); code != http.StatusOK {
		t.Fatalf("hermes fired nothing: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, rulePath("second@1", "apply"), "op-secret", `{"by":"liliang"}`); code != http.StatusOK {
		t.Fatalf("the operator fired nothing: %d %s", code, body)
	}

	if got := h.decisions(t, "?kind=rule.apply", "op-secret"); len(got.Decisions) != 2 {
		t.Fatalf("the operator sees %d firings, want 2", len(got.Decisions))
	}
	confined := h.decisions(t, "?kind=rule.apply", "hermes-secret")
	if len(confined.Decisions) != 1 || confined.Decisions[0].Actor != "hermes" {
		t.Fatalf("a confined key saw somebody else's firings: %+v", confined.Decisions)
	}
	// And it cannot open the chain of one it may not see. The answer is the
	// one a missing decision gets, word for word.
	code, body := h.do(http.MethodGet, "/athanor/decisions/decision:athanor:rule:apply:second@1:graph", "hermes-secret", "")
	missing, absent := h.do(http.MethodGet, "/athanor/decisions/decision:athanor:rule:apply:nothing@1:graph", "hermes-secret", "")
	if code != http.StatusNotFound || missing != code || absent != body {
		t.Fatalf("forbidden and missing answer differently:\n forbidden %d %s\n missing   %d %s", code, body, missing, absent)
	}
}

// Reloading a corpus used to take every derived fact with it, silently.
//
// A rule's conclusions are edges between the entities its premises joined, and
// those entities belong to a load. Replace the load — the ordinary way to
// correct a corpus — and the entities go, and the derived edges on them go
// too. sink.Load counts what it wrote and cannot see what a rule had added on
// top, so the load reported success and the brain came back knowing less than
// it had, with nothing anywhere saying so.
//
// It happened twice in one afternoon on a real graph, and both times the lost
// facts were the ones hardest to miss by eye: two officers whose employment no
// document stated in prose, and eight people whose city came from their team's.
func TestALoadPutsBackWhatItsPredecessorsRulesHadDerived(t *testing.T) {
	ctx := context.Background()
	// A corrected corpus, not the same one again: the same graph twice has the
	// same digest, so the load converges and deletes nothing, and a test over
	// that would prove nothing about what a real correction does. This second
	// extraction says the same two things and one more.
	corrected := chainResult()
	prov := corrected.Entities[0].Provenance
	corrected.Entities = append(corrected.Entities,
		alchemy.Entity{ID: "person:di", Type: "Node", Name: "di", Provenance: prov})
	corrected.Relations = append(corrected.Relations,
		alchemy.Relation{From: "person:cy", To: "person:di", Type: "manages", Provenance: prov})

	h := newHarness(t, &fakeRunner{result: chainResult(), next: &corrected})
	jobID := uploadAndCreate(t, h)
	if code, body := h.do(http.MethodPost, "/athanor/loads", "op-secret", `{"job":"`+jobID+`","load":"orgchart"}`); code != http.StatusOK {
		t.Fatalf("load: %d %s", code, body)
	}

	if code, body := h.do(http.MethodPost, "/athanor/rules", "op-secret", chainRule); code != http.StatusCreated {
		t.Fatalf("declare: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, rulePath("chain@1", "publish"), "op-secret", `{"by":"liliang"}`); code != http.StatusOK {
		t.Fatalf("publish: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, rulePath("chain@1", "apply"), "op-secret", `{"by":"liliang"}`); code != http.StatusOK {
		t.Fatalf("apply: %d %s", code, body)
	}
	before := derivedEdges(t, h)
	if before == 0 {
		t.Fatal("the rule derived nothing, so this test would pass for the wrong reason")
	}

	// The ordinary correction: the corrected corpus, over the old one.
	jobID = uploadAndCreate(t, h)
	code, body := h.do(http.MethodPost, "/athanor/loads", "op-secret",
		`{"job":"`+jobID+`","load":"orgchart","replace":true}`)
	if code != http.StatusOK {
		t.Fatalf("reload: %d %s", code, body)
	}

	// The answer says what it put back, so that a caller reading it knows the
	// derived half of the graph is there without going and counting.
	var answer struct {
		Derived    int    `json:"derived"`
		RulesError string `json:"rules_error"`
		Rules      []struct {
			Rule    string `json:"rule"`
			Derived int    `json:"derived"`
			Act     string `json:"act"`
			Error   string `json:"error"`
		} `json:"rules"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("answer: %v (%s)", err, body)
	}
	if answer.RulesError != "" {
		t.Errorf("a rule in force did not fire after the load: %s", answer.RulesError)
	}
	if len(answer.Rules) != 1 || answer.Rules[0].Rule != "chain@1" || answer.Rules[0].Act == "" {
		t.Errorf("the load does not report re-firing the rule in force: %s", body)
	}
	// The corrected corpus has one more link in the chain, so the rule has
	// more to derive than it did — which is the point of re-firing rather than
	// of restoring what was deleted. What must never happen is fewer.
	if answer.Derived < before {
		t.Errorf("the load put back %d derived edges and the rule had produced %d before it", answer.Derived, before)
	}

	// And the brain really holds them.
	if got := derivedEdges(t, h); got != answer.Derived {
		t.Errorf("the load reported %d derived edges and the brain holds %d", answer.Derived, got)
	} else if got < before {
		t.Errorf("after the reload the brain holds %d derived edges, and held %d before it", got, before)
	}
	if _, err := h.srv.db.ContractTally(ctx); err != nil {
		t.Fatalf("tally: %v", err)
	}
}

// derivedEdges counts the edges that no document stated: the self_consistent
// half of the contract, which is exactly what a rule produces.
func derivedEdges(t *testing.T, h *harness) int {
	t.Helper()
	tally, err := h.srv.db.ContractTally(context.Background())
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	return tally.SelfConsistent.Edges
}
