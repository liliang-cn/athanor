package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	alchemycdb "github.com/liliang-cn/alchemy/connectors/cortexdb"
	"github.com/liliang-cn/alchemy/pkg/sink"
	"github.com/liliang-cn/alchemy/pkg/wire"
	alchemyv1 "github.com/liliang-cn/alchemy/proto/alchemy/v1"
	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
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
// When the decision ledger exists, this is its first kind of entry: who loaded
// what, from which job, when.

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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"job":    req.Job,
		"load":   req.Load,
		"by":     key.ID,
		"report": report,
	})
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
