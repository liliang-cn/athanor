package server

import (
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/liliang-cn/athanor/pkg/rules"
	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
)

// The rule engine, as a workflow: declare → publish → fire, and retire.
//
// CortexDB has the engine — a rule is a Horn clause over graph edges, and
// ApplyRules forward-chains a set of them and writes what they derive with the
// provenance inference_explain reads back. It holds no workflow, because it
// holds nothing (pkg/rules says why at length). These routes are the half that
// has to be somewhere: which rules exist, which one is in force, who put it
// there, and who fired it over what.
//
// The door is the one /athanor/loads and /athanor/ontologies stand in. Reads
// take any key from the policy and writes take a read-write one; every act is
// recorded in the decision ledger under the key that performed it; a refusal
// from the store is a status rather than a stack trace.
//
// One route is not like its siblings. Firing a rule is the only read-shaped
// verb here that changes the graph — it is a POST, it is classified a write,
// and it is refused outright without a `by`, exactly as approving a vocabulary
// is. What it derives lands graded, so the shelf can count it and a person can
// distrust it.

// ruleStores holds one store per brain — ontologies.go's arrangement, for its
// reasons: an entry is created once per *cortexdb.DB, so the schema DDL runs
// once rather than per request.
var ruleStores sync.Map // *cortexdb.DB -> *ruleStore

type ruleStore struct {
	once  sync.Once
	store *rules.Store
	err   error
}

func (s *Server) rules() (*rules.Store, error) {
	entry, _ := ruleStores.LoadOrStore(s.db, &ruleStore{})
	h := entry.(*ruleStore)
	h.once.Do(func() {
		h.store, h.err = rules.New(s.db, rules.WithLedger(ruleLedger{srv: s}))
	})
	return h.store, h.err
}

// ruleOperation is the name the key policy authorizes against, the same shape
// "athanor.loads" and "athanor.ontologies" use.
const ruleOperation = "athanor.rules"

// authorizeRules answers the request itself when the key is missing or the
// clearance is short, and reports whether the handler may continue.
func (s *Server) authorizeRules(w http.ResponseWriter, r *http.Request, access authz.Access) (authz.Key, bool) {
	key, code, msg := s.requestKey(r)
	if code != 0 {
		httpError(w, code, msg)
		return authz.Key{}, false
	}
	if err := key.AuthorizeOperation(ruleOperation, authz.Method{Access: access}); err != nil {
		httpError(w, http.StatusForbidden, err.Error())
		return authz.Key{}, false
	}
	return key, true
}

// rulesReady is the store, or the answer that stands in for it.
func (s *Server) rulesReady(w http.ResponseWriter) (*rules.Store, bool) {
	store, err := s.rules()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	return store, true
}

// handleRules is the collection: POST a declaration to draft it, GET to list.
func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.authorizeRules(w, r, authz.Read); !ok {
			return
		}
		store, ok := s.rulesReady(w)
		if !ok {
			return
		}
		declared, err := store.List(r.Context(),
			strings.TrimSpace(r.URL.Query().Get("lineage")),
			rules.State(strings.TrimSpace(r.URL.Query().Get("state"))))
		if err != nil {
			ruleError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"rules": declared})

	case http.MethodPost:
		key, ok := s.authorizeRules(w, r, authz.Write)
		if !ok {
			return
		}
		store, ok := s.rulesReady(w)
		if !ok {
			return
		}
		// The body is the rule document itself, not an envelope around one: it
		// is the same JSON `current` hands back and the same JSON CortexDB's
		// own rules_save takes, so the two ends of the loop are the same bytes
		// and a person can pipe one into the other.
		document, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			httpError(w, http.StatusBadRequest, "body: "+err.Error())
			return
		}
		// The drafter is the key that presented itself. Declaring a rule is not
		// yet a judgement about the graph — publishing and firing are, and
		// those take a name — so the door's own record of who is calling is the
		// honest actor here.
		declared, err := store.Draft(r.Context(), document, key.ID, strings.TrimSpace(r.URL.Query().Get("note")))
		if err != nil {
			ruleError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, declared)

	default:
		httpError(w, http.StatusMethodNotAllowed, "POST a rule document to declare it, or GET the list")
	}
}

// handleRuleCurrent answers the rule in force for one lineage — the document,
// and nothing wrapped around it.
func (s *Server) handleRuleCurrent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, http.StatusMethodNotAllowed, "GET /athanor/rules/current?lineage=chain")
		return
	}
	if _, ok := s.authorizeRules(w, r, authz.Read); !ok {
		return
	}
	lineage := strings.TrimSpace(r.URL.Query().Get("lineage"))
	if lineage == "" {
		httpError(w, http.StatusBadRequest, "lineage is required: it is the half of an id before the @, e.g. ?lineage=chain")
		return
	}
	store, ok := s.rulesReady(w)
	if !ok {
		return
	}
	current, err := store.Current(r.Context(), lineage)
	if err != nil {
		ruleError(w, err)
		return
	}
	// Which version this is belongs in a header rather than in the body: the
	// body has to stay pasteable, and a caller who wants the metadata has
	// GET /athanor/rules/{id}.
	w.Header().Set("Athanor-Rule-Version", current.ID)
	writeJSON(w, http.StatusOK, current.Definition())
}

// handleRuleFirings is what the rules have actually done, newest first.
//
// A collection of its own rather than a subresource of a rule, for
// /athanor/livedb/runs' reason: the question an operator asks at three in the
// morning is "what has been derived into this brain lately", which is not
// about one rule.
func (s *Server) handleRuleFirings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, http.StatusMethodNotAllowed, "GET /athanor/rules/firings?rule=&limit=")
		return
	}
	if _, ok := s.authorizeRules(w, r, authz.Read); !ok {
		return
	}
	store, ok := s.rulesReady(w)
	if !ok {
		return
	}
	limit := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	firings, err := store.Firings(r.Context(), strings.TrimSpace(r.URL.Query().Get("rule")), limit)
	if err != nil {
		ruleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"firings": firings})
}

// The acts, written after the id as `{id}:verb`.
//
// A verb in the path rather than a subresource, ontologies.go's reason kept:
// none of these is a thing, and `/athanor/rules/chain@1/application` would
// invite a GET, a DELETE and a question about what an application's own
// identity is. Publishing borrows that file's constant — the same word for the
// same act on a different thing, and two spellings of it would be two chances
// to route one of them wrong.
const (
	verbRetire = "retire"
	verbApply  = "apply"
)

// handleRuleVersion is one rule: GET it, or POST `{id}:verb` to act.
func (s *Server) handleRuleVersion(w http.ResponseWriter, r *http.Request) {
	segment := r.PathValue("id")
	id, verb := segment, ""
	if at := strings.LastIndex(segment, ":"); at >= 0 {
		switch segment[at+1:] {
		case verbPublish, verbRetire, verbApply:
			id, verb = segment[:at], segment[at+1:]
		}
	}
	id = strings.TrimSpace(id)
	if id == "" {
		httpError(w, http.StatusBadRequest, "a rule id is required")
		return
	}

	if verb == "" {
		if r.Method != http.MethodGet {
			httpError(w, http.StatusMethodNotAllowed, "GET a rule, or POST {id}:publish, {id}:retire, {id}:apply")
			return
		}
		if _, ok := s.authorizeRules(w, r, authz.Read); !ok {
			return
		}
		store, ok := s.rulesReady(w)
		if !ok {
			return
		}
		declared, err := store.Get(r.Context(), id)
		if err != nil {
			ruleError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, declared)
		return
	}

	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "POST to act on a rule")
		return
	}
	key, ok := s.authorizeRules(w, r, authz.Write)
	if !ok {
		return
	}
	store, ok := s.rulesReady(w)
	if !ok {
		return
	}
	switch verb {
	case verbPublish:
		s.signRule(w, r, store, key, id, verbPublish)
	case verbRetire:
		s.signRule(w, r, store, key, id, verbRetire)
	case verbApply:
		s.applyRule(w, r, store, key, id)
	}
}

// signatureRequest is the body of a publish or a retire.
type signatureRequest struct {
	// By is who decided. Empty takes the key's id: putting a rule in force and
	// taking it out are operational acts by whoever holds the door, where
	// firing one asserts edges and needs a person's name.
	By   string `json:"by,omitempty"`
	Note string `json:"note,omitempty"`
}

func (s *Server) signRule(w http.ResponseWriter, r *http.Request, store *rules.Store, key authz.Key, id, verb string) {
	var req signatureRequest
	if !decodeBody(w, r, &req) {
		return
	}
	by := strings.TrimSpace(req.By)
	if by == "" {
		by = key.ID
	}
	sig := rules.Signature{By: by, Key: key.ID, Note: req.Note}
	if verb == verbRetire {
		retired, err := store.Retire(r.Context(), id, sig)
		if err != nil {
			ruleError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"rule": retired, "by": by})
		return
	}
	published, retired, err := store.Publish(r.Context(), id, sig)
	if err != nil {
		ruleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rule": published, "retired": retired, "by": by})
}

// applyRequest fires a rule.
type applyRequest struct {
	// By is the person. It is in the body and not taken from the key because
	// it is the field that has to survive an argument two years from now, and
	// "whoever held the operator key" is not an answer to who decided that
	// these edges belong in the brain.
	By   string `json:"by"`
	Note string `json:"note,omitempty"`
	// Document scopes which edges take part and is stamped onto what is
	// derived. Empty is the whole graph.
	Document string `json:"document,omitempty"`
	// DryRun writes nothing and reports what would have been derived. It is
	// the only way a draft may be fired.
	DryRun bool `json:"dry_run,omitempty"`
	// DeleteExisting removes what this rule derived before rerunning it.
	DeleteExisting bool `json:"delete_existing,omitempty"`
	MaxIterations  int  `json:"max_iterations,omitempty"`
	MaxDerived     int  `json:"max_derived,omitempty"`
}

func (s *Server) applyRule(w http.ResponseWriter, r *http.Request, store *rules.Store, key authz.Key, id string) {
	var req applyRequest
	if !decodeBody(w, r, &req) {
		return
	}
	firing, err := store.Apply(r.Context(), id, rules.Application{
		By: req.By, Key: key.ID, Note: req.Note, Document: req.Document,
		DryRun: req.DryRun, DeleteExisting: req.DeleteExisting,
		MaxIterations: req.MaxIterations, MaxDerived: req.MaxDerived,
	})
	if err != nil {
		ruleError(w, err)
		return
	}
	// The edges are in the brain. Everything from here is the record of that,
	// and the record is not allowed to undo it: a ledger write that fails
	// leaves a firing that happened, so the failure is reported in the answer
	// and logged, and the status stays 200. This is /athanor/loads' rule, and
	// it is the same rule for the same reason — rolling back a derivation
	// because its audit entry did not write would be a store that loses data
	// to protect a note about the data.
	if firing.LedgerError != "" {
		log.Printf("athanor: ledger: firing %s of rule %s: %s", firing.Act, id, firing.LedgerError)
	}
	writeJSON(w, http.StatusOK, firing)
}

// ruleError turns the store's refusals into the statuses they deserve. They
// are values rather than strings so this reads them by identity.
func ruleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, rules.ErrNotFound):
		httpError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, rules.ErrExists), errors.Is(err, rules.ErrState):
		httpError(w, http.StatusConflict, err.Error())
	// A cap is a conflict rather than a server fault: nothing was written, and
	// the caller resolves it by narrowing the rule or raising the cap.
	case errors.Is(err, rules.ErrCapped):
		httpError(w, http.StatusConflict, err.Error())
	case errors.Is(err, rules.ErrUnsigned), errors.Is(err, rules.ErrInvalid):
		httpError(w, http.StatusBadRequest, err.Error())
	default:
		httpError(w, http.StatusInternalServerError, err.Error())
	}
}
