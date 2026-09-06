package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/liliang-cn/athanor/pkg/livedb"
	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
)

// A database somebody else runs, imported only after somebody signed for it.
//
// pkg/livedb is the workflow — propose a plan from the schema, sign the hash
// you read, run only what was signed. This file is its door, and it is the
// same door /athanor/loads and /athanor/ontologies stand in: the key policy
// decides who may act, the act is recorded in the decision ledger under the
// key that performed it, and a refusal from the store is a status rather than
// a stack trace.
//
// Two properties of this particular route are not shared with its siblings,
// and both are handled here rather than in pkg/livedb because they are the
// door's job.
//
// # The DSN is a secret in a request body
//
// Propose and Run each carry a live credential. It is never stored — the plan
// keeps the redacted form — and here it is never written back: every plan is
// cleared of it on the way out, and every message this file produces is
// scrubbed of the credential the request carried before it is returned. The
// store promises Plan.Source.DSN is empty; the door does not rely on the
// promise, because the cost of that promise being broken once is a password
// in an access log for ever.
//
// # This route dials a host of the caller's choosing
//
// That is the feature. The primary control is the key policy — proposing,
// signing and running are writes, and a read-only key can do none of them.
// Options.LiveDBHosts is the blunt second one: the hosts this Athanor is
// willing to dial at all. It is enforced here, before the store is asked for
// anything, and its refusal is one sentence that does not say why — a list
// that answers differently for "not on the list" and "I could not read that
// DSN" is a list a caller can enumerate.

// livedbStore is the half of *livedb.Store these handlers use.
//
// It is an interface because pkg/server should not be welded to a concrete
// store, and because that is what lets these routes be tested against a fake
// that returns each of the store's refusals in turn — which is the only way
// to prove the status mapping without a live database on the other end.
type livedbStore interface {
	Propose(ctx context.Context, src livedb.Source, opts livedb.ProposeOptions) (livedb.Plan, error)
	Amend(ctx context.Context, id string, changes []livedb.Change, by string) (livedb.Plan, error)
	Sign(ctx context.Context, id, hash, by, note string) (livedb.Plan, error)
	Get(ctx context.Context, id string) (livedb.Plan, error)
	List(ctx context.Context, q livedb.ListQuery) ([]livedb.Plan, error)
	Current(ctx context.Context, sourceKey string) (livedb.Plan, error)
	Run(ctx context.Context, req livedb.RunRequest, actor string) (livedb.RunReport, error)
	Runs(ctx context.Context, planID string, limit int) ([]livedb.RunReport, error)
}

// livedbStores holds one store per brain — ontologies.go's arrangement, for
// its reasons: the schema DDL runs once per *cortexdb.DB rather than once per
// request.
var livedbStores sync.Map // *cortexdb.DB -> *livedbHandle

type livedbHandle struct {
	once  sync.Once
	store *livedb.Store
	err   error
}

// livedb returns the brain's store, building it on first use.
//
// Lazily, and not in New, because pkg/livedb's implementation is still being
// written: livedb.New returns an error today, and a server that refused to
// start over that would take the rest of Athanor down for a feature nobody
// had asked for yet. The handlers answer 503 while that is true. It is a
// temporary state and it is not allowed to be a panic.
func (s *Server) livedb() (livedbStore, error) {
	if s.liveDB != nil {
		return s.liveDB()
	}
	entry, _ := livedbStores.LoadOrStore(s.db, &livedbHandle{})
	h := entry.(*livedbHandle)
	h.once.Do(func() {
		h.store, h.err = livedb.New(s.db, livedb.WithLedger(livedbLedger{srv: s}))
	})
	if h.err != nil {
		return nil, h.err
	}
	return h.store, nil
}

// livedbOperation is the name the key policy authorizes against, the same
// shape "athanor.loads" and "athanor.ontologies" use.
const livedbOperation = "athanor.livedb"

// authorizeLivedb answers the request itself when the key is missing or the
// clearance is short, and reports whether the handler may continue.
//
// Reading a plan is a read; proposing, amending, signing and running are
// writes. That distinction is the whole of this route's access control, and
// the equivalent one has been got wrong twice nearby — CortexDB's REST once
// classified every tool call as a write, and this repo's own method table
// once called alchemy's Review a read — so each cell of the matrix has a test
// rather than a reading of this comment.
func (s *Server) authorizeLivedb(w http.ResponseWriter, r *http.Request, access authz.Access) (authz.Key, bool) {
	key, code, msg := s.httpKey(r.Header.Get("Authorization"))
	if code != 0 {
		httpError(w, code, msg)
		return authz.Key{}, false
	}
	if err := key.AuthorizeOperation(livedbOperation, authz.Method{Access: access}); err != nil {
		httpError(w, http.StatusForbidden, err.Error())
		return authz.Key{}, false
	}
	return key, true
}

// livedbReady is the store, or the 503 that stands in for it.
func (s *Server) livedbReady(w http.ResponseWriter) (livedbStore, bool) {
	store, err := s.livedb()
	if err != nil {
		httpError(w, http.StatusServiceUnavailable, "the live-database store is not available on this server: "+err.Error())
		return nil, false
	}
	return store, true
}

// livedbProposeRequest is the body of POST /athanor/livedb/plans.
//
// The source and the options are two objects rather than one flat one because
// the source is what the plan is about and the options are how it was read,
// and a reviewer of the plan cares about the first and not the second.
type livedbProposeRequest struct {
	Source  livedb.Source         `json:"source"`
	Options livedb.ProposeOptions `json:"options,omitempty"`
}

// livedbAmendRequest is the body of PATCH /athanor/livedb/plans/{id}.
type livedbAmendRequest struct {
	Changes []livedb.Change `json:"changes"`
	// By is the person overriding the classifier. Empty takes the key's id.
	// It lands on the treatment as who decided it, which is a claim about a
	// person; the ledger's actor is never taken from here.
	By string `json:"by,omitempty"`
}

// livedbSignRequest is the body of POST /athanor/livedb/plans/{id}/signature.
type livedbSignRequest struct {
	// Hash is the hash of the plan the signer read. A hash that does not
	// match is the refusal this whole workflow exists for.
	Hash string `json:"hash"`
	// By is the person signing. Empty takes the key's id — though a
	// signature is the one act here where a person's name is worth asking
	// for, and the ledger keeps both when they differ.
	By   string `json:"by,omitempty"`
	Note string `json:"note,omitempty"`
}

// handleLivedbPlans is the collection: POST a source to propose, GET to list.
func (s *Server) handleLivedbPlans(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.authorizeLivedb(w, r, authz.Read); !ok {
			return
		}
		store, ok := s.livedbReady(w)
		if !ok {
			return
		}
		plans, err := store.List(r.Context(), livedb.ListQuery{
			SourceKey: ledgerTrim(r.URL.Query().Get("source_key")),
			State:     livedb.State(ledgerTrim(r.URL.Query().Get("state"))),
			Limit:     livedbLimit(r.URL.Query().Get("limit")),
		})
		if err != nil {
			livedbError(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"plans": livedbRedactAll(plans)})

	case http.MethodPost:
		key, ok := s.authorizeLivedb(w, r, authz.Write)
		if !ok {
			return
		}
		var req livedbProposeRequest
		if !decodeBody(w, r, &req) {
			return
		}
		dsn := strings.TrimSpace(req.Source.DSN)
		if dsn == "" {
			httpError(w, http.StatusBadRequest, "source.dsn is required: a plan is proposed by reading the database's own schema")
			return
		}
		if !s.livedbMayDial(w, dsn) {
			return
		}
		store, ok := s.livedbReady(w)
		if !ok {
			return
		}
		// The proposer is the key that presented itself, whatever the body
		// said. ProposeOptions.By is documented as the key id and is not a
		// signature; letting a caller write it would make it a nickname.
		req.Options.By = key.ID
		plan, err := store.Propose(withCallerKey(r.Context(), key), req.Source, req.Options)
		if err != nil {
			livedbError(w, err, dsn)
			return
		}
		writeJSON(w, http.StatusCreated, livedbRedact(plan))

	default:
		httpError(w, http.StatusMethodNotAllowed, "POST a {source, options} document to propose a plan, or GET the list")
	}
}

// handleLivedbPlan is one plan: GET it, or PATCH a draft.
func (s *Server) handleLivedbPlan(w http.ResponseWriter, r *http.Request) {
	id := ledgerTrim(r.PathValue("id"))
	if id == "" {
		httpError(w, http.StatusBadRequest, "a plan id is required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.authorizeLivedb(w, r, authz.Read); !ok {
			return
		}
		store, ok := s.livedbReady(w)
		if !ok {
			return
		}
		plan, err := store.Get(r.Context(), id)
		if err != nil {
			livedbError(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, livedbRedact(plan))

	case http.MethodPatch:
		key, ok := s.authorizeLivedb(w, r, authz.Write)
		if !ok {
			return
		}
		var req livedbAmendRequest
		if !decodeBody(w, r, &req) {
			return
		}
		store, ok := s.livedbReady(w)
		if !ok {
			return
		}
		plan, err := store.Amend(withCallerKey(r.Context(), key), id, req.Changes, firstNonBlank(req.By, key.ID))
		if err != nil {
			livedbError(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, livedbRedact(plan))

	default:
		httpError(w, http.StatusMethodNotAllowed, "GET a plan, or PATCH {changes} to amend a draft")
	}
}

// handleLivedbSignature is the act that puts a plan in force.
//
// A subresource rather than a verb in the path, unlike the ontology routes
// where three verbs act on one version and none of them is a thing. A
// signature is a thing: it has a signer, a time and a hash it names, and
// POSTing one is the plainest way to say what happened.
func (s *Server) handleLivedbSignature(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "POST {hash, by, note} to sign a plan")
		return
	}
	id := ledgerTrim(r.PathValue("id"))
	if id == "" {
		httpError(w, http.StatusBadRequest, "a plan id is required")
		return
	}
	key, ok := s.authorizeLivedb(w, r, authz.Write)
	if !ok {
		return
	}
	var req livedbSignRequest
	if !decodeBody(w, r, &req) {
		return
	}
	hash := ledgerTrim(req.Hash)
	if hash == "" {
		httpError(w, http.StatusBadRequest, "hash is required: a signature names the plan the signer read, and one that names nothing is a checkbox")
		return
	}
	store, ok := s.livedbReady(w)
	if !ok {
		return
	}
	plan, err := store.Sign(withCallerKey(r.Context(), key), id, hash, firstNonBlank(req.By, key.ID), req.Note)
	if err != nil {
		livedbError(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, livedbRedact(plan))
}

// handleLivedbCurrent answers the plan in force for one source.
//
// "current" is a literal segment and Go's mux prefers it to {id}, so a plan
// whose id was literally "current" would be unreachable at its own path. Ids
// are minted by the store, and this is the trade
// /athanor/ontologies/current already makes.
func (s *Server) handleLivedbCurrent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, http.StatusMethodNotAllowed, "GET /athanor/livedb/plans/current?source_key=…")
		return
	}
	if _, ok := s.authorizeLivedb(w, r, authz.Read); !ok {
		return
	}
	sourceKey := ledgerTrim(r.URL.Query().Get("source_key"))
	if sourceKey == "" {
		httpError(w, http.StatusBadRequest, "source_key is required: it is the identity of a (database, schema, tableset), and every plan carries the one it belongs to")
		return
	}
	store, ok := s.livedbReady(w)
	if !ok {
		return
	}
	plan, err := store.Current(r.Context(), sourceKey)
	if err != nil {
		livedbError(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, livedbRedact(plan))
}

// handleLivedbRuns is the imports: POST to run a signed plan, GET a plan's
// runs.
func (s *Server) handleLivedbRuns(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.authorizeLivedb(w, r, authz.Read); !ok {
			return
		}
		plan := ledgerTrim(r.URL.Query().Get("plan"))
		if plan == "" {
			httpError(w, http.StatusBadRequest, "plan is required: runs are read per plan, because a run is only explicable under the plan it ran with")
			return
		}
		store, ok := s.livedbReady(w)
		if !ok {
			return
		}
		reports, err := store.Runs(r.Context(), plan, livedbLimit(r.URL.Query().Get("limit")))
		if err != nil {
			livedbError(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"runs": reports})

	case http.MethodPost:
		key, ok := s.authorizeLivedb(w, r, authz.Write)
		if !ok {
			return
		}
		var req livedb.RunRequest
		if !decodeBody(w, r, &req) {
			return
		}
		req.Plan = strings.TrimSpace(req.Plan)
		if req.Plan == "" {
			httpError(w, http.StatusBadRequest, "plan is required: only a signed plan runs")
			return
		}
		dsn := strings.TrimSpace(req.DSN)
		if dsn == "" {
			httpError(w, http.StatusBadRequest, "dsn is required: the store never kept the credential, so a run supplies it again")
			return
		}
		if req.Follow {
			// Follow returns when its context ends, and over HTTP the context
			// ends when the client hangs up — so the honest description of
			// "follow over a request" is "keep importing until the connection
			// drops, then stop". That is not what the field means, and a door
			// that accepts a flag it cannot honour is worse than one that
			// says it cannot.
			httpError(w, http.StatusBadRequest, "follow is not available over HTTP: it runs until its context ends, which here means until the connection drops")
			return
		}
		if !s.livedbMayDial(w, dsn) {
			return
		}
		store, ok := s.livedbReady(w)
		if !ok {
			return
		}
		report, err := store.Run(withCallerKey(r.Context(), key), req, key.ID)
		if err != nil {
			livedbError(w, err, dsn)
			return
		}
		writeJSON(w, http.StatusOK, report)

	default:
		httpError(w, http.StatusMethodNotAllowed, "POST {plan, dsn} to run a signed plan, or GET ?plan= for a plan's runs")
	}
}

// livedbMaxLimit caps what a caller may ask for in one listing.
const livedbMaxLimit = 500

func livedbLimit(raw string) int {
	n, err := strconv.Atoi(ledgerTrim(raw))
	if err != nil || n <= 0 {
		return 0 // the store's own default
	}
	if n > livedbMaxLimit {
		return livedbMaxLimit
	}
	return n
}

// livedbRedact clears the credential field on the way out.
//
// The store already leaves it empty — Plan.Source documents that the DSN is
// never returned — so this is belt and braces, and it is worth the two lines
// because the cost of that promise being broken once is a password in
// somebody's browser history, their proxy log and their terminal scrollback
// at the same moment.
func livedbRedact(p livedb.Plan) livedb.Plan {
	p.Source.DSN = ""
	return p
}

func livedbRedactAll(plans []livedb.Plan) []livedb.Plan {
	out := make([]livedb.Plan, 0, len(plans))
	for _, p := range plans {
		out = append(out, livedbRedact(p))
	}
	return out
}

// livedbError turns the store's refusals into the statuses they deserve, by
// identity and never by reading a message — which is why livedb.go declares
// them as values.
//
// dsn is the credential this request carried, when it carried one. Nothing
// the store says is trusted to be free of it: a driver's own error is the
// classic place a connection string escapes into a response, and the door
// knows the exact secret to look for.
func livedbError(w http.ResponseWriter, err error, dsn string) {
	switch {
	case errors.Is(err, livedb.ErrNoPlan):
		httpError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, livedb.ErrUnsigned), errors.Is(err, livedb.ErrNotDraft), errors.Is(err, livedb.ErrStaleHash):
		httpError(w, http.StatusConflict, err.Error())
	case errors.Is(err, livedb.ErrWrongSource), errors.Is(err, livedb.ErrNoVault):
		httpError(w, http.StatusBadRequest, err.Error())
	default:
		httpError(w, http.StatusInternalServerError, livedbScrub(err.Error(), dsn))
	}
}

// livedbRefusal is what a host outside the allow-list is told, and it is also
// what an unreadable DSN is told.
//
// One sentence for both, because the difference between them is exactly the
// fact a caller would use to enumerate the list: a distinct "I could not read
// that" answer turns every malformed string into a probe reporting whether
// parsing is what stopped it.
const livedbRefusal = "this Athanor is confined to a list of databases and will not dial that one"

// livedbMayDial enforces Options.LiveDBHosts, before the store is asked for
// anything at all. An empty list is unconfined.
func (s *Server) livedbMayDial(w http.ResponseWriter, dsn string) bool {
	if len(s.opts.LiveDBHosts) == 0 {
		return true
	}
	host, port, ok := livedbDialHost(dsn)
	if !ok || !livedbHostAllowed(s.opts.LiveDBHosts, host, port) {
		httpError(w, http.StatusForbidden, livedbRefusal)
		return false
	}
	return true
}

// livedbHostAllowed matches a host against the list. An entry naming a bare
// host allows any port on it; an entry naming host:port allows that one.
func livedbHostAllowed(allowed []string, host, port string) bool {
	if host == "" {
		return false
	}
	host = strings.ToLower(host)
	hostPort := host
	if port != "" {
		hostPort = host + ":" + port
	}
	for _, entry := range allowed {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if entry == host || entry == hostPort {
			return true
		}
	}
	return false
}

// livedbDialHost reads the host out of a connection string, in the three
// shapes the two supported drivers accept.
//
// It reports ok=false for anything it cannot read, and the caller turns that
// into the same refusal as a host that is not on the list. Guessing at a
// shape it does not know is the dangerous direction: an unparsed DSN that
// defaulted to allowed would be an allow-list with a syntax hole in it.
func livedbDialHost(dsn string) (host, port string, ok bool) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", "", false
	}
	// postgres://user:pw@host:5432/db, and mysql://… for a caller who writes
	// it that way.
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" && u.Host != "" {
		if h := u.Hostname(); h != "" {
			return h, u.Port(), true
		}
	}
	// user:pw@tcp(host:3306)/db — go-sql-driver's own shape. Only tcp: a unix
	// socket has no host to check, so it cannot be read here and is refused.
	if open := strings.Index(dsn, "tcp("); open >= 0 {
		if end := strings.Index(dsn[open:], ")"); end > 0 {
			if h, p := splitHostPort(dsn[open+len("tcp(") : open+end]); h != "" {
				return h, p, true
			}
		}
		return "", "", false
	}
	// host=… port=… dbname=… — libpq's keyword form.
	for _, field := range strings.Fields(dsn) {
		if h, found := strings.CutPrefix(field, "host="); found {
			host = h
		}
		if p, found := strings.CutPrefix(field, "port="); found {
			port = p
		}
	}
	if host != "" {
		return host, port, true
	}
	return "", "", false
}

// splitHostPort is net.SplitHostPort that tolerates a bare host.
func splitHostPort(addr string) (host, port string) {
	if at := strings.LastIndex(addr, ":"); at >= 0 {
		return strings.Trim(addr[:at], "[]"), addr[at+1:]
	}
	return strings.Trim(addr, "[]"), ""
}

// livedbPlaceholder is what a scrubbed secret leaves behind. A word rather
// than the empty string, so a reader of a mangled message can tell that
// something was removed rather than that the message was always odd.
const livedbPlaceholder = "«credential»"

// livedbScrub removes the credential this request carried from a message.
//
// Both the whole connection string and the password inside it, because a
// driver reports the first and a server reports the second, and either one in
// a 500 body is the leak this route exists to prevent.
func livedbScrub(msg, dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, dsn, livedbPlaceholder)
	if pw := livedbPassword(dsn); pw != "" {
		msg = strings.ReplaceAll(msg, pw, livedbPlaceholder)
	}
	return msg
}

// livedbPassword pulls the password out of a connection string, in the same
// three shapes livedbDialHost reads. An empty answer means there was nothing
// to find, which is also what a wrong guess looks like — so this is only ever
// used to remove text and never to authorize anything.
func livedbPassword(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			return pw
		}
	}
	for _, field := range strings.Fields(dsn) {
		if pw, found := strings.CutPrefix(field, "password="); found && pw != "" {
			return pw
		}
	}
	if at := strings.LastIndex(dsn, "@"); at > 0 {
		if colon := strings.Index(dsn[:at], ":"); colon >= 0 {
			return dsn[colon+1 : at]
		}
	}
	return ""
}
