package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	alchemycdb "github.com/liliang-cn/alchemy/connectors/cortexdb"
	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
)

// The catalogue, and the one verb that destroys.
//
//	GET    /athanor/loads         what this brain holds, newest first
//	DELETE /athanor/loads/{name}  a load should not be here
//
// Both were missing, and the pair of them being missing is the thing worth
// saying: a load could be written and never listed and never removed. The only
// way to say anything about a load already in the brain was to load another
// graph over it, which answers "this one is wrong" with "here is a different
// one" — and there is not always a different one. A store nobody can enumerate
// is a store whose contents are known to whoever wrote them and to no one else,
// which on a shared brain is nobody.
//
// A drop is signed. Every act in this product that changes what the brain
// stands on names a person, and a removal is the act where that matters most:
// what it takes is not recoverable from anything else here, so the entry in
// the ledger is the only place the reason will survive. It is also the only
// route in Athanor whose entry is *about* something that is no longer there —
// the ledger keeps it exactly for that.

// loadDropRequest is the body of DELETE /athanor/loads/{name}.
type loadDropRequest struct {
	// By is the person removing it. Required. The key is recorded as the
	// actor either way; this is the name a reader will want when the records
	// are gone and the entry is what is left.
	By string `json:"by"`
	// Why is the reason, kept verbatim. Required for the same reason a
	// livedb unmask requires one: a removal nobody had to justify is one
	// nobody will remember the reason for.
	Why string `json:"why"`
}

// handleLoadsCatalogue serves GET /athanor/loads.
func (s *Server) handleLoadsCatalogue(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeLoads(w, r, authz.Read); !ok {
		return
	}
	runs, err := alchemycdb.New(s.db, alchemycdb.Options{}).Runs(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("list loads: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"loads": runs, "count": len(runs)})
}

// handleLoadDrop serves DELETE /athanor/loads/{name}.
func (s *Server) handleLoadDrop(w http.ResponseWriter, r *http.Request, name string) {
	key, ok := s.authorizeLoads(w, r, authz.Write)
	if !ok {
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		httpError(w, http.StatusBadRequest, "DELETE /athanor/loads/{name} names the load to remove")
		return
	}
	var req loadDropRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.By) == "" {
		httpError(w, http.StatusBadRequest,
			"a removal must name who is removing it: what this takes is not recoverable from anything else here, "+
				"and the ledger entry is the only place the answer will survive")
		return
	}
	if strings.TrimSpace(req.Why) == "" {
		httpError(w, http.StatusBadRequest,
			"a removal must say why: the records are gone afterwards and the reason is what a later reader has instead of them")
		return
	}

	report, err := alchemycdb.New(s.db, alchemycdb.Options{RunID: name}).Drop(r.Context())
	if err != nil {
		if errors.Is(err, alchemycdb.ErrNoRun) {
			httpError(w, http.StatusNotFound, err.Error())
			return
		}
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("drop %s: %v", name, err))
		return
	}

	// Same ordering as a load: the removal happened, and the record of it is
	// not allowed to undo it. A ledger write that fails leaves a load that is
	// gone, so the failure is reported in the answer and the status stays 200.
	answer := map[string]any{"load": name, "by": strings.TrimSpace(req.By), "actor": key.ID, "removed": report}
	if decision, err := s.recordDrop(r.Context(), key.ID, name, req, report); err != nil {
		log.Printf("athanor: ledger: drop %s: %v", name, err)
		answer["ledger_error"] = err.Error()
	} else {
		answer["decision"] = decision
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

// recordDrop writes the removal into the ledger.
//
// Its premise is the load's own entry, when there is one: the drop rests on
// the load, so DecisionChain walks from "this is not here any more" back to
// "this is what was put here and by whom". The load entry's id is derived from
// the job and the load name and the job is not known here, so the premise is
// the entry itself found by subject — the ledger drops a premise it cannot
// resolve, which is the right answer for a load that predates the ledger.
func (s *Server) recordDrop(ctx context.Context, actor, name string, req loadDropRequest, report alchemycdb.Report) (string, error) {
	detail := map[string]any{
		"load": name,
		"by":   strings.TrimSpace(req.By),
		"why":  strings.TrimSpace(req.Why),
		// What it took, so the entry is a record of the thing and not just of
		// the intention. Batches is the store's own count of writes; it is the
		// only number a drop has, because what is gone cannot be counted after.
		"batches": report.Batches,
	}
	if report.Digest != "" {
		detail["digest"] = report.Digest
	}
	entry := ledgerEntry{
		ID:      dropDecisionID(name),
		Kind:    ledgerKindLoad,
		Actor:   actor,
		Verdict: "dropped",
		Subject: name,
		Note: fmt.Sprintf("dropped the load %s from the brain: %s",
			name, strings.TrimSpace(req.Why)),
		Detail: detail,
	}
	rec, err := s.ledger.record(ctx, entry)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// authorizeLoads is the gate both routes stand in, and the same operation the
// load itself takes. Listing is a read, writing and removing are writes.
func (s *Server) authorizeLoads(w http.ResponseWriter, r *http.Request, access authz.Access) (authz.Key, bool) {
	key, code, msg := s.requestKey(r)
	if code != 0 {
		httpError(w, code, msg)
		return authz.Key{}, false
	}
	if err := key.AuthorizeOperation("athanor.loads", authz.Method{Access: access}); err != nil {
		httpError(w, http.StatusForbidden, err.Error())
		return authz.Key{}, false
	}
	return key, true
}

// dropDecisionID names the entry for a removal. It is keyed on the load rather
// than on the job, because the job is not what was removed and may itself be
// long gone.
func dropDecisionID(load string) string { return "athanor:load:drop:" + load }
