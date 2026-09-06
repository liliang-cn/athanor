package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/liliang-cn/athanor/pkg/livedb"
	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
	"github.com/liliang-cn/cortexdb/v2/pkg/importflow"
)

// Keeping the brain in step with a live database is a job this server owns,
// not a request somebody is holding open.
//
// RunRequest.Follow is refused over /athanor/livedb/runs and stays refused,
// for the reason livedb.go gives: a follow returns when its context ends, and
// over HTTP the context ends when the client hangs up, so the honest reading
// of "follow over a request" is "import until the connection drops". But the
// capability is real and the import screen offers it as a toggle, so refusing
// the flag cannot be the whole answer. It is reachable here instead, as three
// routes over a thing that has a lifetime of its own:
//
//	POST   /athanor/livedb/follows        start one under a signed plan
//	GET    /athanor/livedb/follows        what is running
//	DELETE /athanor/livedb/follows/{id}   stop one
//
// A follow runs under the server's context rather than the request's, so it
// outlives the response that started it. It ends when it is deleted, when the
// server shuts down, or when the change stream fails — and in the last case
// it stays listed with the reason, because an operator has to be able to
// learn why rather than find it gone.
//
// # The credential lives in memory and dies with the process
//
// A run supplies its DSN and is finished with it before the response is
// written. A follow needs it for as long as it runs, and pkg/livedb
// deliberately never persists one. So this server holds it — in the frame of
// the goroutine that is using it, for the life of that goroutine, and nowhere
// else. It is on no struct, in no listing, in no log line, in no metric label
// and in no ledger entry; what names the database everywhere here is the
// redacted string the plan already carries. The consequence is stated to the
// caller rather than hidden: a restart of this server does not resume a
// follow, and somebody re-supplies the credential.
//
// # Starting is not waiting
//
// livedb.Store.Run does the initial import and only then follows, in one
// call, so starting a follow starts a full import. That import can take
// hours. The POST therefore answers 202 as soon as the goroutine is launched,
// and the answer says so: a request that blocked until the first pass
// finished would be the very thing this file exists to avoid — a long-lived
// import tied to a connection that a proxy, a laptop lid or an impatient
// browser will close. It also could not be honest about what happened when it
// timed out, because the import would still be running.
//
// The price is that the listing cannot say which phase a follow is in — the
// first pass and the steady state are one call underneath. That question has
// an exact answer elsewhere: pkg/livedb records the run before it opens the
// change stream, so GET /athanor/livedb/runs?plan=… naming a run is the first
// pass having finished.
//
// # One follow per plan
//
// Two watchers on one change stream fight over the checkpoint, each moving it
// past changes the other has not applied. So a plan has at most one follow,
// and a second start is refused. A follow that has stopped holds the slot
// too, because that is what keeps its reason on the screen; deleting it is
// how somebody says they have read it.

// livedbFollowRequest is the body of POST /athanor/livedb/follows.
//
// It is a type of its own rather than livedb.RunRequest with Follow forced
// on, so that the fields this route accepts are exactly the four it means.
// Follow is not among them — it is what the route is — and neither is DryRun,
// which describes a single pass and says nothing about a stream.
type livedbFollowRequest struct {
	// Plan is the signed plan's id.
	Plan string `json:"plan"`
	// DSN is the credential. See the file comment on where it lives.
	DSN string `json:"dsn"`
	// Mapping states how rows become chunks and triples. Nil derives one.
	Mapping *importflow.MappingPlan `json:"mapping,omitempty"`
	// Namespace is the RAG namespace rows land in. Empty takes the plan id.
	Namespace string `json:"namespace,omitempty"`
}

// The two states a follow is in. There is no third: "starting" would be a
// state nothing ever leaves, because the first pass and the following are one
// call underneath and this server cannot see the boundary.
const (
	livedbFollowRunning = "running"
	livedbFollowStopped = "stopped"
)

// livedbFollowNote is what the start response says about the credential and
// the first pass. It is in the answer and not only in this comment because
// the caller is the one who has to re-supply the credential after a restart,
// and a promise that lives only in a doc comment is a promise to nobody.
const livedbFollowNote = "the first pass runs before the change stream opens, so this follow may be importing for some time yet; " +
	"the credential is held in this server's memory for the life of the follow and nowhere else, so a restart does not resume it and somebody supplies it again"

// The acts this file records.
//
// They are livedb.Act-shaped and go through livedbLedger like the store's
// own, because a follow starting and a follow stopping are acts somebody
// performed with a credential and belong beside the run they extend. The
// kinds are declared here rather than in pkg/livedb because pkg/livedb does
// not know follows exist: it has a Follow flag on a request, and a background
// job that holds a credential for a week is this server's invention.
const (
	livedbActFollow   = "livedb.follow"
	livedbActUnfollow = "livedb.unfollow"
)

// livedbFollowState is one follow.
//
// Every mutable field is guarded by the set's mutex, including the ones read
// only to build an answer — this is the shared state of a background
// goroutine and an HTTP handler, and there is nothing on it worth a second
// lock. The credential is not here; see the file comment.
type livedbFollowState struct {
	id        string
	plan      string
	sourceKey string
	redacted  string
	namespace string
	startedAt time.Time
	cancel    context.CancelFunc

	state     string
	stoppedAt time.Time
	failure   string
	firstPass *livedb.RunReport
}

// livedbFollowView is one follow as an answer.
//
// A separate type from the state, so that a field added to the state is not
// thereby added to the listing: there is nothing here that could hold a
// credential, and the redacted string is what names the database.
type livedbFollowView struct {
	ID        string     `json:"id"`
	Plan      string     `json:"plan"`
	SourceKey string     `json:"source_key,omitempty"`
	Redacted  string     `json:"redacted,omitempty"`
	Namespace string     `json:"namespace,omitempty"`
	State     string     `json:"state"`
	StartedAt time.Time  `json:"started_at"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
	// Error is why it stopped, scrubbed of the credential the start carried.
	Error string `json:"error,omitempty"`
	// FirstPass is the initial import's report, which exists only once the
	// whole follow has ended: Run reports the pass it began with when it
	// returns, and it returns when the following is over.
	FirstPass *livedb.RunReport `json:"first_pass,omitempty"`
}

// view renders the state. The caller holds the set's mutex.
func (st *livedbFollowState) view() livedbFollowView {
	v := livedbFollowView{
		ID: st.id, Plan: st.plan, SourceKey: st.sourceKey, Redacted: st.redacted,
		Namespace: st.namespace, State: st.state, StartedAt: st.startedAt,
		Error: st.failure, FirstPass: st.firstPass,
	}
	if !st.stoppedAt.IsZero() {
		at := st.stoppedAt
		v.StoppedAt = &at
	}
	return v
}

// livedbFollowSet is every follow this server started, and the one place
// their cancel funcs live.
//
// A map behind a mutex rather than a sync.Map because two of the three things
// this has to do are not per-key: admitting one follow depends on every other
// follow's plan, and shutting down has to reach all of them and then wait.
type livedbFollowSet struct {
	mu   sync.Mutex
	byID map[string]*livedbFollowState
	// closed is set by the shutdown, so a start racing it is refused rather
	// than leaking a goroutine nobody is left to wait for.
	closed bool
	// wg counts the launched goroutines, including those whose follow has
	// already been deleted from the map: a deleted follow is gone from the
	// listing at once, and its goroutine is still something the shutdown owes
	// a wait to.
	wg sync.WaitGroup
}

func newLivedbFollowSet() *livedbFollowSet {
	return &livedbFollowSet{byID: map[string]*livedbFollowState{}}
}

// admit reserves the plan's slot for st and counts the goroutine that is
// about to run it.
//
// It returns the refusal when another follow already holds the plan, and
// reports whether this server is still willing to start anything at all. The
// refusal is built here, under the lock, because it names the state of the
// follow that is in the way and reading that state anywhere else would be a
// race.
func (f *livedbFollowSet) admit(st *livedbFollowState) (refusal string, open bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return "", false
	}
	for _, held := range f.byID {
		if held.plan != st.plan {
			continue
		}
		if held.state == livedbFollowRunning {
			return fmt.Sprintf("%s is already being followed, as %s: two watchers on one change stream fight over the checkpoint, "+
				"each moving it past changes the other has not applied, so this server follows a plan at most once", st.plan, held.id), true
		}
		return fmt.Sprintf("%s has a follow that stopped, %s: it stays listed with the reason it stopped until somebody deletes it, "+
			"and deleting it is how another one is started", st.plan, held.id), true
	}
	f.byID[st.id] = st
	f.wg.Add(1)
	return "", true
}

// finish marks a follow stopped and reports whether it was still listed —
// which is to say whether it stopped by itself rather than being deleted —
// and whether the server was shutting down when it did.
func (f *livedbFollowSet) finish(st *livedbFollowState, report livedb.RunReport, failure string) (view livedbFollowView, listed, closing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st.state = livedbFollowStopped
	if st.stoppedAt.IsZero() {
		st.stoppedAt = time.Now().UTC()
	}
	if failure != "" {
		st.failure = failure
	}
	if report.ID != "" {
		r := report
		st.firstPass = &r
	}
	_, listed = f.byID[st.id]
	return st.view(), listed, f.closed
}

// remove takes a follow out of the listing and cancels it.
//
// It does not wait: the goroutine notices the cancellation on its own
// schedule, and the caller of DELETE should not be held while a change stream
// unwinds. What it does guarantee is the property the route promises — the
// follow is gone from the next listing — and the shutdown still waits for the
// goroutine, because wg counts it whether or not the map does.
func (f *livedbFollowSet) remove(id string) (livedbFollowView, bool) {
	f.mu.Lock()
	st, held := f.byID[id]
	if !held {
		f.mu.Unlock()
		return livedbFollowView{}, false
	}
	delete(f.byID, id)
	if st.state == livedbFollowRunning {
		st.state, st.stoppedAt = livedbFollowStopped, time.Now().UTC()
	}
	view := st.view()
	f.mu.Unlock()
	st.cancel()
	return view, true
}

// list is every follow, oldest first so that a repeated GET reads the same
// way twice.
func (f *livedbFollowSet) list() []livedbFollowView {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]livedbFollowView, 0, len(f.byID))
	for _, st := range f.byID {
		out = append(out, st.view())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

// closeAll cancels every follow and waits for the goroutines to return.
//
// Idempotent, because both of the server's two exits call it.
func (f *livedbFollowSet) closeAll() {
	f.mu.Lock()
	f.closed = true
	for _, st := range f.byID {
		st.cancel()
	}
	f.mu.Unlock()
	f.wg.Wait()
}

// stopFollowing ends every background follow this server started.
//
// It runs before the brain is closed and it must: a follow writes into the
// brain, and a goroutine still writing to a handle Close has released is a
// crash on the way out. Serve's shutdown calls it and so does Close — the
// first because that is where the server stops serving, the second because a
// Server that never reached Serve still has to let go of what it started.
func (s *Server) stopFollowing() {
	if s.stopFollows != nil {
		s.stopFollows()
	}
	if s.follows != nil {
		s.follows.closeAll()
	}
}

// followContext is the context a follow runs under: the server's, so that the
// request that started it may end without ending it.
func (s *Server) followContext() context.Context {
	if s.followCtx != nil {
		return s.followCtx
	}
	return context.Background()
}

// handleLivedbFollows is the collection: POST to start following, GET to see
// what is running.
func (s *Server) handleLivedbFollows(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.authorizeLivedb(w, r, authz.Read); !ok {
			return
		}
		// The store is not asked for anything: the follows are this server's
		// own state, and a listing that answered 503 because the store could
		// not be built would hide running goroutines behind the reason they
		// could not have been started.
		writeJSON(w, http.StatusOK, map[string]any{"follows": s.follows.list()})

	case http.MethodPost:
		s.startLivedbFollow(w, r)

	default:
		httpError(w, http.StatusMethodNotAllowed, "POST {plan, dsn} to start following a signed plan, or GET the list")
	}
}

// startLivedbFollow launches one.
func (s *Server) startLivedbFollow(w http.ResponseWriter, r *http.Request) {
	key, ok := s.authorizeLivedb(w, r, authz.Write)
	if !ok {
		return
	}
	var req livedbFollowRequest
	if !decodeBody(w, r, &req) {
		return
	}
	planID := strings.TrimSpace(req.Plan)
	if planID == "" {
		httpError(w, http.StatusBadRequest, "plan is required: only a signed plan is followed")
		return
	}
	dsn := strings.TrimSpace(req.DSN)
	if dsn == "" {
		httpError(w, http.StatusBadRequest, "dsn is required: the store never kept the credential, and a follow needs it for as long as it runs")
		return
	}
	if !s.livedbMayDial(w, dsn) {
		return
	}
	store, ok := s.livedbReady(w)
	if !ok {
		return
	}
	// The plan is read before anything is launched, for the two things a
	// background job cannot report well: that there is no such plan, and that
	// nobody signed it. Both are the caller's mistake and both deserve an
	// answer to the request that made them rather than a line in a listing
	// somebody has to go and read. It is also where the redacted source comes
	// from, which is how every later answer names this database.
	plan, err := store.Get(r.Context(), planID)
	if err != nil {
		livedbError(w, err, dsn)
		return
	}
	if plan.State != livedb.Signed {
		livedbError(w, fmt.Errorf("%w: %s is %s", livedb.ErrUnsigned, plan.ID, plan.State), dsn)
		return
	}

	id, err := mintLivedbFollowID()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The context is the server's, with the key the door authorized carried
	// forward so that the run's own ledger entry names the credential that
	// asked for it and not the open-deployment placeholder.
	ctx, cancel := context.WithCancel(withCallerKey(s.followContext(), key))
	st := &livedbFollowState{
		id: id, plan: plan.ID, sourceKey: plan.SourceKey, redacted: plan.Source.Redacted,
		namespace: strings.TrimSpace(req.Namespace), startedAt: time.Now().UTC(),
		cancel: cancel, state: livedbFollowRunning,
	}
	refusal, open := s.follows.admit(st)
	if !open {
		cancel()
		httpError(w, http.StatusServiceUnavailable, "this server is shutting down and is not starting new follows")
		return
	}
	if refusal != "" {
		cancel()
		httpError(w, http.StatusConflict, refusal)
		return
	}

	mapping, namespace := req.Mapping, st.namespace
	go func() {
		defer s.follows.wg.Done()
		defer cancel()
		report, err := store.Run(ctx, livedb.RunRequest{
			Plan: st.plan, DSN: dsn, Mapping: mapping, Namespace: namespace, Follow: true,
		}, key.ID)
		failure := ""
		if err != nil {
			// Scrubbed with the credential this start carried, because a
			// driver reporting its own connection string is the classic way
			// one escapes — and this one would escape into a listing that
			// anybody with a read key can GET.
			failure = livedbScrub(err.Error(), dsn)
		}
		view, listed, closing := s.follows.finish(st, report, failure)
		if listed && !closing {
			// It stopped by itself. A follow that was deleted has its entry
			// written by the DELETE, and one that ended because the server
			// ended is not an act anybody performed — recording that would
			// also be a write into a brain that is closing.
			s.recordLivedbFollow(ctx, livedbActUnfollow, view, "the change stream ended")
		}
	}()

	view := s.follows.viewOf(st)
	s.recordLivedbFollow(withCallerKey(r.Context(), key), livedbActFollow, view, "keeping the brain in step with "+plan.Source.Redacted)
	writeJSON(w, http.StatusAccepted, map[string]any{"follow": view, "note": livedbFollowNote})
}

// viewOf renders one follow under the lock, for a caller holding the state
// itself rather than its id.
func (f *livedbFollowSet) viewOf(st *livedbFollowState) livedbFollowView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return st.view()
}

// handleLivedbFollow is one follow: DELETE stops it.
//
// There is no GET here. A follow is a handful of fields and the listing is
// the whole of them, so a route that answered one of them would be a second
// rendering of the same thing to keep in step with the first.
func (s *Server) handleLivedbFollow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		httpError(w, http.StatusMethodNotAllowed, "DELETE /athanor/livedb/follows/{id} stops one follow; GET the collection to see them")
		return
	}
	id := ledgerTrim(r.PathValue("id"))
	if id == "" {
		httpError(w, http.StatusBadRequest, "a follow id is required")
		return
	}
	key, ok := s.authorizeLivedb(w, r, authz.Write)
	if !ok {
		return
	}
	view, held := s.follows.remove(id)
	if !held {
		// The id is not quoted back. It is the caller's own string and this
		// is the one route whose caller might have pasted a connection string
		// where an id belongs, and a 404 is not worth the chance of putting
		// one in a response body.
		httpError(w, http.StatusNotFound, "this server is not following anything under that id; a follow does not survive a restart, because the credential it needs was never stored")
		return
	}
	s.recordLivedbFollow(withCallerKey(r.Context(), key), livedbActUnfollow, view, "stopped by hand")
	writeJSON(w, http.StatusOK, map[string]any{"follow": view})
}

// recordLivedbFollow mirrors a follow's start or stop into the ledger.
//
// Through livedbLedger, in livedb.Act's shape, so that these entries sit in
// the same series as the proposal, the signature and the run — and rest on
// the same signature, which is what makes "why is this row in the brain"
// walk back to a name. The detail names the database by its redacted form.
// A failure to record is logged and not returned: the follow is already
// running, and refusing to run it because its record failed would stop an
// import for the sake of the note about it.
func (s *Server) recordLivedbFollow(ctx context.Context, kind string, view livedbFollowView, note string) {
	detail := map[string]any{"plan": view.Plan}
	if view.SourceKey != "" {
		detail["source_key"] = view.SourceKey
	}
	if view.Redacted != "" {
		detail["redacted"] = view.Redacted
	}
	if view.Namespace != "" {
		detail["namespace"] = view.Namespace
	}
	if view.Error != "" {
		detail["error"] = view.Error
	}
	act := livedb.Act{
		ID:       view.ID,
		Kind:     kind,
		Actor:    callerKey(ctx).ID,
		At:       time.Now().UTC(),
		Subject:  view.ID,
		Note:     note,
		Detail:   detail,
		Premises: []string{view.Plan},
	}
	// The context may be the follow's, which is cancelled by the time a stop
	// is recorded. The act happened; a record dropped because the thing it
	// describes is over would be the wrong way round.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := (livedbLedger{srv: s}).Record(ctx, act); err != nil {
		log.Printf("athanor: ledger: %s %s: %v", kind, view.ID, err)
	}
}

// mintLivedbFollowID names one follow. Random rather than derived from the
// plan: a plan followed, stopped and followed again is two jobs with two
// lives, and one ledger entry covering both would lose the first one's
// ending.
func mintLivedbFollowID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("athanor: mint a follow id: %w", err)
	}
	return "follow_" + hex.EncodeToString(b[:]), nil
}
