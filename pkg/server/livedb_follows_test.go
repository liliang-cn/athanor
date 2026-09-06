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

	"github.com/liliang-cn/athanor/pkg/livedb"
)

func followBody(dsn string) string {
	return `{"plan":"plan-1","dsn":"` + dsn + `"}`
}

// followHarness is livedbHarness with every Run held, so a follow started
// here has a lifetime the test controls rather than one that ends before the
// response is read.
func followHarness(t *testing.T) (*harness, *livedbFake) {
	t.Helper()
	h, f := livedbHarness(t)
	f.holdRuns()
	return h, f
}

// startedFollow starts one and returns its id.
func startedFollow(t *testing.T, h *harness, f *livedbFake) string {
	t.Helper()
	code, body := h.do(http.MethodPost, "/athanor/livedb/follows", "op-secret", followBody(livedbDSN))
	if code != http.StatusAccepted {
		t.Fatalf("start a follow: %d %s", code, body)
	}
	f.awaitRun(t)
	var answer struct {
		Follow livedbFollowView `json:"follow"`
		Note   string           `json:"note"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("the start answer is not a follow: %v (%s)", err, body)
	}
	if answer.Follow.ID == "" {
		t.Fatalf("the follow came back without an id: %s", body)
	}
	return answer.Follow.ID
}

// follows reads the listing.
func listFollows(t *testing.T, h *harness, bearer string) []livedbFollowView {
	t.Helper()
	code, body := h.do(http.MethodGet, "/athanor/livedb/follows", bearer, "")
	if code != http.StatusOK {
		t.Fatalf("list follows: %d %s", code, body)
	}
	var answer struct {
		Follows []livedbFollowView `json:"follows"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("the listing is not a list of follows: %v (%s)", err, body)
	}
	return answer.Follows
}

// awaitFollowState polls the listing until a follow reaches a state, because
// the goroutine that sets it is not the one the request returned on.
func awaitFollowState(t *testing.T, h *harness, id, want string) livedbFollowView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, v := range listFollows(t, h, "op-secret") {
			if v.ID == id && v.State == want {
				return v
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached %q: %+v", id, want, listFollows(t, h, "op-secret"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The three routes, cell by cell. Starting and stopping a follow dial a
// database and change what this server is doing; listing them is a read. The
// same matrix the plans have, written out for the same reason: the
// classification has been got wrong twice nearby and both times it was a
// table nobody tested.
func TestTheFollowRoutesTakeTheClearanceTheyShould(t *testing.T) {
	h, f := followHarness(t)

	if code, body := h.do(http.MethodPost, "/athanor/livedb/follows", "ro-secret", followBody(livedbDSN)); code != http.StatusForbidden {
		t.Errorf("a read-only key started a follow: %d %s", code, body)
	}
	if calls := f.seen(); len(calls) != 0 {
		t.Fatalf("a refused start reached the store: %v", calls)
	}

	id := startedFollow(t, h, f)

	if code, body := h.do(http.MethodGet, "/athanor/livedb/follows", "ro-secret", ""); code != http.StatusOK {
		t.Errorf("a read-only key could not list the follows: %d %s", code, body)
	}
	if code, body := h.do(http.MethodDelete, "/athanor/livedb/follows/"+id, "ro-secret", ""); code != http.StatusForbidden {
		t.Errorf("a read-only key stopped a follow: %d %s", code, body)
	}
	if got := listFollows(t, h, "op-secret"); len(got) != 1 || got[0].State != livedbFollowRunning {
		t.Fatalf("the refused delete stopped it anyway: %+v", got)
	}
	if code, body := h.do(http.MethodDelete, "/athanor/livedb/follows/"+id, "op-secret", ""); code != http.StatusOK {
		t.Errorf("delete: %d %s", code, body)
	}
	// And the method table, so a PUT is a refusal rather than a listing.
	if code, _ := h.do(http.MethodPut, "/athanor/livedb/follows", "op-secret", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("PUT on the collection answered %d", code)
	}
	if code, _ := h.do(http.MethodGet, "/athanor/livedb/follows/"+id, "op-secret", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET on one follow answered %d, and there is no such route", code)
	}
}

// A follow appears in the listing while it runs, is deleted, and is gone. The
// store was asked to follow, not merely to run.
func TestAFollowIsListedUntilItIsDeleted(t *testing.T) {
	h, f := followHarness(t)

	if got := listFollows(t, h, "op-secret"); len(got) != 0 {
		t.Fatalf("a fresh server is already following something: %+v", got)
	}
	id := startedFollow(t, h, f)

	got := listFollows(t, h, "op-secret")
	if len(got) != 1 {
		t.Fatalf("the listing holds %d follows, want 1: %+v", len(got), got)
	}
	if got[0].ID != id || got[0].Plan != "plan-1" || got[0].State != livedbFollowRunning {
		t.Errorf("the listed follow is not the one that was started: %+v", got[0])
	}
	if got[0].Redacted != "postgres://importer@db.internal:5432/crm" {
		t.Errorf("the listing does not name the database by its redacted form: %+v", got[0])
	}

	f.mu.Lock()
	req := f.run
	f.mu.Unlock()
	if !req.Follow {
		t.Errorf("the store was asked for a plain run: %+v", req)
	}
	if req.DSN != livedbDSN {
		t.Errorf("the credential did not reach the store, so nothing could be imported: %q", req.DSN)
	}

	code, body := h.do(http.MethodDelete, "/athanor/livedb/follows/"+id, "op-secret", "")
	if code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	if got := listFollows(t, h, "op-secret"); len(got) != 0 {
		t.Fatalf("a deleted follow is still listed: %+v", got)
	}
	// Deleting cancels it: the goroutine ends without the block being
	// released, which is the whole of what "stop" has to mean.
	f.awaitRunReturn(t)

	// And a second delete is a 404 that does not quote the id back.
	code, body = h.do(http.MethodDelete, "/athanor/livedb/follows/"+id, "op-secret", "")
	if code != http.StatusNotFound {
		t.Errorf("deleting it twice answered %d, want 404: %s", code, body)
	}
	if strings.Contains(body, id) {
		t.Errorf("the 404 quoted the caller's own path segment back: %s", body)
	}
}

// One follow per plan. Two watchers on one change stream fight over the
// checkpoint, so the second start is refused rather than accepted and
// quietly corrupting a resume position.
func TestASecondFollowOfOnePlanIsRefused(t *testing.T) {
	h, f := followHarness(t)
	id := startedFollow(t, h, f)

	code, body := h.do(http.MethodPost, "/athanor/livedb/follows", "op-secret", followBody(livedbDSN))
	if code != http.StatusConflict {
		t.Fatalf("a second follow of one plan answered %d, want 409: %s", code, body)
	}
	if !strings.Contains(body, "checkpoint") {
		t.Errorf("the refusal does not say why one follow per plan: %s", body)
	}
	if !strings.Contains(body, id) {
		t.Errorf("the refusal does not name the follow that is in the way: %s", body)
	}
	if got := listFollows(t, h, "op-secret"); len(got) != 1 {
		t.Fatalf("the refused start was registered anyway: %+v", got)
	}

	// A follow that has stopped holds the slot too — that is what keeps its
	// reason on the screen — and says so differently.
	f.releaseRuns()
	awaitFollowState(t, h, id, livedbFollowStopped)
	code, body = h.do(http.MethodPost, "/athanor/livedb/follows", "op-secret", followBody(livedbDSN))
	if code != http.StatusConflict {
		t.Fatalf("starting over a stopped follow answered %d, want 409: %s", code, body)
	}
	if !strings.Contains(body, "delet") {
		t.Errorf("the refusal does not say how to get past it: %s", body)
	}

	// Deleting the stopped one is how another is started.
	if code, body := h.do(http.MethodDelete, "/athanor/livedb/follows/"+id, "op-secret", ""); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, "/athanor/livedb/follows", "op-secret", followBody(livedbDSN)); code != http.StatusAccepted {
		t.Fatalf("starting after the stopped one was deleted: %d %s", code, body)
	}
}

// The credential is in no answer this route produces: not the start, not the
// listing, not a refusal, and not the reason a follow stopped — which is the
// dangerous one, because a driver quotes its own connection string and the
// listing is readable by any key at all.
func TestTheCredentialIsNeverInAFollowsAnswers(t *testing.T) {
	check := func(t *testing.T, what, body string) {
		t.Helper()
		if strings.Contains(body, livedbPassword_) {
			t.Fatalf("%s put the password in the answer: %s", what, body)
		}
		if strings.Contains(body, livedbDSN) {
			t.Fatalf("%s put the whole DSN in the answer: %s", what, body)
		}
	}

	h, f := followHarness(t)
	// What the store answers when the follow ends is a driver's own error,
	// with the connection string in it.
	f.mu.Lock()
	f.runErr = errors.New(`dial "` + livedbDSN + `": the replication slot went away`)
	f.mu.Unlock()

	code, start := h.do(http.MethodPost, "/athanor/livedb/follows", "op-secret", followBody(livedbDSN))
	if code != http.StatusAccepted {
		t.Fatalf("start: %d %s", code, start)
	}
	check(t, "a start", start)
	f.awaitRun(t)
	var answer struct {
		Follow livedbFollowView `json:"follow"`
	}
	if err := json.Unmarshal([]byte(start), &answer); err != nil {
		t.Fatalf("the start answer: %v (%s)", err, start)
	}
	id := answer.Follow.ID

	_, running := h.do(http.MethodGet, "/athanor/livedb/follows", "ro-secret", "")
	check(t, "a listing of a running follow", running)

	f.releaseRuns()
	stopped := awaitFollowState(t, h, id, livedbFollowStopped)
	if stopped.Error == "" {
		t.Fatal("a follow that failed is listed with no reason at all")
	}
	if !strings.Contains(stopped.Error, "replication slot") {
		t.Errorf("the reason lost what happened: %q", stopped.Error)
	}
	_, afterFailure := h.do(http.MethodGet, "/athanor/livedb/follows", "ro-secret", "")
	check(t, "a listing of a failed follow", afterFailure)

	_, refusal := h.do(http.MethodPost, "/athanor/livedb/follows", "op-secret", followBody(livedbDSN))
	check(t, "a 409", refusal)
	_, gone := h.do(http.MethodDelete, "/athanor/livedb/follows/nothing", "op-secret", "")
	check(t, "a 404", gone)
	_, deleted := h.do(http.MethodDelete, "/athanor/livedb/follows/"+id, "op-secret", "")
	check(t, "a delete", deleted)
}

// A follow that fails stays listed with its reason. An operator learns why
// rather than finding it simply gone, and it does not retry in a tight loop:
// the store is asked exactly once.
func TestAFailedFollowStaysListedWithItsReason(t *testing.T) {
	h, f := followHarness(t)
	f.mu.Lock()
	f.runErr = errors.New("the replication slot went away")
	f.mu.Unlock()

	id := startedFollow(t, h, f)
	f.releaseRuns()
	stopped := awaitFollowState(t, h, id, livedbFollowStopped)

	if !strings.Contains(stopped.Error, "replication slot") {
		t.Errorf("the follow stopped without saying why: %+v", stopped)
	}
	if stopped.StoppedAt == nil || stopped.StoppedAt.Before(stopped.StartedAt) {
		t.Errorf("the follow has no sensible ending: %+v", stopped)
	}
	// Once. A watcher error is a reason to stop, not a reason to dial again
	// as fast as the loop goes round.
	time.Sleep(50 * time.Millisecond)
	runs := 0
	for _, call := range f.seen() {
		if call == "run" {
			runs++
		}
	}
	if runs != 1 {
		t.Fatalf("the store was asked to follow %d times, want 1", runs)
	}
	if got := listFollows(t, h, "op-secret"); len(got) != 1 {
		t.Fatalf("the failed follow was removed rather than kept: %+v", got)
	}
}

// The allow-list is checked before anything is launched, and before the plan
// is even read: the point of the list is that the dial does not happen.
func TestTheAllowListRefusesAFollow(t *testing.T) {
	h, f := followHarness(t)
	h.srv.opts.LiveDBHosts = []string{"db.internal"}

	elsewhere := "postgres://importer:" + livedbPassword_ + "@somewhere.else:5432/crm"
	code, body := h.do(http.MethodPost, "/athanor/livedb/follows", "op-secret", followBody(elsewhere))
	if code != http.StatusForbidden {
		t.Fatalf("a follow of a host off the list answered %d, want 403: %s", code, body)
	}
	if !strings.Contains(body, livedbRefusal) {
		t.Errorf("the refusal is not the one the list gives everywhere else: %s", body)
	}
	if calls := f.seen(); len(calls) != 0 {
		t.Fatalf("a refused follow reached the store: %v", calls)
	}
	if got := listFollows(t, h, "op-secret"); len(got) != 0 {
		t.Fatalf("a refused follow was registered: %+v", got)
	}

	// The host on the list still starts.
	if code, body := h.do(http.MethodPost, "/athanor/livedb/follows", "op-secret", followBody(livedbDSN)); code != http.StatusAccepted {
		t.Fatalf("a host on the list was refused: %d %s", code, body)
	}
	f.awaitRun(t)
}

// The body's mistakes, answered by the request that made them rather than by
// a line in a listing somebody has to go and read.
func TestAFollowRefusesWhatItCannotStart(t *testing.T) {
	h, f := followHarness(t)

	for _, tc := range []struct {
		what string
		body string
		want int
	}{
		{"no plan", `{"dsn":"` + livedbDSN + `"}`, http.StatusBadRequest},
		{"no credential", `{"plan":"plan-1"}`, http.StatusBadRequest},
		{"a flag this route does not take", `{"plan":"plan-1","dsn":"x","dry_run":true}`, http.StatusBadRequest},
	} {
		if code, body := h.do(http.MethodPost, "/athanor/livedb/follows", "op-secret", tc.body); code != tc.want {
			t.Errorf("%s answered %d, want %d: %s", tc.what, code, tc.want, body)
		}
	}
	if calls := f.seen(); len(calls) != 0 {
		t.Fatalf("an unstartable follow reached the store: %v", calls)
	}

	// A plan nobody signed cannot be followed, and the answer is the same
	// conflict a run gets — the plan is read before the goroutine is
	// launched exactly so that this is a status and not a listing entry.
	draft := signedPlan()
	draft.State = livedb.Draft
	f.mu.Lock()
	f.plan = draft
	f.mu.Unlock()
	if code, body := h.do(http.MethodPost, "/athanor/livedb/follows", "op-secret", followBody(livedbDSN)); code != http.StatusConflict {
		t.Errorf("following an unsigned plan answered %d, want 409: %s", code, body)
	}
	if got := listFollows(t, h, "op-secret"); len(got) != 0 {
		t.Fatalf("an unsigned plan was followed anyway: %+v", got)
	}

	// And a plan that does not exist is a 404 rather than a follow that
	// fails a minute later.
	f.mu.Lock()
	f.err = livedb.ErrNoPlan
	f.mu.Unlock()
	if code, body := h.do(http.MethodPost, "/athanor/livedb/follows", "op-secret", followBody(livedbDSN)); code != http.StatusNotFound {
		t.Errorf("following a plan nothing answers to: %d %s", code, body)
	}
}

// Starting and stopping a follow are acts somebody performed with a
// credential, so they are entries — in the same series as the run they
// extend, and resting on the same signature.
func TestAFollowsStartAndStopReachTheLedger(t *testing.T) {
	h, f := followHarness(t)
	spy := &spyLedger{}
	h.srv.ledger = spy

	id := startedFollow(t, h, f)
	entries := spy.seen()
	if len(entries) != 1 {
		t.Fatalf("starting a follow wrote %d entries, want 1: %+v", len(entries), entries)
	}
	start := entries[0]
	if start.ID != "athanor:livedb:follow:"+id {
		t.Errorf("the follow's entry id = %q", start.ID)
	}
	if start.Kind != livedbActFollow || start.Verdict != "following" {
		t.Errorf("the follow was recorded as %q/%q", start.Kind, start.Verdict)
	}
	if start.Actor != "operator" {
		t.Errorf("the follow was signed by %q, not the key that called", start.Actor)
	}
	if !contains(start.Premises, "decision:athanor:livedb:plan:plan-1") {
		t.Errorf("the follow does not rest on the signature that permitted it: %v", start.Premises)
	}
	for _, entry := range entries {
		body, _ := json.Marshal(entry)
		if strings.Contains(string(body), livedbPassword_) {
			t.Fatalf("the ledger holds the credential: %s", body)
		}
	}

	if code, body := h.do(http.MethodDelete, "/athanor/livedb/follows/"+id, "op-secret", ""); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	f.awaitRunReturn(t)
	entries = spy.seen()
	if len(entries) != 2 {
		t.Fatalf("stopping a follow wrote %d entries in total, want 2: %+v", len(entries), entries)
	}
	stop := entries[1]
	if stop.ID != start.ID {
		t.Errorf("the stop grew a second entry instead of updating the follow's: %q then %q", start.ID, stop.ID)
	}
	if stop.Verdict != "stopped" {
		t.Errorf("the stop's verdict = %q", stop.Verdict)
	}
	// The goroutine ends after the DELETE has already recorded the stop, and
	// must not record it a second time under its own name.
	time.Sleep(50 * time.Millisecond)
	if got := len(spy.seen()); got != 2 {
		t.Fatalf("the cancelled goroutine recorded the stop again: %d entries", got)
	}
}

// Shutting down cancels every follow and waits for it. A goroutine still
// writing into a brain that Close has released is a crash on the way out, so
// the assertion is that the run has already returned by the time Close does —
// not that it was told to.
func TestShuttingDownStopsEveryFollow(t *testing.T) {
	f := &livedbFake{
		plan:   signedPlan(),
		report: livedb.RunReport{ID: "run-9", Plan: "plan-1"},
	}
	f.holdRuns()
	addr, stop := followServer(t, f)

	code, body := doAt(t, addr, http.MethodPost, "/athanor/livedb/follows", "op-secret", followBody(livedbDSN))
	if code != http.StatusAccepted {
		t.Fatalf("start: %d %s", code, body)
	}
	f.awaitRun(t)

	// Nothing releases the block: the only thing that can end this follow is
	// the server ending.
	stop()
	if !f.runReturned() {
		t.Fatal("Close returned while a follow was still running, so a goroutine is writing into a closed brain")
	}
}

// followServer is a server this test shuts down itself, which newHarness's
// does not allow: its cleanup closes the server, and a test that closed it
// too would close the brain twice.
func followServer(t *testing.T, f *livedbFake) (httpAddr string, stop func()) {
	t.Helper()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(keyFile, []byte(keysJSON), 0o600); err != nil {
		t.Fatalf("keys: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv, err := New(ctx, Options{
		DBPath: filepath.Join(dir, "brain.db"), GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0",
		KeyFile: keyFile, Spool: dir, Runner: fakeRunner{result: cannedResult()},
	})
	if err != nil {
		cancel()
		t.Fatalf("New: %v", err)
	}
	srv.liveDB = func() (livedbStore, error) { return f, nil }
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	_, httpAddr = srv.Addrs()

	var once bool
	stop = func() {
		if once {
			return
		}
		once = true
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return")
		}
		_ = srv.Close()
	}
	t.Cleanup(stop)
	return httpAddr, stop
}

// doAt is harness.do against an address rather than a harness.
func doAt(t *testing.T, addr, method, path, bearer, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+addr+path, stringsReader(body))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	answer, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(answer)
}
