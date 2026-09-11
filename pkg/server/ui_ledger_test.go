package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	alchemyv1 "github.com/liliang-cn/alchemy/proto/alchemy/v1"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// The two ledger screens are tested the way a browser meets them: a GET with
// a cookie, and an assertion about what is on the page. What they must agree
// with is the JSON route beside them — ledger_test.go asserts the same
// confinement against /athanor/decisions, and the point of these is that the
// page cannot drift away from it.

// browse is a GET as a signed-in browser makes it: the key in the cookie the
// sign-in form sets, and redirects left unfollowed, because whether an
// unsigned browser is sent to the door is the assertion in one of these.
func (h *harness) browse(t *testing.T, path, secret string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+h.httpAddr+path, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	if secret != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: secret})
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther || resp.StatusCode == http.StatusFound {
		return resp.StatusCode, resp.Header.Get("Location")
	}
	return resp.StatusCode, string(body)
}

// loadDecision performs a load and reports the ledger entry it wrote.
func loadDecision(t *testing.T, h *harness, secret, job, load string) string {
	t.Helper()
	code, body := h.do(http.MethodPost, "/athanor/loads", secret, `{"job":"`+job+`","load":"`+load+`"}`)
	if code != http.StatusOK {
		t.Fatalf("load %s: %d %s", load, code, body)
	}
	var answer struct {
		Decision    string `json:"decision"`
		LedgerError string `json:"ledger_error"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("load answer: %v (%s)", err, body)
	}
	if answer.LedgerError != "" || answer.Decision == "" {
		t.Fatalf("the load recorded no decision: %s", body)
	}
	return answer.Decision
}

func TestTheLedgerScreensSendASignedOutBrowserToTheDoor(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})

	for _, path := range []string{"/app/decisions", "/app/decisions/decision:athanor:load:a:b"} {
		code, location := h.browse(t, path, "")
		if code != http.StatusSeeOther {
			t.Errorf("%s answered %d, want a redirect to the sign-in form — a page is not an API", path, code)
		}
		if !strings.HasPrefix(location, "/?") {
			t.Errorf("%s sent the browser to %q, want the front page", path, location)
		}
	}
}

func TestAReadOnlyKeyReadsBothLedgerScreens(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	decision := loadDecision(t, h, "op-secret", uploadAndCreate(t, h), "march")

	code, page := h.browse(t, "/app/decisions", "ro-secret")
	if code != http.StatusOK {
		t.Fatalf("a reader could not open the ledger: %d %s", code, page)
	}
	if !strings.Contains(page, "march") {
		t.Errorf("the list does not show the entry: %s", page)
	}
	code, page = h.browse(t, "/app/decisions/"+url.PathEscape(decision), "ro-secret")
	if code != http.StatusOK {
		t.Fatalf("a reader could not open the chain: %d %s", code, page)
	}
	if !strings.Contains(page, decision) {
		t.Errorf("the chain page does not name the entry: %s", page)
	}
}

func TestTheLedgerListShowsEveryKindAndTheKindFilterNarrowsIt(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	loadDecision(t, h, "op-secret", uploadAndCreate(t, h), "march")
	// An agent's own decision, recorded the way an agent records one, so the
	// list is proved to hold more than Athanor's own acts.
	if _, err := h.srv.db.RecordDecision(context.Background(), cortexdb.DecisionRecordRequest{
		Kind: cortexdb.DecisionKindAction, Actor: "operator", Verdict: "held",
		Note: "held the release",
	}); err != nil {
		t.Fatalf("agent decision: %v", err)
	}

	code, page := h.browse(t, "/app/decisions", "op-secret")
	if code != http.StatusOK {
		t.Fatalf("decisions: %d %s", code, page)
	}
	for _, want := range []string{"march", "held the release", "/app/decisions/decision:athanor:load:"} {
		if !strings.Contains(page, want) {
			t.Errorf("the ledger list does not show %q", want)
		}
	}
	// The verdict is a pill, not a column of grades — a decision is verified
	// by construction, so the grade would say the same thing on every row.
	if !strings.Contains(page, `<span class="pill `) {
		t.Errorf("no verdict pill on the list: %s", page)
	}

	code, filtered := h.browse(t, "/app/decisions?kind=load", "op-secret")
	if code != http.StatusOK {
		t.Fatalf("filtered: %d %s", code, filtered)
	}
	if !strings.Contains(filtered, "march") {
		t.Errorf("?kind=load dropped the load: %s", filtered)
	}
	if strings.Contains(filtered, "held the release") {
		t.Errorf("?kind=load kept an action entry: %s", filtered)
	}
}

func TestTheChainPageLinksToThePremiseItRestsOn(t *testing.T) {
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
	decision := loadDecision(t, h, "op-secret", jobID, "march")

	premise := cortexdb.DecisionID(reviewDecisionID(jobID, item.GetId()))
	code, page := h.browse(t, "/app/decisions/"+url.PathEscape(decision), "op-secret")
	if code != http.StatusOK {
		t.Fatalf("chain: %d %s", code, page)
	}
	if !strings.Contains(page, `href="/app/decisions/`+url.PathEscape(premise)+`"`) {
		t.Fatalf("the chain page does not link to the review it rests on (%s): %s", premise, page)
	}
	// The premise is labelled with the sentence the review was written as
	// rather than with its id, and it carries its grade, which is the whole
	// reason a chain lists premises at all.
	chain, err := h.srv.db.DecisionChain(context.Background(), premise, 0)
	if err != nil || len(chain.Decisions) == 0 {
		t.Fatalf("the review entry: %v %+v", err, chain)
	}
	line := ledgerLine(chain.Decisions[0].Note)
	if line == "" || !strings.Contains(page, line) {
		t.Errorf("the premise is shown as a bare id, not as %q: %s", line, page)
	}
	if !strings.Contains(page, `class="g-verified">verified`) {
		t.Errorf("the premise's grade is missing: %s", page)
	}
	// And the premise's own page opens, with the reviewer's own words on it.
	code, premisePage := h.browse(t, "/app/decisions/"+url.PathEscape(premise), "op-secret")
	if code != http.StatusOK {
		t.Fatalf("the premise's own page: %d %s", code, premisePage)
	}
	if !strings.Contains(premisePage, "hp really is the primary") {
		t.Errorf("the reviewer's note is not on the review's own page: %s", premisePage)
	}
}

func TestTheChainPageRendersTheStructuredDetailAsFields(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	jobID := uploadAndCreate(t, h)
	decision := loadDecision(t, h, "op-secret", jobID, "march")

	code, page := h.browse(t, "/app/decisions/"+url.PathEscape(decision), "op-secret")
	if code != http.StatusOK {
		t.Fatalf("chain: %d %s", code, page)
	}
	// The load's detail keys, each its own row, and the job's id as a value.
	for _, want := range []string{"<th>entities</th>", "<th>job</th>", "<th>load</th>", "<td>march</td>"} {
		if !strings.Contains(page, want) {
			t.Errorf("the detail is not rendered as fields — %q is missing: %s", want, page)
		}
	}
	// And not as the line the ledger stored it on. The note's JSON has no
	// space after the colon; html/template would escape the quotes, so look
	// for the escaped form too.
	for _, unwanted := range []string{`{"chunks":`, `{&#34;chunks&#34;:`} {
		if strings.Contains(page, unwanted) {
			t.Errorf("the raw JSON detail was dumped onto the page: %s", page)
		}
	}
}

func TestAConfinedKeySeesOnlyItsOwnEntriesOnTheLedgerScreens(t *testing.T) {
	h := newConfinedHarness(t)
	loadDecision(t, h, "hermes-secret", uploadAndCreate(t, h), "hermes-load")
	theirs := loadDecision(t, h, "op-secret", uploadAndCreate(t, h), "operator-load")

	code, page := h.browse(t, "/app/decisions", "hermes-secret")
	if code != http.StatusOK {
		t.Fatalf("confined list: %d %s", code, page)
	}
	if !strings.Contains(page, "hermes-load") {
		t.Errorf("a confined key cannot see its own entries: %s", page)
	}
	if strings.Contains(page, "operator-load") {
		t.Fatalf("a confined key saw somebody else's ledger: %s", page)
	}
	// The operator sees both, so the absence above is confinement and not an
	// empty ledger.
	if _, all := h.browse(t, "/app/decisions", "op-secret"); !strings.Contains(all, "operator-load") ||
		!strings.Contains(all, "hermes-load") {
		t.Fatalf("the operator does not see both loads: %s", all)
	}
	// Asking for another actor by hand answers nothing rather than quietly
	// answering with the caller's own entries.
	if _, asked := h.browse(t, "/app/decisions?actor=operator", "hermes-secret"); strings.Contains(asked, "hermes-load") ||
		strings.Contains(asked, "operator-load") {
		t.Fatalf("?actor= let a confined key past its confinement: %s", asked)
	}

	// And the chain of an entry that is not theirs answers exactly as one that
	// does not exist — the same status and the same page.
	code, refused := h.browse(t, "/app/decisions/"+url.PathEscape(theirs), "hermes-secret")
	if code != http.StatusNotFound {
		t.Fatalf("a confined key opened somebody else's chain: %d %s", code, refused)
	}
	missing, absent := h.browse(t, "/app/decisions/decision:athanor:load:nobody:nothing", "hermes-secret")
	if missing != code || absent != refused {
		t.Fatalf("forbidden and missing answer differently:\n forbidden %d\n missing   %d", code, missing)
	}
}

func TestAnIdThatNamesNothingRendersTheShellAndNotAFailure(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})

	code, page := h.browse(t, "/app/decisions/decision:athanor:load:nobody:nothing", "op-secret")
	if code != http.StatusNotFound {
		t.Fatalf("an unknown id answered %d, want 404: %s", code, page)
	}
	if !strings.Contains(page, notFoundDecision) {
		t.Errorf("the page does not say the entry is missing: %s", page)
	}
	// Still the shell, so the reader can go somewhere from here.
	for _, want := range []string{`class="rail"`, `href="/app/decisions"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the not-found page is not drawn in the shell — %q is missing", want)
		}
	}
}

func TestTheEmptyLedgerSaysSo(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})

	code, page := h.browse(t, "/app/decisions", "op-secret")
	if code != http.StatusOK {
		t.Fatalf("decisions: %d %s", code, page)
	}
	if !strings.Contains(page, "The ledger holds nothing yet") {
		t.Errorf("an empty ledger rendered as a blank table: %s", page)
	}
}
