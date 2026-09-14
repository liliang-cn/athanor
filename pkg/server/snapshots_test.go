package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/liliang-cn/athanor/pkg/snapshots"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
	"github.com/liliang-cn/cortexdb/v2/pkg/graph"
)

// The snapshot routes, through a bound server. Nothing here needs a model: the
// pipeline is the fake runner every other test in this package uses, and the
// one fact whose grade moves is moved by writing it back into the graph, which
// is what a re-load or a correction does.

// snapshotPath escapes a name for a URL. A name is the caller's own word and an
// act is spelled `{name}:drop`, so this is here for the reason the ontology
// tests have their own: a test must not pass by accidentally agreeing with the
// handler about escaping.
func snapshotPath(name, verb string) string {
	seg := name
	if verb != "" {
		seg += ":" + verb
	}
	return "/athanor/snapshots/" + url.PathEscape(seg)
}

// takeSnapshot names the present moment and returns the whole answer.
func (h *harness) take(t *testing.T, name, bearer, body string) map[string]any {
	t.Helper()
	code, answer := h.do(http.MethodPost, "/athanor/snapshots", bearer, body)
	if code != http.StatusCreated {
		t.Fatalf("take %s: %d %s", name, code, answer)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(answer), &out); err != nil {
		t.Fatalf("take %s: %v (%s)", name, err, answer)
	}
	return out
}

func decodeSnapshot(t *testing.T, answer map[string]any) snapshots.Snapshot {
	t.Helper()
	body, err := json.Marshal(answer["snapshot"])
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var snap snapshots.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("snapshot: %v (%s)", err, body)
	}
	return snap
}

// regrade rewrites one fact the pipeline already wrote at a different grade —
// a correction, or the second half of a review that moved. It is done through
// the brain's own handle because no door of Athanor's changes a grade in
// place, and the diff has to be tested against a grade that really moved
// rather than one a test asserted had.
func regrade(t *testing.T, h *harness, nodeID, grade string) {
	t.Helper()
	node, err := h.srv.db.Graph().GetNode(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("read %s: %v", nodeID, err)
	}
	if node.Properties == nil {
		node.Properties = map[string]any{}
	}
	node.Properties[cortexdb.KeyGrade] = grade
	// ValidFrom is the one temporal column a write may set, and GetNode
	// returned the one this fact already had. Writing it back unchanged would
	// say the corrected version has been true since the original load, which
	// leaves both versions visible at every instant in between — so the
	// correction becomes true at the moment it is made, which is what a
	// correction is.
	node.ValidFrom = time.Time{}
	if err := h.srv.db.Graph().UpsertNode(context.Background(), node); err != nil {
		t.Fatalf("regrade %s: %v", nodeID, err)
	}
	// The graph versions on a clock, and two writes inside one microsecond are
	// two facts an as-of read cannot put on either side of an instant between
	// them.
	time.Sleep(3 * time.Millisecond)
}

// aLoadedFact is the id of one node the fake pipeline's result put in the
// brain, so a test can move its grade and watch a diff notice.
func aLoadedFact(t *testing.T, h *harness) string {
	t.Helper()
	nodes, err := h.srv.db.Graph().ListNodes(context.Background(), &graph.GraphFilter{NodeTypes: []string{"Node"}})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatal("the load put no facts in the brain to move")
	}
	return nodes[0].ID
}

func TestAMomentIsTakenNamedAndMirroredIntoTheLedger(t *testing.T) {
	h := newHarness(t, &fakeRunner{result: cannedResult()})
	jobID := uploadAndCreate(t, h)

	// A vocabulary in force and a load that landed: the two things a snapshot
	// says it rests on.
	if code, body := h.do(http.MethodPost, "/athanor/ontologies", "op-secret", testOntology); code != http.StatusCreated {
		t.Fatalf("draft: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, path("sds-demo@1", "publish"), "op-secret", `{"by":"liliang"}`); code != http.StatusOK {
		t.Fatalf("publish: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, "/athanor/loads", "op-secret", `{"job":"`+jobID+`","load":"march"}`); code != http.StatusOK {
		t.Fatalf("load: %d %s", code, body)
	}

	answer := h.take(t, "march-end", "op-secret", `{"name":"march-end","by":"liliang","note":"before the migration"}`)
	if msg, failed := answer["ledger_error"]; failed {
		t.Fatalf("the snapshot recorded no decision: %v", msg)
	}
	snap := decodeSnapshot(t, answer)
	if snap.Name != "march-end" || snap.Actor != "operator" || snap.By != "liliang" {
		t.Fatalf("the moment landed as %+v", snap)
	}
	if snap.Counts.Nodes == 0 || snap.Grades.Asserted.Nodes == 0 {
		t.Fatalf("the moment counted nothing: %+v", snap)
	}
	// What it rests on: the published vocabulary and the load's ledger entry.
	if got := snap.Footing.Ontologies["sds-demo"]; got != "sds-demo@1" {
		t.Fatalf("the snapshot does not name the vocabulary in force: %+v", snap.Footing)
	}
	if len(snap.Footing.Loads) != 1 || !strings.Contains(snap.Footing.Loads[0], "march") {
		t.Fatalf("the snapshot does not name the load it rests on: %+v", snap.Footing)
	}

	// The ledger carries it, under the key id, with the name typed into the
	// body kept beside it.
	entries := h.decisions(t, "?kind=snapshot.take", "op-secret")
	if len(entries.Decisions) != 1 {
		t.Fatalf("snapshot.take entries = %d, want 1", len(entries.Decisions))
	}
	entry := entries.Decisions[0]
	if entry.Actor != "operator" {
		t.Errorf("actor = %q, want the key id", entry.Actor)
	}
	if !strings.Contains(entry.Note, "march-end") {
		t.Errorf("the entry does not name the moment: %s", entry.Note)
	}
	if !strings.Contains(entry.Note, "liliang") {
		t.Errorf("the name it was signed with is missing: %s", entry.Note)
	}
	// The instant is in the entry to the nanosecond it was stored at, because
	// it is what a caller hands to a past read.
	if !strings.Contains(entry.Note, snap.At.Format(time.RFC3339Nano)) {
		t.Errorf("the entry does not carry the instant %s: %s", snap.At.Format(time.RFC3339Nano), entry.Note)
	}
	if answer["decision"] != cortexdb.DecisionID(snapshotDecisionID("march-end")) {
		t.Errorf("the answer names decision %v", answer["decision"])
	}
}

func TestAMomentNobodyIsNamedForIsRefusedAtTheDoor(t *testing.T) {
	h := newHarness(t, &fakeRunner{result: cannedResult()})

	for _, body := range []string{
		`{"name":"nameless"}`,
		`{"name":"nameless","by":""}`,
		`{"name":"nameless","by":"   "}`,
	} {
		if code, answer := h.do(http.MethodPost, "/athanor/snapshots", "op-secret", body); code != http.StatusBadRequest {
			t.Fatalf("an unsigned snapshot was taken: %d %s", code, answer)
		}
	}
	// And nothing was recorded by any of it.
	if got := h.decisions(t, "?kind=snapshot.take", "op-secret"); len(got.Decisions) != 0 {
		t.Fatalf("a refused snapshot reached the ledger: %+v", got.Decisions)
	}
	if code, _ := h.do(http.MethodGet, snapshotPath("nameless", ""), "op-secret", ""); code != http.StatusNotFound {
		t.Fatalf("a refused snapshot left a moment behind: %d", code)
	}
	// A drop nobody signed is refused the same way.
	if code, body := h.do(http.MethodPost, "/athanor/snapshots", "op-secret", `{"name":"real","by":"liliang"}`); code != http.StatusCreated {
		t.Fatalf("take: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, snapshotPath("real", "drop"), "op-secret", `{}`); code != http.StatusBadRequest {
		t.Fatalf("an unsigned drop was accepted: %d %s", code, body)
	}
}

func TestAReaderMayReadEveryMomentAndTakeNone(t *testing.T) {
	h := newHarness(t, &fakeRunner{result: cannedResult()})
	if code, body := h.do(http.MethodPost, "/athanor/snapshots", "op-secret", `{"name":"one","by":"liliang"}`); code != http.StatusCreated {
		t.Fatalf("take: %d %s", code, body)
	}

	writes := []struct{ path, body string }{
		{"/athanor/snapshots", `{"name":"two","by":"liliang"}`},
		{snapshotPath("one", "drop"), `{"by":"liliang"}`},
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
		"/athanor/snapshots",
		snapshotPath("one", ""),
		"/athanor/snapshots/diff?from=one&to=one",
	}
	for _, p := range reads {
		if code, body := h.do(http.MethodGet, p, "ro-secret", ""); code != http.StatusOK {
			t.Fatalf("a reader was refused %s: %d %s", p, code, body)
		}
		if code, _ := h.do(http.MethodGet, p, "", ""); code != http.StatusUnauthorized {
			t.Fatalf("%s answered a caller with no key", p)
		}
	}
}

func TestRetakingANameUpdatesOneLedgerEntry(t *testing.T) {
	h := newHarness(t, &fakeRunner{result: cannedResult()})

	first := decodeSnapshot(t, h.take(t, "nightly", "op-secret", `{"name":"nightly","by":"liliang"}`))
	// Without `replace` a name is one moment, so the second take is refused
	// rather than silently re-pointing the name at this afternoon.
	if code, body := h.do(http.MethodPost, "/athanor/snapshots", "op-secret", `{"name":"nightly","by":"liliang"}`); code != http.StatusConflict {
		t.Fatalf("one name became two moments: %d %s", code, body)
	}
	time.Sleep(3 * time.Millisecond)
	second := decodeSnapshot(t, h.take(t, "nightly", "op-secret", `{"name":"nightly","by":"liliang","replace":true}`))
	if !second.At.After(first.At) {
		t.Fatalf("replacing did not move the moment: %s then %s", first.At, second.At)
	}

	// The entry id is derived from the name, so the ledger holds one entry for
	// this moment rather than two claiming it was taken twice.
	got := h.decisions(t, "?kind=snapshot.take", "op-secret")
	if len(got.Decisions) != 1 {
		t.Fatalf("re-taking a name grew the ledger to %d entries: %+v", len(got.Decisions), got.Decisions)
	}
	if !strings.Contains(got.Decisions[0].Note, second.At.Format(time.RFC3339Nano)) {
		t.Fatalf("the one entry still describes the moment that was replaced: %s", got.Decisions[0].Note)
	}
}

func TestDroppingRetiresTheNameAndKeepsTheRecord(t *testing.T) {
	h := newHarness(t, &fakeRunner{result: cannedResult()})
	taken := decodeSnapshot(t, h.take(t, "scratch", "op-secret", `{"name":"scratch","by":"liliang"}`))

	code, body := h.do(http.MethodPost, snapshotPath("scratch", "drop"), "op-secret", `{"by":"liliang","note":"taken by mistake"}`)
	if code != http.StatusOK {
		t.Fatalf("drop: %d %s", code, body)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("drop answer: %v (%s)", err, body)
	}
	if msg, failed := answer["ledger_error"]; failed {
		t.Fatalf("the drop recorded no decision: %v", msg)
	}
	dropped := decodeSnapshot(t, answer)
	if dropped.State != snapshots.Dropped || dropped.DroppedBy != "liliang" {
		t.Fatalf("the drop was not recorded: %+v", dropped)
	}
	if !dropped.At.Equal(taken.At) {
		t.Fatalf("dropping moved the moment: %s then %s", taken.At, dropped.At)
	}

	// The moment is off the list and still readable, and the two acts are two
	// entries — a drop that overwrote the take would lose what was counted.
	list := h.snapshotList(t, "", "op-secret")
	if len(list) != 0 {
		t.Fatalf("a dropped moment is still offered: %+v", list)
	}
	if len(h.snapshotList(t, "?state=dropped", "op-secret")) != 1 {
		t.Fatalf("a dropped moment cannot be found at all")
	}
	if code, _ := h.do(http.MethodGet, snapshotPath("scratch", ""), "op-secret", ""); code != http.StatusOK {
		t.Fatalf("a dropped moment is gone: %d", code)
	}
	if got := h.decisions(t, "?kind=snapshot.take", "op-secret"); len(got.Decisions) != 1 {
		t.Fatalf("the take entry did not survive the drop: %+v", got.Decisions)
	}
	if got := h.decisions(t, "?kind=snapshot.drop", "op-secret"); len(got.Decisions) != 1 {
		t.Fatalf("snapshot.drop entries = %d, want 1", len(got.Decisions))
	}
}

func TestADiffSaysWhatFellOffTheLadder(t *testing.T) {
	h := newHarness(t, &fakeRunner{result: cannedResult()})
	jobID := uploadAndCreate(t, h)
	if code, body := h.do(http.MethodPost, "/athanor/loads", "op-secret", `{"job":"`+jobID+`"}`); code != http.StatusOK {
		t.Fatalf("load: %d %s", code, body)
	}
	fact := aLoadedFact(t, h)

	// Somebody checked it.
	regrade(t, h, fact, cortexdb.GradeVerified)
	before := decodeSnapshot(t, h.take(t, "before", "op-secret", `{"name":"before","by":"liliang"}`))
	time.Sleep(3 * time.Millisecond)
	// And then a later run put it back to what a model said.
	regrade(t, h, fact, cortexdb.GradeAsserted)
	after := decodeSnapshot(t, h.take(t, "after", "op-secret", `{"name":"after","by":"liliang"}`))

	code, body := h.do(http.MethodGet, "/athanor/snapshots/diff?from=before&to=after&limit=200", "op-secret", "")
	if code != http.StatusOK {
		t.Fatalf("diff: %d %s", code, body)
	}
	var diff snapshots.Diff
	if err := json.Unmarshal([]byte(body), &diff); err != nil {
		t.Fatalf("diff: %v (%s)", err, body)
	}
	if diff.From.Name != before.Name || diff.To.Name != after.Name {
		t.Fatalf("the diff names %s..%s", diff.From.Name, diff.To.Name)
	}
	var moved snapshots.Change
	for _, c := range diff.Changes {
		if c.ID == fact {
			moved = c
		}
	}
	if moved.Kind != snapshots.Regraded {
		t.Fatalf("a fact that stopped being verified reads as %q: %+v", moved.Kind, moved)
	}
	if moved.WasGrade != cortexdb.GradeVerified || moved.NowGrade != cortexdb.GradeAsserted || !moved.Fell {
		t.Fatalf("the fall was not reported: %+v", moved)
	}
	if diff.Counts.Fell != 1 {
		t.Fatalf("the summary counted %d falls, want 1: %+v", diff.Counts.Fell, diff.Counts)
	}
	// The two ends carry their own ladders, so the movement is readable
	// without opening a single change: one more fact rests on nothing but a
	// model than did at the earlier moment.
	//
	// The count of verified records does not fall by one, and that is not a
	// bug to fix here: the decision ledger lives in the same graph, every
	// entry is a node a named actor signed, and the snapshot taken at the
	// earlier moment wrote one. A tally of this brain has always counted the
	// ledger — the front page's does too — and a snapshot that quietly
	// excluded it would disagree with contract_tally.
	if diff.To.Grades.Asserted.Nodes != diff.From.Grades.Asserted.Nodes+1 {
		t.Fatalf("the shelf did not gain an asserted fact: %+v then %+v", diff.From.Grades, diff.To.Grades)
	}

	// A diff runs forwards, and a moment nobody named is not a diff.
	if code, _ := h.do(http.MethodGet, "/athanor/snapshots/diff?from=after&to=before", "op-secret", ""); code != http.StatusBadRequest {
		t.Fatalf("a backwards diff was answered: %d", code)
	}
	if code, _ := h.do(http.MethodGet, "/athanor/snapshots/diff?from=never&to=after", "op-secret", ""); code != http.StatusNotFound {
		t.Fatalf("a diff against a moment nobody named: %d", code)
	}
	if code, _ := h.do(http.MethodGet, "/athanor/snapshots/diff?from=before", "op-secret", ""); code != http.StatusBadRequest {
		t.Fatalf("a diff with one end: %d", code)
	}
}

func TestASnapshotSurvivesALedgerWriteThatFails(t *testing.T) {
	h := newHarness(t, &fakeRunner{result: cannedResult()})
	h.srv.ledger = brokenLedger{}

	code, body := h.do(http.MethodPost, "/athanor/snapshots", "op-secret", `{"name":"march","by":"liliang"}`)
	if code != http.StatusCreated {
		t.Fatalf("a ledger failure undid the snapshot: %d %s", code, body)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("take answer: %v (%s)", err, body)
	}
	if answer["ledger_error"] == nil {
		t.Fatalf("the response says nothing about the ledger failure: %s", body)
	}
	// The moment is named regardless — naming it is what succeeded.
	if code, read := h.do(http.MethodGet, snapshotPath("march", ""), "op-secret", ""); code != http.StatusOK {
		t.Fatalf("the moment was rolled back with its record: %d %s", code, read)
	}
	// And a drop whose record fails leaves the name retired.
	code, body = h.do(http.MethodPost, snapshotPath("march", "drop"), "op-secret", `{"by":"liliang"}`)
	if code != http.StatusOK {
		t.Fatalf("a ledger failure undid the drop: %d %s", code, body)
	}
	if !strings.Contains(body, "ledger_error") {
		t.Fatalf("the drop says nothing about the ledger failure: %s", body)
	}
}

func TestAConfinedKeySeesOnlyTheMomentsItSigned(t *testing.T) {
	h := newConfinedHarness(t)

	if code, body := h.do(http.MethodPost, "/athanor/snapshots", "hermes-secret", `{"name":"hermes-moment","by":"hermes"}`); code != http.StatusCreated {
		t.Fatalf("confined take: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, "/athanor/snapshots", "op-secret", `{"name":"operator-moment","by":"liliang"}`); code != http.StatusCreated {
		t.Fatalf("take: %d %s", code, body)
	}

	if got := h.snapshotList(t, "", "op-secret"); len(got) != 2 {
		t.Fatalf("the operator sees %d moments, want 2", len(got))
	}
	confined := h.snapshotList(t, "", "hermes-secret")
	if len(confined) != 1 || confined[0].Actor != "hermes" {
		t.Fatalf("a confined key saw somebody else's moment: %+v", confined)
	}
	// Somebody else's moment answers exactly as one that was never taken —
	// same status and the same bytes, so the two cannot be told apart.
	code, forbidden := h.do(http.MethodGet, snapshotPath("operator-moment", ""), "hermes-secret", "")
	missing, absent := h.do(http.MethodGet, snapshotPath("never-taken", ""), "hermes-secret", "")
	if code != http.StatusNotFound || missing != code || forbidden != absent {
		t.Fatalf("forbidden and missing answer differently:\n forbidden %d %s\n missing   %d %s",
			code, forbidden, missing, absent)
	}
	// Nor can it be dropped or diffed against.
	if code, body := h.do(http.MethodPost, snapshotPath("operator-moment", "drop"), "hermes-secret", `{"by":"hermes"}`); code != http.StatusNotFound {
		t.Fatalf("a confined key dropped somebody else's moment: %d %s", code, body)
	}
	if code, body := h.do(http.MethodGet, "/athanor/snapshots/diff?from=hermes-moment&to=operator-moment", "hermes-secret", ""); code != http.StatusNotFound {
		t.Fatalf("a confined key diffed against somebody else's moment: %d %s", code, body)
	}
}

// snapshotList reads GET /athanor/snapshots.
func (h *harness) snapshotList(t *testing.T, query, bearer string) []snapshots.Snapshot {
	t.Helper()
	code, body := h.do(http.MethodGet, "/athanor/snapshots"+query, bearer, "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	var out struct {
		Snapshots []snapshots.Snapshot `json:"snapshots"`
		Count     int                  `json:"count"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("list: %v (%s)", err, body)
	}
	if out.Count != len(out.Snapshots) {
		t.Fatalf("the listing counts %d and carries %d", out.Count, len(out.Snapshots))
	}
	return out.Snapshots
}
