package server

import (
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/liliang-cn/athanor/pkg/livedb"
	"github.com/liliang-cn/cortexdb/v2/pkg/connector"
)

// The import screens, and the one thing they must not do.
//
// Everything here is written from the browser's side — a cookie jar, a form
// post, a rendered page — because that is the only side on which the property
// this screen exists to keep is even visible. The API cannot leak a
// credential into a browser history; a page can.
//
// The credential is livedb_test.go's, deliberately: the JSON tests and these
// search the same string, so a leak that one of them would catch cannot be
// introduced by the other. Every assertion searches raw response bytes rather
// than a decoded field, because a leak that mattered would be one nobody
// thought to decode.

// uiPost is a browser submitting one of these forms.
func (h *harness) uiPost(t *testing.T, c *http.Client, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+h.httpAddr+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("request %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	resp, err := stopAtRedirect(c).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// importBrowser is a signed-in browser and the fake the screens act through.
func importBrowser(t *testing.T, key string) (*harness, *livedbFake, *http.Client) {
	t.Helper()
	h, f := livedbHarness(t)
	b := h.browser(t)
	if resp := h.signIn(t, b, key); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in answered %d", resp.StatusCode)
	}
	return h, f, b
}

// draftPlan is signedPlan() as it comes back from a proposal: a draft, with
// the columns and counts the review table is made of. Its Source carries the
// DSN, which the real store never does — the point being that the screen
// shows the redacted form because the door cleared the other one, not because
// the store was polite.
func draftPlan() livedb.Plan {
	plan := signedPlan()
	plan.State = livedb.Draft
	plan.Hash = "sha256:draft-hash"
	plan.CreatedBy = "operator"
	plan.Counts = livedb.Counts{
		Columns: 4, Personal: 2, Dropped: 1, Masked: 1, Generalized: 0,
		Passed: 2, Reversible: 1, UnscannedText: 1,
	}
	plan.Columns = []livedb.Treatment{{
		Table: "customers", Column: "id", Type: "integer",
		Kind: connector.PiiNone, Sensitivity: connector.Public,
		Action: connector.ActionKeep, Reason: "rule", Sample: "4711", Enters: "4711",
	}, {
		Table: "customers", Column: "email", Type: "text",
		Kind: connector.PiiEmail, Sensitivity: connector.Confidential,
		Action: connector.ActionPseudonymize, Reason: "rule", By: "rule",
		Sample: "a***@example.com", Enters: "tok_8f21",
	}, {
		Table: "customers", Column: "phone", Type: "text",
		Kind: connector.PiiPhone, Sensitivity: connector.Restricted,
		Action: connector.ActionDrop, Reason: "rule", Sample: "138****1234", Enters: "",
	}, {
		Table: "customers", Column: "notes", Type: "text",
		Kind: connector.PiiNone, Sensitivity: connector.Internal,
		Action: connector.ActionKeep, Reason: "rule", Sample: "called about the rack", Enters: "called about the rack",
	}}
	return plan
}

// leaked fails the test if the credential is anywhere in a response, in any
// of the shapes a page could carry it in.
func leaked(t *testing.T, where, body string) {
	t.Helper()
	for _, form := range []string{
		livedbDSN, livedbPassword_,
		url.QueryEscape(livedbDSN), url.QueryEscape(livedbPassword_),
	} {
		if strings.Contains(body, form) {
			t.Fatalf("%s carried the credential:\n%s", where, body)
		}
	}
}

// A browser with no cookie is sent to the one sign-in form. Not a 401: a page
// is not an API and a person cannot act on a status code.
func TestASignedOutBrowserIsSentToTheDoorFromEveryImportScreen(t *testing.T) {
	h, _ := livedbHarness(t)
	b := h.browser(t)

	for _, path := range []string{"/app/import", "/app/import/livedb", "/app/import/runs", "/app/import/follows"} {
		resp, _ := h.navigate(t, b, path)
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("GET %s answered %d, want 303", path, resp.StatusCode)
		}
		if to := resp.Header.Get("Location"); !strings.HasPrefix(to, "/?") {
			t.Errorf("GET %s pointed at %q, want the sign-in form", path, to)
		}
	}
	resp, _ := h.uiPost(t, b, "/app/import/livedb", url.Values{"act": {"propose"}, "dsn": {livedbDSN}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a signed-out propose answered %d, want 303", resp.StatusCode)
	}
	if to := resp.Header.Get("Location"); strings.Contains(to, "dsn") || strings.Contains(to, livedbPassword_) {
		t.Fatalf("the redirect carried the credential: %s", to)
	}
}

// The chooser names four kinds in the order of how checkable each one is, and
// says of each whether a model is called. That ordering is the product's case
// and belongs where somebody is choosing.
func TestTheChooserOrdersTheKindsByHowCheckableTheyAre(t *testing.T) {
	h, _, b := importBrowser(t, "op-secret")

	resp, body := h.navigate(t, b, "/app/import")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the chooser answered %d", resp.StatusCode)
	}
	var at []int
	for _, title := range []string{"A database schema (DDL)", "An existing graph", "A table (CSV, or a live database)", "Prose"} {
		i := strings.Index(body, title)
		if i < 0 {
			t.Fatalf("the chooser does not offer %q", title)
		}
		at = append(at, i)
	}
	for i := 1; i < len(at); i++ {
		if at[i] < at[i-1] {
			t.Fatalf("the kinds are out of order: a less checkable one is shown first")
		}
	}
	for _, want := range []string{
		"No model is called.",
		"A model is called only if you decline to state the mapping.",
		"A model is called per chunk, and it cannot run without a vocabulary.",
		`href="/app/import/livedb"`,
		`href="/app/import/runs"`,
		`href="/app/import/follows"`,
		"knowledge_graph_import",
		"/athanor/loads",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the chooser never says %q", want)
		}
	}
}

// A read-only key may look at the chooser and at a plan, and may perform none
// of the five acts. The refusal comes from the JSON handler the screen calls,
// which is the point: the screen and the API cannot drift about who may act
// because there is only one answer to drift from.
func TestAReadOnlyKeyMaySeeTheImportScreensAndActOnNone(t *testing.T) {
	h, f, b := importBrowser(t, "ro-secret")
	f.plan = draftPlan()

	if resp, _ := h.navigate(t, b, "/app/import"); resp.StatusCode != http.StatusOK {
		t.Fatalf("a read-only key could not see the chooser: %d", resp.StatusCode)
	}
	_, body := h.navigate(t, b, "/app/import/livedb?plan=plan-1")
	if !strings.Contains(body, "customers.email") {
		t.Fatalf("a read-only key could not read the plan:\n%s", body)
	}

	for _, act := range []struct {
		what string
		path string
		form url.Values
	}{
		{"propose", "/app/import/livedb", url.Values{"act": {"propose"}, "driver": {"postgres"}, "dsn": {livedbDSN}}},
		{"amend", "/app/import/livedb", url.Values{
			"act": {"amend"}, "plan": {"plan-1"}, "table": {"customers"}, "column": {"email"}, "action": {"drop"},
		}},
		{"sign", "/app/import/livedb", url.Values{"act": {"sign"}, "plan": {"plan-1"}, "hash": {"sha256:draft-hash"}, "by": {"reader"}}},
		{"run", "/app/import/livedb", url.Values{"act": {"run"}, "plan": {"plan-1"}, "dsn": {livedbDSN}}},
		{"follow", "/app/import/follows", url.Values{"act": {"start"}, "plan": {"plan-1"}, "dsn": {livedbDSN}}},
	} {
		resp, page := h.uiPost(t, b, act.path, act.form)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s answered %d, want the page carrying its refusal", act.what, resp.StatusCode)
		}
		if !strings.Contains(page, "(403)") {
			t.Errorf("a read-only key was not refused %s:\n%s", act.what, page)
		}
		leaked(t, act.what+" refused to a read-only key", page)
	}
	for _, call := range f.seen() {
		if call != "get" {
			t.Fatalf("a refused caller reached the store: %v", f.seen())
		}
	}
}

// The credential is in no response byte, on the way in or on the way out.
//
// Four paths: a proposal that worked, a proposal the allow-list refused, a run
// that worked and a run the store refused with the credential inside its own
// message. The last is the one a driver actually does.
func TestTheConnectionStringIsInNoResponseByte(t *testing.T) {
	h, f, b := importBrowser(t, "op-secret")
	f.plan = draftPlan()

	_, page := h.uiPost(t, b, "/app/import/livedb", url.Values{
		"act": {"propose"}, "driver": {"postgres"}, "dsn": {livedbDSN}, "tables": {"customers"},
	})
	leaked(t, "a proposal that worked", page)
	if !strings.Contains(page, "postgres://importer@db.internal:5432/crm") {
		t.Fatalf("the plan page does not show the redacted form:\n%s", page)
	}
	if strings.Contains(page, `name="dsn" value=`) {
		t.Fatalf("the page put a connection string back in an input")
	}

	// The store's own message carries the credential, which is how one really
	// escapes: a driver reports the string it was handed.
	f.plan, f.err = livedb.Plan{}, errors.New("dial postgres://importer:"+livedbPassword_+"@db.internal:5432/crm: connection refused")
	_, page = h.uiPost(t, b, "/app/import/livedb", url.Values{
		"act": {"propose"}, "driver": {"postgres"}, "dsn": {livedbDSN},
	})
	leaked(t, "a proposal the store refused in its own words", page)
	if !strings.Contains(page, livedbPlaceholder) {
		t.Fatalf("the credential was removed without saying so:\n%s", page)
	}

	f.err = nil
	f.plan = signedPlan()
	_, page = h.uiPost(t, b, "/app/import/livedb", url.Values{
		"act": {"run"}, "plan": {"plan-1"}, "dsn": {livedbDSN},
	})
	leaked(t, "a run that worked", page)

	f.err = errors.New("copy from customers: " + livedbPassword_)
	_, page = h.uiPost(t, b, "/app/import/livedb", url.Values{
		"act": {"run"}, "plan": {"plan-1"}, "dsn": {livedbDSN},
	})
	leaked(t, "a run the store refused", page)

	// The refusal that is not scrubbed on the way through the JSON layer.
	// livedbError only scrubs the message it turns into a 500; a refusal it
	// recognises — ErrWrongSource here — is passed through as the store wrote
	// it, and a store that wrote the credential into one would put it on this
	// page. The DSN carries an ampersand and an angle bracket, so the page
	// holds the HTML-escaped form and the raw string is not what to search for.
	odd := "postgres://importer:" + livedbPassword_ + "@db.internal:5432/crm?sslmode=require&x=<1>"
	f.err = fmt.Errorf("%w: %s", livedb.ErrWrongSource, odd)
	_, page = h.uiPost(t, b, "/app/import/livedb", url.Values{
		"act": {"run"}, "plan": {"plan-1"}, "dsn": {odd},
	})
	if !strings.Contains(page, "(400)") {
		t.Fatalf("the wrong-source refusal did not reach the page:\n%s", page)
	}
	for _, form := range []string{odd, template.HTMLEscapeString(odd), url.QueryEscape(odd), livedbPassword_} {
		if strings.Contains(page, form) {
			t.Fatalf("a refusal the JSON layer does not scrub carried the credential onto the page:\n%s", page)
		}
	}

	// And a follow, which holds the credential for a lifetime rather than a
	// request: its listing must name the database by the redacted form.
	f.err = nil
	f.plan = signedPlan()
	_, page = h.uiPost(t, b, "/app/import/follows", url.Values{
		"act": {"start"}, "plan": {"plan-1"}, "dsn": {livedbDSN},
	})
	leaked(t, "a follow that started", page)
}

// A connection string in the URL is refused rather than used, and the page
// that says so is rendered without it. A GET carrying one has already put it
// in a history this server cannot reach into, and the least it can do is not
// add its own copy.
func TestAConnectionStringInTheURLIsRefusedAndNotEchoed(t *testing.T) {
	h, f, b := importBrowser(t, "op-secret")

	resp, page := h.navigate(t, b, "/app/import/livedb?dsn="+url.QueryEscape(livedbDSN))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("answered %d", resp.StatusCode)
	}
	leaked(t, "a DSN passed in the query", page)
	if !strings.Contains(page, "will not read one from there") {
		t.Fatalf("the refusal does not say what happened:\n%s", page)
	}
	if calls := f.seen(); len(calls) != 0 {
		t.Fatalf("a DSN in the URL reached the store: %v", calls)
	}
}

// The plan screen leads with the counts, renders what enters the graph per
// column, and says the vault paragraph exactly when something is reversible.
func TestThePlanScreenLeadsWithTheCountsAndSaysWhatEnters(t *testing.T) {
	h, f, b := importBrowser(t, "op-secret")
	f.plan = draftPlan()

	_, page := h.navigate(t, b, "/app/import/livedb?plan=plan-1")
	for _, want := range []string{
		"columns", "personal", "dropped", "masked", "generalized", "passed", "reversible", "unscanned_text",
		"customers.email", "tok_8f21", "pseudonymize", "sha256:draft-hash",
		"vault", "a signature and not a checkbox",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the plan screen never says %q", want)
		}
	}
	counts := strings.Index(page, "unscanned_text")
	if columns := strings.Index(page, "customers.email"); counts > columns {
		t.Errorf("the counts do not lead the screen")
	}

	// No reversible treatment, no vault paragraph: it is a claim about this
	// deployment holding real personal data, and one made on a plan that holds
	// none would be false.
	plan := draftPlan()
	plan.Counts.Reversible = 0
	f.plan = plan
	_, page = h.navigate(t, b, "/app/import/livedb?plan=plan-1")
	if strings.Contains(page, "it is put in this Athanor's") {
		t.Errorf("the vault paragraph was rendered for a plan with nothing reversible")
	}
}

// An amendment posts to the amend endpoint and comes back as the plan it
// made, with the new hash the signature will have to name.
func TestATreatmentIsOverriddenThroughTheAmendEndpoint(t *testing.T) {
	h, f, b := importBrowser(t, "op-secret")
	amended := draftPlan()
	amended.Hash = "sha256:after-the-override"
	amended.Columns[1].Action = connector.ActionDrop
	amended.Columns[1].By = "operator"
	f.plan = amended

	_, page := h.uiPost(t, b, "/app/import/livedb", url.Values{
		"act": {"amend"}, "plan": {"plan-1"}, "table": {"customers"}, "column": {"email"}, "action": {"drop"},
	})
	if !strings.Contains(page, "sha256:after-the-override") {
		t.Fatalf("the amended plan's hash is not on the page:\n%s", page)
	}
	if !strings.Contains(page, "customers.email is now drop") {
		t.Errorf("the page does not say what was overridden")
	}
	seen := f.seen()
	if len(seen) == 0 || seen[0] != "amend" {
		t.Fatalf("the screen did not go through the amend endpoint: %v", seen)
	}
}

// A signature carries the hash the operator read, so a plan amended in
// another tab is refused rather than silently signed.
func TestASignatureNamesTheHashTheOperatorReadAndAStaleOneIsRefused(t *testing.T) {
	h, f, b := importBrowser(t, "op-secret")
	f.plan = draftPlan()
	f.err = livedb.ErrStaleHash

	_, page := h.uiPost(t, b, "/app/import/livedb", url.Values{
		"act": {"sign"}, "plan": {"plan-1"}, "hash": {"sha256:what-i-read"}, "by": {"liliang"},
	})
	if !strings.Contains(page, "(409)") {
		t.Fatalf("a stale hash did not render as a 409:\n%s", page)
	}
	if !strings.Contains(page, "the plan changed since you read it") {
		t.Errorf("the refusal does not say what happened:\n%s", page)
	}
	if strings.Contains(page, "signed. The ledger has the signature") {
		t.Fatalf("a refused signature rendered as a success")
	}
	if f.hash != "sha256:what-i-read" {
		t.Fatalf("the signature named %q rather than the hash the page showed", f.hash)
	}

	f.err = nil
	f.plan = signedPlan()
	_, page = h.uiPost(t, b, "/app/import/livedb", url.Values{
		"act": {"sign"}, "plan": {"plan-1"}, "hash": {"sha256:abcdef"}, "by": {"liliang"},
	})
	if !strings.Contains(page, "signed.") {
		t.Fatalf("a good signature did not take:\n%s", page)
	}
	if f.signBy != "liliang" {
		t.Errorf("the signature was recorded as %q", f.signBy)
	}
}

// The allow-list refuses, and refuses an unreadable connection string in
// exactly the same words — so a browser cannot read the list back either.
func TestTheImportScreenRefusesAnUnlistedHostAndAnUnreadableDSNAlike(t *testing.T) {
	h, f, b := importBrowser(t, "op-secret")
	f.plan = draftPlan()
	h.srv.opts.LiveDBHosts = []string{"db.internal"}

	if _, page := h.uiPost(t, b, "/app/import/livedb", url.Values{
		"act": {"propose"}, "driver": {"postgres"}, "dsn": {livedbDSN},
	}); !strings.Contains(page, "a draft plan was proposed") {
		t.Fatalf("a host on the list was refused:\n%s", page)
	}

	elsewhere := "postgres://importer:" + livedbPassword_ + "@somewhere.else:5432/crm"
	_, offList := h.uiPost(t, b, "/app/import/livedb", url.Values{
		"act": {"propose"}, "driver": {"postgres"}, "dsn": {elsewhere},
	})
	_, unreadable := h.uiPost(t, b, "/app/import/livedb", url.Values{
		"act": {"propose"}, "driver": {"postgres"}, "dsn": {"this is not a connection string"},
	})
	if !strings.Contains(offList, livedbRefusal) {
		t.Fatalf("a host off the list was not refused:\n%s", offList)
	}
	if offList != unreadable {
		t.Fatalf("the two refusals differ, so the list is an oracle")
	}
	leaked(t, "a refusal by the allow-list", offList)
}

// A run's report says what it did, and names drift by column: the plan does
// not cover those, they were dropped, and a re-signing is owed.
func TestTheRunReportNamesDriftByColumn(t *testing.T) {
	h, f, b := importBrowser(t, "op-secret")
	f.plan = signedPlan()
	f.report = livedb.RunReport{
		ID: "run-9", Plan: "plan-1", RowsRead: 120, Chunks: 30, Triples: 44,
		Drift: []string{"customers.loyalty_tier", "customers.date_of_birth"},
		Gone:  []string{"customers.fax"},
	}

	_, page := h.uiPost(t, b, "/app/import/livedb", url.Values{
		"act": {"run"}, "plan": {"plan-1"}, "dsn": {livedbDSN},
	})
	for _, want := range []string{"customers.loyalty_tier", "customers.date_of_birth", "customers.fax", "re-signing is owed", "120"} {
		if !strings.Contains(page, want) {
			t.Errorf("the run report never says %q:\n%s", want, page)
		}
	}
	leaked(t, "a run report with drift", page)

	// And the runs screen, which reads the same reports per plan.
	_, page = h.navigate(t, b, "/app/import/runs?plan=plan-1")
	if !strings.Contains(page, "customers.loyalty_tier") {
		t.Errorf("the runs screen does not name the drift:\n%s", page)
	}
}

// Starting a follow is a write that answers 202, and the listing that comes
// back names the database by its redacted form and offers a stop.
func TestAFollowIsStartedAndStoppedFromTheScreen(t *testing.T) {
	h, f, b := importBrowser(t, "op-secret")
	f.plan = signedPlan()
	f.holdRuns()
	t.Cleanup(f.releaseRuns)

	_, page := h.uiPost(t, b, "/app/import/follows", url.Values{
		"act": {"start"}, "plan": {"plan-1"}, "dsn": {livedbDSN}, "namespace": {"crm"},
	})
	if !strings.Contains(page, "accepted (202)") {
		t.Fatalf("starting a follow did not answer 202:\n%s", page)
	}
	if !strings.Contains(page, "postgres://importer@db.internal:5432/crm") {
		t.Errorf("the listing does not name the database by its redacted form:\n%s", page)
	}
	leaked(t, "a follow listing", page)
	f.awaitRun(t)

	id := h.srv.follows.list()[0].ID
	_, page = h.uiPost(t, b, "/app/import/follows", url.Values{"act": {"stop"}, "id": {id}})
	if !strings.Contains(page, "stopped.") {
		t.Fatalf("stopping a follow did not take:\n%s", page)
	}
	if strings.Contains(page, id) {
		t.Errorf("a stopped follow is still in the listing")
	}
}

// A store that could not be built is a 503 the screen renders, not a panic
// and not an empty page pretending there is nothing to show.
func TestAnUnavailableStoreRendersTheShellAndSaysSo(t *testing.T) {
	h, _ := livedbHarness(t)
	h.srv.liveDB = func() (livedbStore, error) { return nil, errors.New("the live-database schema is not written yet") }
	b := h.browser(t)
	h.signIn(t, b, "op-secret")

	resp, page := h.navigate(t, b, "/app/import/livedb?plan=plan-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the wizard answered %d, want the shell carrying the reason", resp.StatusCode)
	}
	for _, want := range []string{"Athanor", "(503)", "not available on this server"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page never says %q:\n%s", want, page)
		}
	}

	if _, page := h.uiPost(t, b, "/app/import/livedb", url.Values{
		"act": {"propose"}, "driver": {"postgres"}, "dsn": {livedbDSN},
	}); !strings.Contains(page, "(503)") {
		t.Errorf("a proposal against an unavailable store did not say so:\n%s", page)
	} else {
		leaked(t, "a proposal against an unavailable store", page)
	}

	// The follows listing is this server's own state and answers even when the
	// store cannot be built, which is deliberate: a 503 there would hide
	// running goroutines behind the reason they could not have been started.
	if resp, page := h.navigate(t, b, "/app/import/follows"); resp.StatusCode != http.StatusOK ||
		!strings.Contains(page, "Nothing is being followed.") {
		t.Errorf("the follows screen did not answer: %d\n%s", resp.StatusCode, page)
	}

	if resp, page := h.navigate(t, b, "/app/import"); resp.StatusCode != http.StatusOK ||
		!strings.Contains(page, "A database schema (DDL)") {
		t.Errorf("the chooser needs no store and did not answer: %d\n%s", resp.StatusCode, page)
	}
}
