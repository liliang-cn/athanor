package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	alchemycdb "github.com/liliang-cn/alchemy/connectors/cortexdb"
	"github.com/liliang-cn/alchemy/pkg/sink"
	"github.com/liliang-cn/alchemy/pkg/wire"
	alchemyv1 "github.com/liliang-cn/alchemy/proto/alchemy/v1"
	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// The one verb Athanor adds: a finished job goes into the brain.
//
// alchemy returns and forgets; CortexDB remembers. Between them is an act
// nobody performs automatically, on purpose. A job that finished is not in the
// brain until somebody says so, and a job that is held cannot be said so at
// all — GetResult refuses it, and this route asks GetResult rather than
// reaching past it. The brain therefore holds only graphs that were finished,
// and finished means reviewed when review was owed.
//
// It is a write, so a read-only key is refused before anything is fetched.
// It is also the ledger's first kind of entry — who loaded what, from which
// job, when, and what the load report counted — recorded after sink.Load
// returns and never in front of it.

// loadRequest is the body of POST /athanor/loads.
type loadRequest struct {
	// Job is the finished job to load.
	Job string `json:"job"`
	// Load names the import in the brain. Empty takes the job id.
	Load string `json:"load,omitempty"`
	// Collection is the vector collection chunk embeddings go into. Empty
	// takes the connector's default.
	Collection string `json:"collection,omitempty"`
	// Replace overwrites a load of the same name holding a different graph.
	Replace bool `json:"replace,omitempty"`
}

func (s *Server) handleLoads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "POST a {job, load} document")
		return
	}
	key, code, msg := s.httpKey(r.Header.Get("Authorization"))
	if code != 0 {
		httpError(w, code, msg)
		return
	}
	if err := key.AuthorizeOperation("athanor.loads", authz.Method{Access: authz.Write}); err != nil {
		httpError(w, http.StatusForbidden, err.Error())
		return
	}

	var req loadRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "body: "+err.Error())
		return
	}
	req.Job = strings.TrimSpace(req.Job)
	if req.Job == "" {
		httpError(w, http.StatusBadRequest, "job is required")
		return
	}
	if req.Load == "" {
		req.Load = req.Job
	}

	// Through the service's own gate, with the process's credential: the
	// caller was authorized above, and GetResult is what decides whether a
	// job is finished. A held job comes back as an error, not a graph.
	ctx := withInternalToken(r.Context(), s.internal)
	res, err := s.alchemy.GetResult(ctx, &alchemyv1.GetResultRequest{JobId: req.Job})
	if err != nil {
		st, _ := status.FromError(err)
		switch st.Code() {
		case codes.NotFound:
			httpError(w, http.StatusNotFound, st.Message())
		case codes.FailedPrecondition:
			// Held, or still running. The message names which.
			httpError(w, http.StatusConflict, st.Message())
		case codes.ResourceExhausted:
			httpError(w, http.StatusRequestEntityTooLarge, st.Message()+" — StreamResult is not wired into loads yet")
		default:
			httpError(w, http.StatusBadGateway, st.Message())
		}
		return
	}

	loader := alchemycdb.New(s.db, alchemycdb.Options{RunID: req.Load, Collection: req.Collection})
	report, err := sink.Load(r.Context(), loader, wire.ResultFromProto(res), sink.Options{Load: req.Load, Replace: req.Replace})
	if err != nil {
		// The name already holds a different graph and Replace was not
		// said: a refusal, not a failure, and the caller can say Replace.
		if errors.Is(err, sink.ErrExists) {
			httpError(w, http.StatusConflict, err.Error())
			return
		}
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("load %s: %v", req.Load, err))
		return
	}
	// The graph is in the brain. Everything from here is the record of that,
	// and the record is not allowed to undo it: a ledger write that fails
	// leaves a load that succeeded, so the failure is reported in the answer
	// and logged, and the status stays 200. Rolling back a load because its
	// audit entry did not write would be a store that loses data to protect a
	// note about the data.
	answer := map[string]any{
		"job":    req.Job,
		"load":   req.Load,
		"by":     key.ID,
		"report": report,
	}
	if decision, err := s.recordLoad(r.Context(), key.ID, req, report); err != nil {
		log.Printf("athanor: ledger: load %s of job %s: %v", req.Load, req.Job, err)
		answer["ledger_error"] = err.Error()
	} else {
		answer["decision"] = decision
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

// recordLoad writes the load into the ledger and reports the entry's id.
//
// Its premises are the review decisions this job's own findings were answered
// with, when there are any: the load rests on them, and they are decisions, so
// DecisionChain walks from the load back through every judgement that
// unblocked it. They are found without a query CortexDB does not have —
// alchemy names the job's items, and a review entry's id is derived from the
// job and the item (ledger.go) — and any that were never recorded are dropped
// by the ledger rather than costing the entry.
func (s *Server) recordLoad(ctx context.Context, actor string, req loadRequest, report sink.Report) (string, error) {
	detail := map[string]any{
		"job":       req.Job,
		"load":      req.Load,
		"entities":  report.Entities,
		"relations": report.Relations,
		"chunks":    report.Chunks,
		"vectors":   report.Vectors,
	}
	if report.Digest != "" {
		detail["digest"] = report.Digest
	}
	if report.Converged {
		detail["converged"] = true
	}
	for k, v := range map[string]int{
		"corroborated": report.Corroborated, "violations": report.Violations,
		"duplicates": report.Duplicates, "guesses": report.Guesses,
		"unread": report.Unread, "supersessions": report.Supersessions,
	} {
		if v != 0 {
			detail[k] = v
		}
	}
	verdict := "loaded"
	if report.Converged {
		verdict = "converged"
	}
	entry := ledgerEntry{
		ID:      loadDecisionID(req.Job, req.Load),
		Kind:    ledgerKindLoad,
		Actor:   actor,
		Verdict: verdict,
		// The job id is what this was about, and it is a node only if
		// something else in the brain made it one; ledger.go decides.
		Subject: req.Job,
		Note: fmt.Sprintf("loaded job %s into the brain as %s: %d entities, %d relations, %d chunks",
			req.Job, req.Load, report.Entities, report.Relations, report.Chunks),
		Detail:   detail,
		Premises: s.reviewPremises(ctx, req.Job),
	}
	rec, err := s.ledger.record(ctx, entry)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// reviewPremises names the ledger entries for this job's findings.
//
// The ids are derived, not searched for: a review entry is
// decision:athanor:review:<job>:<item>, so knowing the job's item ids is
// knowing them. The ledger drops the ones that are not there, so a job nobody
// reviewed contributes nothing and a job reviewed through a door that failed
// to record contributes what it did record.
func (s *Server) reviewPremises(ctx context.Context, job string) []string {
	items := s.findingsByID(withInternalToken(ctx, s.internal), job)
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, cortexdb.DecisionID(reviewDecisionID(job, id)))
	}
	sort.Strings(ids)
	return ids
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
