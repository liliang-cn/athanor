package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/liliang-cn/athanor/pkg/livedb"
	"github.com/liliang-cn/athanor/pkg/ontologies"
	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
	"github.com/liliang-cn/cortexdb/v2/pkg/graph"
)

// Athanor's own acts, as ledger entries — and the door that reads them back.
//
// CortexDB v2.98.0 made a decision a first-class record: a node typed
// Decision, `based_on` / `about` / `supersedes` edges, graded `verified`
// because a named actor signed it, with DecisionChain and Precedents to read
// it back. Everything an agent decided through decision_record over MCP or
// gRPC has been landing there since. Nothing Athanor itself did was.
//
// Athanor performs five kinds of act and four of them are exactly what a
// ledger is for: a load (a graph entered the brain), a review decision (a
// person accepted, rejected or edited a finding), an ontology act (a
// vocabulary was drafted, proposed against, approved, published, retired),
// and a live-database act (a plan was proposed, signed, and run against a
// database somebody else runs). The fifth — an agent's own decision — already
// arrives. This file records the other four in the same store, in the same
// shape, so one query answers all five.
//
// # Who the actor is
//
// The actor is always the **key id**: `authz.Key.ID`, the identity the policy
// knows and an operator can revoke. It becomes the decision's `actor`
// property and, through RecordDecision, the knowledge contract's `_by` on the
// node and on every edge the decision writes.
//
// Several of these acts also carry a free-text `by` — the `by` on alchemy's
// ReviewDecision, the `by` an ontology approval is signed with. That is a
// person's claim about themselves, typed into a request body, checked by
// nobody. It is kept, verbatim, in the note. So `_by` answers "which
// credential did this", the note answers "who said they did it", and when the
// two disagree the disagreement is itself in the record — which is the only
// useful thing to do with an unverifiable name. Nothing here ever promotes a
// free-text `by` to the actor: a ledger whose actor field can be typed by the
// caller is a ledger that cannot be used as evidence.
//
// # Why an entry usually has no `about` edge
//
// RecordDecision refuses a subject that is not already a node, and it is
// right to: an `about` edge pointing at nothing reads as a link to a fact and
// is not one. At the moment Athanor acts, the thing it acted on is usually
// not a node yet — a job id never is, and a conflict subject is not, because
// review happens *before* the load by construction. Minting a stub under the
// raw subject id would be worse than leaving it out: alchemy's connector
// namespaces every node it writes by the run (`entity:alchemy:<run>:<id>`),
// so the stub would never be joined by the load and would sit beside the real
// entity for ever, indistinguishable in a graph view from a fact.
//
// So the subject is set only when it already names a node — which is the case
// for an agent recording a decision about something on the shelf — and
// otherwise the job, the load, the version and the conflict subject are
// carried in the entry's own id and in the note's structured detail, both of
// which are exact and neither of which claims to be a graph edge.
//
// # Ids are deterministic
//
// A load's entry is `decision:athanor:load:<job>:<load>`, a review's is
// `decision:athanor:review:<job>:<item>`, an ontology act's is
// `decision:athanor:ontology:<act id>`, and a live-database plan's is
// `decision:athanor:livedb:plan:<plan>` with its runs at
// `decision:athanor:livedb:run:<run>`. RecordDecision treats a supplied id
// as an upsert, so re-running a load under the same name updates one entry
// rather than growing a second — the same property that lets an agent replay
// a transcript without doubling its ledger. It is also what lets a load find
// the review decisions that unblocked it without a query CortexDB does not
// have: it knows the job's item ids, so it knows their entries' ids.

// The kinds Athanor writes. `load` and `review` are CortexDB's own words, so
// Athanor's entries line up with everybody else's precedents. The ontology
// verbs are namespaced because they are this product's workflow and not a
// shape a general ledger has an opinion about.
const (
	ledgerKindLoad     = cortexdb.DecisionKindLoad
	ledgerKindReview   = cortexdb.DecisionKindReview
	ledgerKindOntology = "ontology." // + the verb
)

func loadDecisionID(job, load string) string   { return "athanor:load:" + job + ":" + load }
func reviewDecisionID(job, item string) string { return "athanor:review:" + job + ":" + item }
func ontologyDecisionID(actID string) string   { return "athanor:ontology:" + actID }
func livedbPlanDecisionID(plan string) string  { return "athanor:livedb:plan:" + plan }
func livedbRunDecisionID(run string) string    { return "athanor:livedb:run:" + run }
func ledgerTrim(s string) string               { return strings.TrimSpace(s) }

// ledgerEntry is one act of Athanor's, in the shape RecordDecision takes.
type ledgerEntry struct {
	// ID is the entry's own id, unprefixed. Deterministic in what the act was
	// about, so the same act recorded twice is one entry.
	ID string
	// Kind groups the entry with its precedents.
	Kind string
	// Actor is the key id. Never a name out of a request body.
	Actor string
	// Verdict is the outcome in the act's own word: "loaded", "accept",
	// "published".
	Verdict string
	// Subject is what the act was about. It is written as an `about` edge only
	// if it already names a node; see the file comment.
	Subject string
	// Note is the entry in words, and Detail is what a reader would otherwise
	// have to parse out of it. They are rendered as one string because that is
	// what a decision's note is.
	Note   string
	Detail map[string]any
	// Premises are ids that must already exist. The caller filters; a premise
	// that does not exist would cost the whole entry.
	Premises []string
}

// note renders the entry the way it is stored: a line a person reads, then
// the structured detail on its own line. Go marshals a map with its keys
// sorted, so the same act renders the same bytes twice — which is what makes
// re-recording an entry converge instead of churning.
func (e ledgerEntry) note() string {
	if len(e.Detail) == 0 {
		return e.Note
	}
	body, err := json.Marshal(e.Detail)
	if err != nil {
		return e.Note
	}
	return e.Note + "\n" + string(body)
}

// ledger is where Athanor's own acts go. It is an interface for one reason:
// every caller of it is an act that must stand whether or not the record of it
// does, and a test proves that by making the write fail.
type ledger interface {
	record(ctx context.Context, entry ledgerEntry) (cortexdb.DecisionRecord, error)
}

// brainLedger is the real one: the same brain the process serves.
type brainLedger struct{ db *cortexdb.DB }

func (l brainLedger) record(ctx context.Context, entry ledgerEntry) (cortexdb.DecisionRecord, error) {
	actor := ledgerTrim(entry.Actor)
	if actor == "" {
		return cortexdb.DecisionRecord{}, fmt.Errorf("athanor: ledger: no actor — the key id is what signs an entry")
	}
	req := cortexdb.DecisionRecordRequest{
		ID:       entry.ID,
		Kind:     entry.Kind,
		Actor:    actor,
		Verdict:  entry.Verdict,
		Note:     entry.note(),
		Premises: l.existing(ctx, entry.Premises),
		Source:   ledgerSource,
	}
	// Only if the brain already holds it. See the file comment on `about`.
	if subject := ledgerTrim(entry.Subject); subject != "" && l.holds(ctx, subject) {
		req.Subject = subject
	}
	return l.db.RecordDecision(ctx, req)
}

// ledgerSource is where these entries came from, for the knowledge contract's
// `_source`. "decision-ledger" is CortexDB's default and would be true; this
// is truer, and it is what tells an auditor that the door wrote the entry
// rather than an agent calling decision_record through it.
const ledgerSource = "athanor"

func (l brainLedger) holds(ctx context.Context, id string) bool {
	_, err := l.db.Graph().GetNode(ctx, id)
	return err == nil
}

// existing keeps the premises the brain actually holds.
//
// RecordDecision refuses an entry naming a premise that does not exist, which
// is the right rule for a caller that believes it has one. Athanor's callers
// do not: a load offers the review decisions it *thinks* answered the job, and
// the honest answer to one that was never recorded is an entry with one fewer
// premise, not a load that reports a ledger failure.
func (l brainLedger) existing(ctx context.Context, ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	nodes, err := l.db.Graph().GetNodesBatch(ctx, ids)
	if err != nil {
		return nil
	}
	found := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		found[n.ID] = true
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if found[id] {
			out = append(out, id)
		}
	}
	return out
}

// ontologyLedger is pkg/ontologies' Ledger, implemented against the brain.
//
// It holds the Server rather than the ledger, because the store it is given to
// is built once per brain and cached (ontologies.go) while the ledger is a
// field a test replaces; reading it at call time is what keeps the two from
// drifting apart.
type ontologyLedger struct{ srv *Server }

func (l ontologyLedger) Record(ctx context.Context, act ontologies.Act) error {
	kind := act.Kind
	if !strings.HasPrefix(kind, ledgerKindOntology) {
		kind = ledgerKindOntology + kind
	}
	detail := map[string]any{
		"version": act.Subject,
		"act":     act.ID,
	}
	// The name the act was signed with, when it is not the key that called.
	// An approval is the one act here that takes a person's name in the body,
	// and the ledger keeps both rather than choosing.
	if act.Actor != "" && act.Actor != act.Key {
		detail["by"] = act.Actor
	}
	if act.Note != "" {
		detail["note"] = act.Note
	}
	verb := strings.TrimPrefix(kind, ledgerKindOntology)
	entry := ledgerEntry{
		ID:      ontologyDecisionID(act.ID),
		Kind:    kind,
		Actor:   ledgerTrim(firstNonBlank(act.Key, act.Actor)),
		Verdict: verb,
		Subject: act.Subject,
		Note:    fmt.Sprintf("%s the vocabulary %s: %s", verb, act.Subject, act.Note),
		Detail:  detail,
	}
	_, err := l.srv.ledger.record(ctx, entry)
	return err
}

// livedbLedger is pkg/livedb's Ledger, implemented against the brain. It
// holds the Server for ontologyLedger's reason: the store it is given to is
// built once per brain and cached, while the ledger is a field a test
// replaces, and reading it at call time is what keeps the two from drifting.
//
// # Two acts, one entry
//
// A proposal and the signature that follows it are the same plan, so they are
// the same entry: `athanor:livedb:plan:<plan id>`, whose verdict goes from
// "proposed" to "signed" when somebody signs. RecordDecision treats a
// supplied id as an upsert, which is what makes that an update rather than a
// second entry claiming the plan was proposed twice. A run is its own thing
// and gets its own: `athanor:livedb:run:<run id>`.
//
// # The actor
//
// livedb.Act carries one Actor field, and by the time an act reaches here it
// may hold the name a signer typed into the request body rather than the key
// that presented itself. So the actor is read from the context, where the
// handler parked the key authorization resolved (withCallerKey) — the same
// mechanism the gRPC ledger hooks use, for the same reason. A free-text name
// that disagrees with it is kept in the detail, verbatim, under `by`.
type livedbLedger struct{ srv *Server }

func (l livedbLedger) Record(ctx context.Context, act livedb.Act) error {
	actor := callerKey(ctx).ID
	detail := map[string]any{"act": act.ID}
	if act.Subject != "" {
		detail["subject"] = act.Subject
	}
	// The name the act was signed with, when it is not the key that called.
	// Nothing here promotes it to the actor; a ledger whose actor can be
	// typed by the caller is a ledger that cannot be used as evidence.
	if by := ledgerTrim(act.Actor); by != "" && by != actor {
		detail["by"] = by
	}
	if act.Note != "" {
		detail["note"] = act.Note
	}
	for k, v := range act.Detail {
		if _, taken := detail[k]; !taken {
			detail[k] = v
		}
	}

	// Act.Subject is what the act was about, and it is the only field that
	// says so: Act.ID is a freshly minted id for the act itself, different on
	// every call, so an entry keyed by it would be a new entry every time and
	// a signature would never find the proposal it is amending.
	subject := firstNonBlank(act.Subject, act.ID)
	var id, verdict, note string
	switch act.Kind {
	case livedb.ActPropose:
		id, verdict = livedbPlanDecisionID(subject), "proposed"
		note = fmt.Sprintf("proposed a plan for reading %s: %s", subject, act.Note)
	case livedb.ActSign:
		id, verdict = livedbPlanDecisionID(subject), "signed"
		note = fmt.Sprintf("signed the plan %s: %s", subject, act.Note)
	case livedb.ActRun:
		id, verdict = livedbRunDecisionID(subject), "ran"
		note = fmt.Sprintf("ran the import %s: %s", subject, act.Note)
	default:
		return fmt.Errorf("athanor: ledger: %s is not one of this package's acts", act.Kind)
	}

	entry := ledgerEntry{
		ID:       id,
		Kind:     act.Kind,
		Actor:    actor,
		Verdict:  verdict,
		Subject:  act.Subject,
		Note:     strings.TrimSpace(note),
		Detail:   detail,
		Premises: livedbPremises(act),
	}
	_, err := l.srv.ledger.record(ctx, entry)
	return err
}

// livedbPremises names the entries a livedb act rests on.
//
// A run rests on the signature that permitted it, so that "where did this
// node come from" walks back to a signature and from there to the column it
// was made of. Finding that entry needs one translation, because the two
// packages number things differently: pkg/livedb offers the signing *act's*
// own id, which is minted per act and is not an entry in anybody's ledger,
// while the entry the signature actually wrote is the plan's —
// athanor:livedb:plan:<plan>, the same one the proposal wrote and the
// signature updated. A run's detail names its plan, so the entry is derived
// from that rather than looked up.
//
// Whatever livedb did offer is passed through as well, prefixed the way
// loads.go prefixes the review decisions a load rests on. An id that names no
// entry costs nothing: brainLedger.existing drops the premises the brain does
// not hold, so a premise that was never recorded costs one premise and not
// the whole entry.
func livedbPremises(act livedb.Act) []string {
	ids := make([]string, 0, len(act.Premises)+1)
	if act.Kind == livedb.ActRun {
		if plan, _ := act.Detail["plan"].(string); ledgerTrim(plan) != "" {
			ids = append(ids, livedbPlanDecisionID(ledgerTrim(plan)))
		}
	}
	ids = append(ids, act.Premises...)

	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id = ledgerTrim(id); id == "" {
			continue
		}
		if id = cortexdb.DecisionID(id); !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

func firstNonBlank(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// The read door.
//
// Two routes, both reads, both behind the same key policy as everything else
// (auth.go's httpKey):
//
//	GET /athanor/decisions?kind=&subject=&actor=&limit=   precedents, by actor
//	GET /athanor/decisions/{id}                           one entry's chain
//
// There is no route that writes one. A decision is recorded by performing the
// act it describes — loading a graph, deciding a finding, approving a
// vocabulary — and a door that let a caller write an entry without doing the
// thing would make the ledger a diary.

const ledgerOperation = "athanor.decisions"

const (
	ledgerDefaultLimit = 20
	ledgerMaxLimit     = 200
	// ledgerFetchCap is how many entries a query reads before applying the
	// filters it could not push down.
	//
	// CortexDB's ledger can be asked for one kind, one subject or one actor,
	// and this door offers all three at once — which no single call answers.
	// So the narrowest available filter runs in the store and the rest run
	// here, over a bounded read. The alternative, capping in the store before
	// the remaining filter, is the bug decisionsWhere's own comment names:
	// it hands back some entries and calls them the newest.
	ledgerFetchCap = 500
)

// ledgerQuery is the read route's question.
type ledgerQuery struct {
	Kind    string
	Subject string
	Actor   string
	Limit   int
}

// ledgerConfinement is row confinement on the ledger: what a key may see.
//
// This is the gap auth.go's package note names. A key confined to a `user_id`
// may see only the entries whose actor is that user — which works here, and
// did not work on the pipeline, precisely because a ledger entry names its
// actor and a CreateJob names nobody.
//
// A confinement a decision cannot express — a key confined to a collection, a
// namespace or a memory scope, none of which a decision has — is a refusal
// rather than a pass. That is CortexDB's fieldsMatch rule kept: ignoring an
// uncheckable constraint is how a scope quietly becomes wider than it reads.
// Such a key is confined to the empty actor, which nothing matches.
func ledgerConfinement(key authz.Key) (actor string, confined bool) {
	if key.Scope.IsZero() {
		return "", false
	}
	if key.Scope.MemoryScope != "" || key.Scope.Namespace != "" || key.Scope.Collection != "" {
		return "", true
	}
	return key.Scope.UserID, true
}

// notFoundDecision is the one answer a caller gets for an entry that is not
// there and for one that is not theirs.
//
// It does not repeat the id back, so the two answers are identical byte for
// byte rather than merely equal in status — CortexDB's withheld() rule, taken
// one step further because an HTTP body is easier to compare than a gRPC
// status and an id space that answers differently is an oracle. NOT_FOUND
// rather than FORBIDDEN for CortexDB's reason: the frequent honest case is a
// caller asking for an entry that was never written.
const notFoundDecision = "no such decision"

func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, http.StatusMethodNotAllowed, "GET /athanor/decisions?kind=&subject=&actor=&limit= — the ledger is written by performing an act, not by posting to it")
		return
	}
	key, ok := s.authorizeLedger(w, r)
	if !ok {
		return
	}
	q := ledgerQuery{
		Kind:    ledgerTrim(r.URL.Query().Get("kind")),
		Subject: ledgerTrim(r.URL.Query().Get("subject")),
		Actor:   ledgerTrim(r.URL.Query().Get("actor")),
		Limit:   ledgerLimit(r.URL.Query().Get("limit")),
	}
	if actor, confined := ledgerConfinement(key); confined {
		if actor == "" || (q.Actor != "" && q.Actor != actor) {
			writeJSON(w, http.StatusOK, map[string]any{"decisions": []cortexdb.DecisionRecord{}, "count": 0})
			return
		}
		q.Actor = actor
	}
	recs, err := s.readLedger(r.Context(), q)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": recs, "count": len(recs)})
}

func (s *Server) handleDecisionChain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, http.StatusMethodNotAllowed, "GET /athanor/decisions/{id}")
		return
	}
	key, ok := s.authorizeLedger(w, r)
	if !ok {
		return
	}
	id := ledgerTrim(r.PathValue("id"))
	if id == "" {
		httpError(w, http.StatusBadRequest, "a decision id is required")
		return
	}
	depth := 0
	if raw := ledgerTrim(r.URL.Query().Get("depth")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			depth = n
		}
	}
	chain, err := s.db.DecisionChain(r.Context(), id, depth)
	if err != nil {
		httpError(w, http.StatusNotFound, notFoundDecision)
		return
	}
	// The root's actor decides. A chain reaches decisions other people made —
	// that is what a chain is — and refusing one because a premise two hops
	// back was signed by somebody else would make every shared decision
	// unreadable. What a confined key may not do is open somebody else's
	// account of why they did something.
	if actor, confined := ledgerConfinement(key); confined {
		if len(chain.Decisions) == 0 || actor == "" || chain.Decisions[0].Actor != actor {
			httpError(w, http.StatusNotFound, notFoundDecision)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"chain": chain})
}

// authorizeLedger is the ledger routes' door: any key from the policy, read
// clearance, exactly as the graph and the metrics take.
func (s *Server) authorizeLedger(w http.ResponseWriter, r *http.Request) (authz.Key, bool) {
	key, code, msg := s.httpKey(r.Header.Get("Authorization"))
	if code != 0 {
		httpError(w, code, msg)
		return authz.Key{}, false
	}
	if err := key.AuthorizeOperation(ledgerOperation, authz.Method{Access: authz.Read}); err != nil {
		httpError(w, http.StatusForbidden, err.Error())
		return authz.Key{}, false
	}
	return key, true
}

func ledgerLimit(raw string) int {
	n, err := strconv.Atoi(ledgerTrim(raw))
	if err != nil || n <= 0 {
		return ledgerDefaultLimit
	}
	if n > ledgerMaxLimit {
		return ledgerMaxLimit
	}
	return n
}

// readLedger answers one question with the narrowest call CortexDB has, then
// applies whatever the call could not.
func (s *Server) readLedger(ctx context.Context, q ledgerQuery) ([]cortexdb.DecisionRecord, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = ledgerDefaultLimit
	}
	fetch := limit
	if fetch < ledgerFetchCap {
		fetch = ledgerFetchCap
	}

	var (
		recs []cortexdb.DecisionRecord
		err  error
	)
	switch {
	case q.Kind != "" || q.Subject != "":
		recs, err = s.db.Precedents(ctx, cortexdb.PrecedentsQuery{Kind: q.Kind, Subject: q.Subject, Limit: fetch})
	case q.Actor != "":
		recs, err = s.db.DecisionsBy(ctx, q.Actor, fetch)
	default:
		recs, err = s.latestDecisions(ctx, limit)
	}
	if err != nil {
		return nil, err
	}

	out := make([]cortexdb.DecisionRecord, 0, len(recs))
	for _, rec := range recs {
		if q.Actor != "" && rec.Actor != q.Actor {
			continue
		}
		if q.Kind != "" && rec.Kind != q.Kind {
			continue
		}
		if q.Subject != "" && rec.Subject != q.Subject {
			continue
		}
		out = append(out, rec)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// latestDecisions is the whole ledger, newest first — the front page's
// question, and the one Precedents deliberately refuses.
//
// Precedents refuses it because on a shared brain "every decision anybody ever
// made" is other people's ledger. On the front page of the server that holds
// that brain it is the right question, and the operator's key is the one
// asking. So it is answered here rather than by widening a library API whose
// refusal is correct for its callers.
//
// The ids are read whole and sorted before the cap, and only then decoded.
// Capping before the sort would hand back the entries whose ids sort first and
// call them the newest — decisionsWhere's own warning, and it applies to a
// listing ordered by id exactly as it does to one ordered by nothing.
func (s *Server) latestDecisions(ctx context.Context, limit int) ([]cortexdb.DecisionRecord, error) {
	nodes, err := s.db.Graph().ListNodes(ctx, &graph.GraphFilter{NodeTypes: []string{cortexdb.DecisionNodeType}})
	if err != nil {
		return nil, fmt.Errorf("athanor: ledger: %w", err)
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := decisionNodeAt(nodes[i]), decisionNodeAt(nodes[j])
		if a != b {
			return a > b
		}
		return nodes[i].ID > nodes[j].ID
	})
	if len(nodes) > limit {
		nodes = nodes[:limit]
	}
	out := make([]cortexdb.DecisionRecord, 0, len(nodes))
	for _, n := range nodes {
		// One hop, so the entry comes back decoded by the code that wrote it
		// rather than by a second reading of its properties here.
		chain, err := s.db.DecisionChain(ctx, n.ID, 1)
		if err != nil || len(chain.Decisions) == 0 {
			continue
		}
		out = append(out, chain.Decisions[0])
	}
	return out, nil
}

// decisionNodeAt reads when a decision was made off its node.
//
// Through the knowledge contract's own key, which is exported, and not through
// the decision's `at` property, which is not: RecordDecision writes the same
// RFC 3339 stamp into both, and reading the one this package is entitled to
// name keeps a sort order from depending on a constant CortexDB never
// promised.
func decisionNodeAt(n *graph.GraphNode) string {
	if n == nil || n.Properties == nil {
		return ""
	}
	at, _ := n.Properties[cortexdb.KeyAt].(string)
	return at
}
