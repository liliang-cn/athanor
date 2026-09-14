package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	alchemycdb "github.com/liliang-cn/alchemy/connectors/cortexdb"
	"github.com/liliang-cn/alchemy/pkg/sink"
	"github.com/liliang-cn/alchemy/pkg/wire"
	alchemyv1 "github.com/liliang-cn/alchemy/proto/alchemy/v1"
	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
	"google.golang.org/protobuf/encoding/protojson"
)

// Saying something directly, and saying what it replaces.
//
//	POST /athanor/assertions
//
// The gap this closes was a sentence in an error message. A reviewer who found
// a wrong record in a delivered graph was told to "assert the correction and
// name what it retires — POST /v1/assertions with supersedes", and that route
// is real and answers a Result. Nothing could then put the Result anywhere:
// /athanor/loads takes a job id and reads the pipeline's own store, an
// assertion is not a job, and the correction stopped at the edge of the brain.
// The instruction was correct and the road ended.
//
// So this is one act: state it, put it in, and act on what it retires.
//
// # Acting on a supersession
//
// alchemy carries a supersession and never performs one, and it is right not
// to: §4 leaves it holding no graph, and a producer that could delete another
// producer's fact by naming it would be a pipeline with write access to
// everybody's brain. Its own comment names who may act — "a store can act on
// it deliberately" — and Athanor is the store.
//
// Acting is marking, not deleting. The retired record keeps its place, its
// provenance and its citations, and gains the contract's own words for where
// it stands: _state superseded, _why the reason given, _by whoever gave it,
// _at when. A reader meeting the old fact meets it with the retirement on it;
// a reader who deleted it would meet nothing at all and could not tell that
// from a fact nobody ever wrote. Everything downstream — the tally, the graph,
// citations already issued — goes on working, which is the point.
//
// A record named in supersedes and not found in the brain is reported as not
// found. It is the one outcome worth being loud about: the assertion landed,
// the retirement did not, and a route that answered 200 for both would leave
// somebody believing the old answer was marked when it is still standing.

// assertRequest is the body. Entities and relations are alchemy's own wire
// shapes, because the whole value of this route is that it is the same
// vocabulary and the same validation as everything else that reaches the brain.
type assertRequest struct {
	Entities  []json.RawMessage `json:"entities,omitempty"`
	Relations []json.RawMessage `json:"relations,omitempty"`
	// By is the person asserting. Required by the pipeline, and required here
	// before the call is made so the refusal names the field rather than
	// arriving as a preflight code.
	By   string `json:"by"`
	Note string `json:"note,omitempty"`
	// Ontology is the document to validate against. Empty is whatever the
	// pipeline's default is — which for a store with a published vocabulary is
	// the thing a caller almost always wants, so `lineage` is the easier way
	// to say it.
	Ontology string `json:"ontology,omitempty"`
	// Lineage names a published vocabulary in this Athanor to validate
	// against, so a caller does not have to paste a document they already gave
	// this server. Ignored when Ontology is set.
	Lineage string `json:"lineage,omitempty"`
	Part    string `json:"part,omitempty"`
	// Supersedes names records this assertion retires, and why.
	Supersedes []supersedesRequest `json:"supersedes,omitempty"`
	// Load names the import in the brain. Empty derives one from the digest of
	// what was asserted, which makes re-sending the same assertion converge on
	// the same load rather than piling up near-copies.
	Load       string `json:"load,omitempty"`
	Collection string `json:"collection,omitempty"`
}

type supersedesRequest struct {
	// Retires is the record in the brain: a node id or an edge id, as the
	// graph spells them. It is the brain's id and not alchemy's, because this
	// is the half of the sentence that is about the store.
	Retires string `json:"retires"`
	Reason  string `json:"reason,omitempty"`
}

// retirement is what happened to one named record.
type retirement struct {
	Retires string `json:"retires"`
	// Found says whether the brain holds it. False means nothing was marked,
	// and the assertion still landed.
	Found  bool   `json:"found"`
	Kind   string `json:"kind,omitempty"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) handleAssertions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "POST an {entities, relations, by, supersedes} document")
		return
	}
	key, code, msg := s.requestKey(r)
	if code != 0 {
		httpError(w, code, msg)
		return
	}
	// The same operation a load authorizes against. Asserting a fact and
	// loading a graph put records in the same brain under the same contract,
	// and a key allowed to do one and not the other would be a distinction
	// nobody could explain to the person holding it.
	if err := key.AuthorizeOperation("athanor.loads", authz.Method{Access: authz.Write}); err != nil {
		httpError(w, http.StatusForbidden, err.Error())
		return
	}

	var req assertRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.By = strings.TrimSpace(req.By)
	if req.By == "" {
		httpError(w, http.StatusBadRequest, "by is required: a fact asserted by nobody cannot be written into provenance")
		return
	}
	if len(req.Entities) == 0 && len(req.Relations) == 0 {
		httpError(w, http.StatusBadRequest, "nothing to assert: give entities, relations, or both")
		return
	}

	ontology, err := s.assertionOntology(r.Context(), req)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	msgReq, err := assertProto(req, ontology)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Through the service's own gate with the process's credential, exactly as
	// loads.go reads a result: the validation, the preflight and the refusals
	// are the pipeline's, and a second opinion here would be a second thing to
	// keep in step with them.
	ctx := withInternalToken(r.Context(), s.internal)
	res, err := s.alchemy.Assert(ctx, msgReq)
	if err != nil {
		pipelineError(w, err)
		return
	}

	result := wire.ResultFromProto(res)
	load := strings.TrimSpace(req.Load)
	if load == "" {
		load = assertionLoad(sink.Digest(result), req.By)
	}
	loader := alchemycdb.New(s.db, alchemycdb.Options{RunID: load, Collection: req.Collection})
	// Replace, and the reason is the load name: it is derived from the digest
	// of what was asserted, so the same name can only ever hold the same
	// graph, and replace makes re-sending an assertion converge instead of
	// refusing. A caller who named the load themselves gets the same
	// behaviour, which is the one they asked for by reusing a name.
	report, err := sink.Load(r.Context(), loader, result, sink.Options{Load: load, Replace: true})
	if err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Sprintf("assert into %s: %v", load, err))
		return
	}

	// The facts are in. Everything from here is about records that were
	// already there, and a failure in it does not undo the assertion — the
	// same rule loads.go states about its ledger write, for the same reason.
	retired := s.retire(r.Context(), req)

	answer := map[string]any{
		"load":    load,
		"by":      req.By,
		"report":  report,
		"retired": retired,
	}
	if decision, err := s.recordAssertion(r.Context(), key.ID, req, load, report, retired); err != nil {
		log.Printf("athanor: ledger: assertion %s: %v", load, err)
		answer["ledger_error"] = err.Error()
	} else {
		answer["decision"] = decision
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

// assertionOntology resolves which vocabulary to validate against.
func (s *Server) assertionOntology(ctx context.Context, req assertRequest) (string, error) {
	if strings.TrimSpace(req.Ontology) != "" {
		return req.Ontology, nil
	}
	lineage := strings.TrimSpace(req.Lineage)
	if lineage == "" {
		return "", nil
	}
	store, err := s.ontologies()
	if err != nil {
		return "", err
	}
	version, err := store.Current(ctx, lineage)
	if err != nil {
		return "", fmt.Errorf("lineage %q: %w", lineage, err)
	}
	return string(version.Document), nil
}

// assertProto builds the pipeline's request out of the body's raw records.
//
// The entity and relation JSON is handed through rather than re-typed here.
// protojson is what the pipeline's own HTTP route would have used on the same
// bytes, so a document that works against /v1/assertions works against this,
// and a field this file has never heard of survives.
func assertProto(req assertRequest, ontology string) (*alchemyv1.AssertRequest, error) {
	out := &alchemyv1.AssertRequest{
		By: req.By, Note: req.Note, Ontology: ontology, Part: req.Part,
	}
	for i, raw := range req.Entities {
		var e alchemyv1.Entity
		if err := protojson.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("entity %d: %w", i+1, err)
		}
		out.Entities = append(out.Entities, &e)
	}
	for i, raw := range req.Relations {
		var rel alchemyv1.Relation
		if err := protojson.Unmarshal(raw, &rel); err != nil {
			return nil, fmt.Errorf("relation %d: %w", i+1, err)
		}
		out.Relations = append(out.Relations, &rel)
	}
	// The supersessions travel with the assertion as well as being acted on
	// here. What this route marks is the brain's copy; what the pipeline
	// carries is the claim itself, which lands on the load's own completion
	// record and is what a reader months from now finds beside the graph.
	for _, sup := range req.Supersedes {
		retires := strings.TrimSpace(sup.Retires)
		if retires == "" {
			return nil, fmt.Errorf("a supersedes entry names no record")
		}
		out.Supersedes = append(out.Supersedes, &alchemyv1.Supersedes{Retires: retires, Reason: sup.Reason})
	}
	return out, nil
}

// assertionLoad names an assertion's import.
//
// The digest of what was asserted, so that re-sending the same correction
// converges on one load instead of leaving a trail of near-identical ones, and
// the asserter's name so that a person reading the catalogue sees who put it
// there without opening anything.
func assertionLoad(digest, by string) string {
	name := "assertion:" + ledgerTrim(by)
	if digest == "" {
		return name + ":" + time.Now().UTC().Format("20060102T150405Z")
	}
	if len(digest) > 12 {
		digest = digest[:12]
	}
	return name + ":" + digest
}

// retire marks each named record as superseded, and reports what it found.
func (s *Server) retire(ctx context.Context, req assertRequest) []retirement {
	out := make([]retirement, 0, len(req.Supersedes))
	stamp := time.Now().UTC().Format(time.RFC3339)
	for _, sup := range req.Supersedes {
		id := strings.TrimSpace(sup.Retires)
		rec := retirement{Retires: id, Reason: sup.Reason}
		marks := map[string]string{
			cortexdb.KeyState: "superseded",
			cortexdb.KeyBy:    req.By,
			cortexdb.KeyAt:    stamp,
		}
		// _why is the reason a record is held or refused, "in words a person
		// can act on". A retirement with no reason writes none rather than an
		// empty string: the key being absent says nobody gave one, and an
		// empty value says somebody gave nothing.
		if reason := strings.TrimSpace(sup.Reason); reason != "" {
			marks[cortexdb.KeyWhy] = reason
		}
		if node, err := s.db.Graph().GetNode(ctx, id); err == nil && node != nil {
			rec.Found, rec.Kind = true, "node"
			node.Properties = withMarks(node.Properties, marks)
			if err := s.db.Graph().UpsertNode(ctx, node); err != nil {
				rec.Error = err.Error()
			}
			out = append(out, rec)
			continue
		}
		if edges, err := s.db.Graph().GetEdgesBatch(ctx, []string{id}); err == nil && len(edges) == 1 {
			rec.Found, rec.Kind = true, "edge"
			edges[0].Properties = withMarks(edges[0].Properties, marks)
			if err := s.db.Graph().UpsertEdge(ctx, edges[0]); err != nil {
				rec.Error = err.Error()
			}
			out = append(out, rec)
			continue
		}
		// Not found is reported and not an error. The assertion is in the
		// brain either way, and a caller who mistyped an id has to be told
		// which half of their sentence landed.
		out = append(out, rec)
	}
	return out
}

// withMarks writes the contract keys onto a record's properties without
// touching anything else it carries.
func withMarks(props map[string]any, marks map[string]string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	for k, v := range marks {
		props[k] = v
	}
	return props
}

// recordAssertion writes the act into the ledger.
//
// Its premises are the records it retires, which is what makes the chain
// readable in the direction a reader asks it: standing at a superseded fact,
// decision_chain walks to the assertion that ended it and to whoever signed
// that. A named record the brain does not hold is not a premise — the ledger
// drops premises that do not exist, and this filters first so the entry says
// what it actually rests on.
func (s *Server) recordAssertion(
	ctx context.Context, actor string, req assertRequest, load string,
	report sink.Report, retired []retirement,
) (string, error) {
	var premises []string
	var missing []string
	for _, rec := range retired {
		if rec.Found && rec.Kind == "node" {
			premises = append(premises, rec.Retires)
			continue
		}
		if !rec.Found {
			missing = append(missing, rec.Retires)
		}
	}
	detail := map[string]any{
		"load":      load,
		"by":        req.By,
		"entities":  report.Entities,
		"relations": report.Relations,
	}
	if report.Digest != "" {
		detail["digest"] = report.Digest
	}
	if len(retired) > 0 {
		detail["retires"] = len(retired)
	}
	if len(missing) > 0 {
		detail["not_found"] = missing
	}
	note := fmt.Sprintf("%s asserted %d entities and %d relations as %s",
		req.By, report.Entities, report.Relations, load)
	if len(retired) > 0 {
		note += fmt.Sprintf(", retiring %d record(s)", len(retired))
	}
	if req.Note != "" {
		note += ": " + req.Note
	}
	rec, err := s.ledger.record(ctx, ledgerEntry{
		ID:       assertionDecisionID(load),
		Kind:     ledgerKindAssert,
		Actor:    actor,
		Verdict:  "asserted",
		Subject:  firstFound(retired),
		Note:     note,
		Detail:   detail,
		Premises: premises,
	})
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// firstFound is the record this act was most about: the one it retired. An
// assertion that retires nothing is about nothing already in the brain, and
// says so by naming no subject rather than naming the load, which is a node
// only in the sense that the ledger made it one.
func firstFound(retired []retirement) string {
	for _, rec := range retired {
		if rec.Found {
			return rec.Retires
		}
	}
	return ""
}
