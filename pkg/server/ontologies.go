package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/liliang-cn/alchemy/pkg/wire"
	alchemyv1 "github.com/liliang-cn/alchemy/proto/alchemy/v1"
	"github.com/liliang-cn/athanor/pkg/ontologies"
	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
)

// The vocabulary, as a workflow: propose → approve → publish.
//
// Until now an ontology was a JSON string a client pasted into every
// CreateJob. alchemy has the two hard pieces — a run says which types it
// wanted and the vocabulary lacked (Result.Proposals), and Extend declares the
// accepted ones under a new id — and holds neither of them, because it holds
// nothing (§4). These routes are the half that has to be somewhere: which
// edits exist, who approved which, and which one a client should be pasting.
//
// The propose route is deliberately the same shape as /athanor/loads. It does
// not read a job's result out of a store of its own; it asks the pipeline's
// own GetResult with the process's credential, which is the call that decides
// whether a job is finished. A held job is refused there, by the service that
// knows it is held, exactly as a load is — the alternative being a second
// opinion in this file about what "finished" means, and two of those is one
// too many.
//
// Every act here is signed, and shaped for the decision ledger it becomes an
// entry in: kind, actor, at, subject, note (pkg/ontologies).

// ontologyStores holds one version store per brain.
//
// It would be a field on Server, and is not only because server.go is shared
// ground this file does not own. An entry is created once per *cortexdb.DB, of
// which a process has one, so the schema DDL runs once rather than per
// request — which is what a field would have bought. The cost of doing it this
// way is that an entry outlives the Server that made it: one pointer per brain
// a process ever opened, which is one in production and a handful in a test
// binary.
var ontologyStores sync.Map // *cortexdb.DB -> *ontologyStore

type ontologyStore struct {
	once  sync.Once
	store *ontologies.Store
	err   error
}

func (s *Server) ontologies() (*ontologies.Store, error) {
	entry, _ := ontologyStores.LoadOrStore(s.db, &ontologyStore{})
	h := entry.(*ontologyStore)
	h.once.Do(func() { h.store, h.err = ontologies.New(s.db) })
	return h.store, h.err
}

// ontologyOperation is the name the key policy authorizes against, the same
// shape "athanor.loads" uses.
const ontologyOperation = "athanor.ontologies"

// authorizeOntology answers the request itself when the key is missing or the
// clearance is short, and reports whether the handler may continue.
func (s *Server) authorizeOntology(w http.ResponseWriter, r *http.Request, access authz.Access) (authz.Key, bool) {
	key, code, msg := s.httpKey(r.Header.Get("Authorization"))
	if code != 0 {
		httpError(w, code, msg)
		return authz.Key{}, false
	}
	if err := key.AuthorizeOperation(ontologyOperation, authz.Method{Access: access}); err != nil {
		httpError(w, http.StatusForbidden, err.Error())
		return authz.Key{}, false
	}
	return key, true
}

// handleOntologies is the collection: POST a document to draft it, GET to list.
func (s *Server) handleOntologies(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.authorizeOntology(w, r, authz.Read); !ok {
			return
		}
		store, err := s.ontologies()
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		versions, err := store.List(r.Context(), strings.TrimSpace(r.URL.Query().Get("lineage")))
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"versions": versions})

	case http.MethodPost:
		key, ok := s.authorizeOntology(w, r, authz.Write)
		if !ok {
			return
		}
		store, err := s.ontologies()
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// The body is the ontology document itself, not an envelope around
		// one: it is the same JSON `current` hands back and the same string a
		// client pastes into CreateJob, so the two ends of the loop are the
		// same bytes and a person can pipe one into the other.
		document, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			httpError(w, http.StatusBadRequest, "body: "+err.Error())
			return
		}
		// The drafter is the key that presented itself. A draft is not yet a
		// judgement about anything — approve and publish are, and those take a
		// name in the body — so the door's own record of who is calling is the
		// honest actor here.
		version, err := store.Draft(r.Context(), document, key.ID, strings.TrimSpace(r.URL.Query().Get("note")))
		if err != nil {
			ontologyError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, version)

	default:
		httpError(w, http.StatusMethodNotAllowed, "POST an ontology document to draft it, or GET the list")
	}
}

// handleOntologyCurrent answers the published document of one lineage — the
// JSON a client pastes into CreateJob, and nothing wrapped around it.
func (s *Server) handleOntologyCurrent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, http.StatusMethodNotAllowed, "GET /athanor/ontologies/current?lineage=sds")
		return
	}
	if _, ok := s.authorizeOntology(w, r, authz.Read); !ok {
		return
	}
	lineage := strings.TrimSpace(r.URL.Query().Get("lineage"))
	if lineage == "" {
		httpError(w, http.StatusBadRequest, "lineage is required: it is the half of an id before the @, e.g. ?lineage=sds")
		return
	}
	store, err := s.ontologies()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	version, err := store.Current(r.Context(), lineage)
	if err != nil {
		ontologyError(w, err)
		return
	}
	// Which version this is belongs in a header rather than in the body: the
	// body has to stay pasteable, and a caller who wants the metadata has
	// GET /athanor/ontologies/{id}.
	w.Header().Set("Athanor-Ontology-Version", version.ID)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(version.Document)
}

// ontologyVerbs are the three acts, written after the id as `{id}:verb`.
//
// A verb in the path rather than a subresource because none of these is a
// thing: `/athanor/ontologies/sds@3/approval` would invite a GET, a DELETE and
// a question about what an approval's own identity is. This is the shape
// alchemy's own gRPC gateway already uses for the same reason.
const (
	verbPropose = "propose"
	verbApprove = "approve"
	verbPublish = "publish"
)

// handleOntologyVersion is one version: GET it, or POST `{id}:verb` to act.
func (s *Server) handleOntologyVersion(w http.ResponseWriter, r *http.Request) {
	segment := r.PathValue("id")
	id, verb := segment, ""
	if at := strings.LastIndex(segment, ":"); at >= 0 {
		switch segment[at+1:] {
		case verbPropose, verbApprove, verbPublish:
			id, verb = segment[:at], segment[at+1:]
		}
	}
	id = strings.TrimSpace(id)
	if id == "" {
		httpError(w, http.StatusBadRequest, "an ontology id is required")
		return
	}

	if verb == "" {
		if r.Method != http.MethodGet {
			httpError(w, http.StatusMethodNotAllowed, "GET a version, or POST {id}:propose, {id}:approve, {id}:publish")
			return
		}
		if _, ok := s.authorizeOntology(w, r, authz.Read); !ok {
			return
		}
		store, err := s.ontologies()
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		version, err := store.Get(r.Context(), id)
		if err != nil {
			ontologyError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, version)
		return
	}

	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "POST to act on a version")
		return
	}
	key, ok := s.authorizeOntology(w, r, authz.Write)
	if !ok {
		return
	}
	store, err := s.ontologies()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch verb {
	case verbPropose:
		s.proposeOntology(w, r, store, key, id)
	case verbApprove:
		s.approveOntology(w, r, store, id)
	case verbPublish:
		s.publishOntology(w, r, store, key, id)
	}
}

// proposeRequest names the job whose run wanted types this vocabulary lacks.
type proposeRequest struct {
	// Job is the finished job to read the proposals out of.
	Job string `json:"job"`
	// Part is which part of the vocabulary the job read its corpus under.
	// Empty means prose, which is CreateJob's own default.
	Part string `json:"part,omitempty"`
	Note string `json:"note,omitempty"`
}

func (s *Server) proposeOntology(w http.ResponseWriter, r *http.Request, store *ontologies.Store, key authz.Key, id string) {
	var req proposeRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.Job = strings.TrimSpace(req.Job)
	if req.Job == "" {
		httpError(w, http.StatusBadRequest, "job is required: a proposal is what one run under this vocabulary observed")
		return
	}
	// Through the service's own gate, with the process's credential — loads.go
	// exactly. GetResult is what decides whether a job is finished, and a held
	// job comes back as an error rather than a result.
	ctx := withInternalToken(r.Context(), s.internal)
	res, err := s.alchemy.GetResult(ctx, &alchemyv1.GetResultRequest{JobId: req.Job})
	if err != nil {
		pipelineError(w, err)
		return
	}
	result := wire.ResultFromProto(res)
	version, err := store.Propose(r.Context(), id, req.Job, req.Part, result.Proposals, key.ID, req.Note)
	if err != nil {
		ontologyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, version)
}

// approveRequest is the decision.
type approveRequest struct {
	// Accept names types from what was proposed against this version.
	Accept []string `json:"accept"`
	// By is the person. It is in the body and not taken from the key because
	// it is the one field here that has to survive an argument two years from
	// now, and "whoever held the operator key" is not an answer to who decided
	// what a type means.
	By   string `json:"by"`
	Note string `json:"note,omitempty"`
	// ID overrides the new version's id. Empty increments the version half.
	ID string `json:"id,omitempty"`
	// Part overrides which part the accepted types are declared in. Empty is
	// the part recorded when the proposals were.
	Part string `json:"part,omitempty"`
}

func (s *Server) approveOntology(w http.ResponseWriter, r *http.Request, store *ontologies.Store, id string) {
	var req approveRequest
	if !decodeBody(w, r, &req) {
		return
	}
	version, err := store.Approve(r.Context(), id, ontologies.Approval{
		Accept: req.Accept, By: req.By, Note: req.Note, NewID: req.ID, Part: req.Part,
	})
	if err != nil {
		ontologyError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, version)
}

// publishRequest makes a version current.
type publishRequest struct {
	// By is who decided. Empty takes the key's id: publishing is an
	// operational act by whoever holds the door, where approving is a
	// judgement about meaning and needs a person's name.
	By   string `json:"by,omitempty"`
	Note string `json:"note,omitempty"`
}

func (s *Server) publishOntology(w http.ResponseWriter, r *http.Request, store *ontologies.Store, key authz.Key, id string) {
	var req publishRequest
	if !decodeBody(w, r, &req) {
		return
	}
	by := strings.TrimSpace(req.By)
	if by == "" {
		by = key.ID
	}
	version, retired, err := store.Publish(r.Context(), id, by, req.Note)
	if err != nil {
		ontologyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": version,
		"retired": retired,
		"by":      by,
	})
}

// decodeBody reads a request body, refusing a misspelled field for loads.go's
// reason: a client who wrote "jobb" believes they named a job.
func decodeBody(w http.ResponseWriter, r *http.Request, into any) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil && !errors.Is(err, io.EOF) {
		httpError(w, http.StatusBadRequest, "body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	body, err := jsonMarshal(v)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// ontologyError turns the store's refusals into the statuses they deserve.
// They are values rather than strings so this reads them by identity.
func ontologyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ontologies.ErrNotFound):
		httpError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ontologies.ErrExists), errors.Is(err, ontologies.ErrState):
		httpError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ontologies.ErrUnsigned), errors.Is(err, ontologies.ErrInvalid):
		httpError(w, http.StatusBadRequest, err.Error())
	default:
		httpError(w, http.StatusInternalServerError, err.Error())
	}
}

// pipelineError is loads.go's mapping of a GetResult failure, kept identical:
// a job that is held is a conflict a person resolves, not a server fault.
func pipelineError(w http.ResponseWriter, err error) {
	st, _ := status.FromError(err)
	switch st.Code() {
	case codes.NotFound:
		httpError(w, http.StatusNotFound, st.Message())
	case codes.FailedPrecondition:
		httpError(w, http.StatusConflict, st.Message())
	case codes.ResourceExhausted:
		httpError(w, http.StatusRequestEntityTooLarge, st.Message())
	default:
		httpError(w, http.StatusBadGateway, st.Message())
	}
}
