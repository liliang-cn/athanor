package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/liliang-cn/alchemy/pkg/alchemy"
	"github.com/liliang-cn/athanor/pkg/ontologies"
)

// The vocabulary the fake pipeline is asked to read a corpus under, and the
// two types its run says the corpus wanted and this vocabulary lacks. The
// entity comes first because Extend refuses a relation whose ends are
// themselves undeclared, which is the ordering rule the propose → approve
// round is for.
const ontologyDoc = `{"id":"sds-demo@1","parts":{"prose":{
  "entities":[{"name":"Node"},{"name":"DRBDResource"}],
  "relations":[{"name":"promotes","from":["Node"],"to":["DRBDResource"],"at_most_one_in":true}]}}}`

func wantedTypes() []alchemy.Proposal {
	return []alchemy.Proposal{
		{Kind: alchemy.ProposalEntity, Type: "Cluster", Records: 4, Sources: []string{"runbook.md"}},
		{Kind: alchemy.ProposalRelation, Type: "member_of", Records: 6,
			From: []string{"Node"}, To: []string{"Cluster"}, Sources: []string{"runbook.md"}},
	}
}

// proposingResult is a finished run that wanted types the vocabulary lacks.
func proposingResult() alchemy.Result {
	r := cannedResult()
	r.Proposals = wantedTypes()
	return r
}

func decodeVersion(t *testing.T, resp *http.Response, want int) ontologies.Version {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("%s answered %d, want %d: %s", resp.Request.URL.Path, resp.StatusCode, want, body)
	}
	var v ontologies.Version
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("answer: %v (%s)", err, body)
	}
	return v
}

// do is the harness's http with the two lines every assertion here would
// otherwise repeat: fail on a transport error, read and close the body.
func (h *harness) do(method, path, bearer, body string) (int, string) {
	h.t.Helper()
	resp, err := h.http(method, path, bearer, body)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	answer, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(answer)
}

// path escapes an id for a URL. An id carries an "@" and an act carries a ":",
// both legal in a path segment; this is here so a test cannot pass by
// accidentally agreeing with the handler about escaping.
func path(id, verb string) string {
	seg := id
	if verb != "" {
		seg += ":" + verb
	}
	return "/athanor/ontologies/" + url.PathEscape(seg)
}

func TestAVocabularyIsDraftedProposedApprovedAndPublished(t *testing.T) {
	h := newHarness(t, fakeRunner{result: proposingResult()})
	jobID := uploadAndCreate(t, h)

	// 1. a draft: the document, validated by alchemy's own loader.
	resp, err := h.http(http.MethodPost, "/athanor/ontologies?note=the+first+one", "op-secret", ontologyDoc)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	v := decodeVersion(t, resp, http.StatusCreated)
	if v.ID != "sds-demo@1" || v.State != ontologies.Draft || v.Lineage != "sds-demo" {
		t.Fatalf("the draft landed as %+v", v)
	}

	// 2. proposed, from a finished run that wanted types this lacks.
	resp, err = h.http(http.MethodPost, path("sds-demo@1", "propose"), "op-secret", `{"job":"`+jobID+`","part":"prose"}`)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	v = decodeVersion(t, resp, http.StatusOK)
	if v.State != ontologies.Proposed {
		t.Fatalf("after a proposal the draft is %q", v.State)
	}
	if len(v.Proposals) != 2 || v.ProposedFrom != jobID {
		t.Fatalf("the proposals were not recorded against the version: %+v", v)
	}

	// It is current before it is extended, so there is something to retire.
	code, body := h.do(http.MethodPost, path("sds-demo@1", "publish"), "op-secret", `{"by":"liliang"}`)
	if code != http.StatusOK {
		t.Fatalf("publish sds-demo@1: %d %s", code, body)
	}

	// 3. approved: Extend runs, and the new version says who decided.
	resp, err = h.http(http.MethodPost, path("sds-demo@1", "approve"), "op-secret",
		`{"accept":["Cluster","member_of"],"by":"liliang","note":"both are real"}`)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	next := decodeVersion(t, resp, http.StatusCreated)
	if next.ID != "sds-demo@2" || next.Parent != "sds-demo@1" || next.State != ontologies.Approved {
		t.Fatalf("the extension landed as %+v", next)
	}
	if next.ApprovedBy != "liliang" || next.ApprovedAt == nil {
		t.Fatalf("the approval is unsigned: %+v", next)
	}
	if !strings.Contains(string(next.Document), "Cluster") {
		t.Fatalf("the accepted type is not in the new document: %s", next.Document)
	}

	// 4. published: the previous is retired and `current` is the new body.
	resp, err = h.http(http.MethodPost, path("sds-demo@2", "publish"), "op-secret", `{"by":"liliang","note":"in force"}`)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	pub, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("publish answered %d: %s", resp.StatusCode, pub)
	}
	var published struct {
		Version ontologies.Version `json:"version"`
		Retired string             `json:"retired"`
		By      string             `json:"by"`
	}
	if err := json.Unmarshal(pub, &published); err != nil {
		t.Fatalf("publish answer: %v (%s)", err, pub)
	}
	if published.Retired != "sds-demo@1" || published.By != "liliang" {
		t.Fatalf("publishing sds-demo@2 reported %+v", published)
	}

	code, body = h.do(http.MethodGet, path("sds-demo@1", ""), "op-secret", "")
	if code != http.StatusOK || !strings.Contains(body, string(ontologies.Retired)) {
		t.Fatalf("sds-demo@1 is not retired: %d %s", code, body)
	}

	// `current` is the JSON a client pastes into CreateJob: the document, and
	// nothing wrapped around it.
	resp, err = h.http(http.MethodGet, "/athanor/ontologies/current?lineage=sds-demo", "op-secret", "")
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	current, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("current answered %d: %s", resp.StatusCode, current)
	}
	if got := resp.Header.Get("Athanor-Ontology-Version"); got != "sds-demo@2" {
		t.Fatalf("current names version %q", got)
	}
	var doc struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(current, &doc); err != nil || doc.ID != "sds-demo@2" {
		t.Fatalf("current is not a pasteable document: %v %s", err, current)
	}
	if !strings.Contains(string(current), "Cluster") || !strings.Contains(string(current), "member_of") {
		t.Fatalf("current does not carry the accepted types: %s", current)
	}

	// It is a document alchemy accepts: the loop closes.
	if _, err := h.http(http.MethodPost, "/athanor/ontologies", "op-secret", string(current)); err != nil {
		t.Fatalf("re-drafting the current document: %v", err)
	}
}

func TestAHeldJobCannotBeProposedFrom(t *testing.T) {
	held := proposingResult()
	held.Conflicts = []alchemy.Conflict{{
		Kind: alchemy.ConflictCardinality, Subject: "drbdresource:sds-meta",
		Detail: "two nodes claim to promote it",
	}}
	h := newHarness(t, fakeRunner{result: held})
	jobID := uploadAndCreate(t, h)

	if code, body := h.do(http.MethodPost, "/athanor/ontologies", "op-secret", ontologyDoc); code != http.StatusCreated {
		t.Fatalf("draft: %d %s", code, body)
	}
	code, body := h.do(http.MethodPost, path("sds-demo@1", "propose"), "op-secret", `{"job":"`+jobID+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("a held job was proposed from: %d %s", code, body)
	}
	if lower := strings.ToLower(body); !strings.Contains(lower, "held") && !strings.Contains(lower, "review") {
		t.Fatalf("the refusal should say the job is held: %s", body)
	}
	// Nothing was recorded against the version.
	code, body = h.do(http.MethodGet, path("sds-demo@1", ""), "op-secret", "")
	if code != http.StatusOK || strings.Contains(body, "proposals") {
		t.Fatalf("a held job's proposals were recorded: %d %s", code, body)
	}

	// An unknown job is a 404, not a proposal against nothing.
	if code, _ := h.do(http.MethodPost, path("sds-demo@1", "propose"), "op-secret", `{"job":"never"}`); code != http.StatusNotFound {
		t.Fatalf("an unknown job: %d", code)
	}
}

func TestAnApprovalNobodyIsNamedForIsRefused(t *testing.T) {
	h := newHarness(t, fakeRunner{result: proposingResult()})
	jobID := uploadAndCreate(t, h)

	if code, body := h.do(http.MethodPost, "/athanor/ontologies", "op-secret", ontologyDoc); code != http.StatusCreated {
		t.Fatalf("draft: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, path("sds-demo@1", "propose"), "op-secret", `{"job":"`+jobID+`"}`); code != http.StatusOK {
		t.Fatalf("propose: %d %s", code, body)
	}
	for _, by := range []string{`""`, `"   "`} {
		code, body := h.do(http.MethodPost, path("sds-demo@1", "approve"), "op-secret",
			`{"accept":["Cluster"],"by":`+by+`}`)
		if code != http.StatusBadRequest {
			t.Fatalf("an approval by %s was accepted: %d %s", by, code, body)
		}
	}
	// And a type nobody proposed is refused rather than silently ignored.
	if code, body := h.do(http.MethodPost, path("sds-demo@1", "approve"), "op-secret",
		`{"accept":["Rack"],"by":"liliang"}`); code != http.StatusBadRequest {
		t.Fatalf("a type nobody proposed was accepted: %d %s", code, body)
	}
	// Nothing was created by any of that.
	if code, _ := h.do(http.MethodGet, path("sds-demo@2", ""), "op-secret", ""); code != http.StatusNotFound {
		t.Fatalf("a refused approval left a version behind: %d", code)
	}
}

func TestAReaderMayLookAtEveryVocabularyAndChangeNone(t *testing.T) {
	h := newHarness(t, fakeRunner{result: proposingResult()})
	jobID := uploadAndCreate(t, h)

	if code, body := h.do(http.MethodPost, "/athanor/ontologies", "op-secret", ontologyDoc); code != http.StatusCreated {
		t.Fatalf("draft: %d %s", code, body)
	}
	if code, _ := h.do(http.MethodPost, path("sds-demo@1", "propose"), "op-secret", `{"job":"`+jobID+`"}`); code != http.StatusOK {
		t.Fatalf("propose: %d", code)
	}
	if code, _ := h.do(http.MethodPost, path("sds-demo@1", "publish"), "op-secret", `{"by":"liliang"}`); code != http.StatusOK {
		t.Fatalf("publish: %d", code)
	}

	writes := []struct{ path, body string }{
		{"/athanor/ontologies", ontologyDoc},
		{path("sds-demo@1", "propose"), `{"job":"` + jobID + `"}`},
		{path("sds-demo@1", "approve"), `{"accept":["Cluster"],"by":"liliang"}`},
		{path("sds-demo@1", "publish"), `{"by":"liliang"}`},
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
		"/athanor/ontologies",
		path("sds-demo@1", ""),
		"/athanor/ontologies/current?lineage=sds-demo",
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

func TestTwoLineagesArePublishedIndependently(t *testing.T) {
	h := newHarness(t, fakeRunner{result: proposingResult()})
	const codeDoc = `{"id":"code@1","parts":{"code":{"entities":[{"name":"File"}]}}}`
	const codeTwo = `{"id":"code@2","parts":{"code":{"entities":[{"name":"File"},{"name":"Package"}]}}}`

	for _, doc := range []string{ontologyDoc, codeDoc, codeTwo} {
		if code, body := h.do(http.MethodPost, "/athanor/ontologies", "op-secret", doc); code != http.StatusCreated {
			t.Fatalf("draft: %d %s", code, body)
		}
	}
	for _, id := range []string{"sds-demo@1", "code@1"} {
		if code, body := h.do(http.MethodPost, path(id, "publish"), "op-secret", `{"by":"liliang"}`); code != http.StatusOK {
			t.Fatalf("publish %s: %d %s", id, code, body)
		}
	}
	// Publishing code@2 retires code@1 and leaves the other lineage alone.
	code, body := h.do(http.MethodPost, path("code@2", "publish"), "op-secret", `{"by":"liliang"}`)
	if code != http.StatusOK || !strings.Contains(body, `"retired":"code@1"`) {
		t.Fatalf("publish code@2: %d %s", code, body)
	}
	for lineage, want := range map[string]string{"sds-demo": "sds-demo@1", "code": "code@2"} {
		resp, err := h.http(http.MethodGet, "/athanor/ontologies/current?lineage="+lineage, "op-secret", "")
		if err != nil {
			t.Fatalf("current %s: %v", lineage, err)
		}
		got := resp.Header.Get("Athanor-Ontology-Version")
		resp.Body.Close()
		if got != want {
			t.Fatalf("current of %s is %q, want %q", lineage, got, want)
		}
	}
	// A lineage nobody published has no current version.
	if code, _ := h.do(http.MethodGet, "/athanor/ontologies/current?lineage=nothing", "op-secret", ""); code != http.StatusNotFound {
		t.Fatalf("an unpublished lineage has a current version: %d", code)
	}
	// And the list is every version of every lineage.
	resp, err := h.http(http.MethodGet, "/athanor/ontologies", "op-secret", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	listBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var listed struct {
		Versions []ontologies.Version `json:"versions"`
	}
	if err := json.Unmarshal(listBody, &listed); err != nil || len(listed.Versions) != 3 {
		t.Fatalf("list is %s (%v)", listBody, err)
	}
}
