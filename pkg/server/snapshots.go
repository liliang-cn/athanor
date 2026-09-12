package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/liliang-cn/athanor/pkg/ontologies"
	"github.com/liliang-cn/athanor/pkg/snapshots"
	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
)

// Time travel, as a door.
//
// pkg/snapshots is the workflow — name the present moment, retire a name,
// compare two — and this file is its door, the same one /athanor/loads and
// /athanor/ontologies stand in: the key policy decides who may act, the act is
// recorded in the decision ledger under the key that performed it, a refusal
// from the store is a status rather than a stack trace, and a ledger write
// that fails never undoes the act it describes.
//
// # What the door adds that the store cannot
//
// A snapshot names what it rested on, and neither half of that is in the
// store's own table. Which vocabulary was current lives in pkg/ontologies;
// which loads had landed lives in the decision ledger. Both are read here, at
// the moment of taking, and handed to the store as the footing — so the store
// has one reader of each rather than a second one disagreeing with the first.
//
// # Row confinement
//
// A snapshot names the key that took it, so the ledger's rule applies without
// a hole in it: a key confined to a user_id lists only the moments it signed,
// and a moment somebody else signed answers exactly as one that was never
// taken — same status, same bytes. That is CortexDB's withheld() rule, and the
// reason it works here and not on the pipeline is the one auth.go gives.

// snapshotStores holds one store per brain — ontologies.go's arrangement, for
// its reasons: the schema DDL runs once per *cortexdb.DB rather than once per
// request.
var snapshotStores sync.Map // *cortexdb.DB -> *snapshotHandle

type snapshotHandle struct {
	once  sync.Once
	store *snapshots.Store
	err   error
}

func (s *Server) snapshots() (*snapshots.Store, error) {
	entry, _ := snapshotStores.LoadOrStore(s.db, &snapshotHandle{})
	h := entry.(*snapshotHandle)
	h.once.Do(func() { h.store, h.err = snapshots.New(s.db) })
	return h.store, h.err
}

// snapshotOperation is the name the key policy authorizes against, the same
// shape "athanor.loads" and "athanor.ontologies" use.
const snapshotOperation = "athanor.snapshots"

// notFoundSnapshot is the one answer a caller gets for a moment that was never
// taken and for one that is not theirs.
//
// It does not repeat the name back, so the two answers are identical byte for
// byte rather than merely equal in status — the ledger's rule, and it matters
// more here because the store's own refusal does name the snapshot, and
// passing that through would have made the confined case distinguishable from
// the missing one by the length of the body.
const notFoundSnapshot = "no such snapshot"

// authorizeSnapshots answers the request itself when the key is missing or the
// clearance is short, and reports whether the handler may continue.
//
// Reading a moment is a read; taking one and dropping one are writes. A
// snapshot is not a change to the graph, and it would have been easy to argue
// that taking one is therefore a read — it is not: it puts a row in a table
// and an entry in the ledger, both signed, and a read-only key that could sign
// things is not read-only.
func (s *Server) authorizeSnapshots(w http.ResponseWriter, r *http.Request, access authz.Access) (authz.Key, bool) {
	key, code, msg := s.requestKey(r)
	if code != 0 {
		httpError(w, code, msg)
		return authz.Key{}, false
	}
	if err := key.AuthorizeOperation(snapshotOperation, authz.Method{Access: access}); err != nil {
		httpError(w, http.StatusForbidden, err.Error())
		return authz.Key{}, false
	}
	return key, true
}

func (s *Server) snapshotsReady(w http.ResponseWriter) (*snapshots.Store, bool) {
	store, err := s.snapshots()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	return store, true
}

// takeRequest is the body of POST /athanor/snapshots.
type takeRequest struct {
	// Name is what the moment is called.
	Name string `json:"name"`
	// By is the person taking it. It is in the body and not taken from the key
	// for the reason an ontology approval's is: "whoever held the operator
	// key" is not an answer to who says this is what we believed in March.
	By   string `json:"by"`
	Note string `json:"note,omitempty"`
	// Replace moves a name that is already a moment to this one.
	Replace bool `json:"replace,omitempty"`
}

// dropRequest is the body of POST /athanor/snapshots/{name}:drop.
type dropRequest struct {
	By   string `json:"by"`
	Note string `json:"note,omitempty"`
}

// handleSnapshots is the collection: GET to list, POST to take one.
func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		key, ok := s.authorizeSnapshots(w, r, authz.Read)
		if !ok {
			return
		}
		store, ok := s.snapshotsReady(w)
		if !ok {
			return
		}
		q := snapshots.ListQuery{
			State: snapshots.State(ledgerTrim(r.URL.Query().Get("state"))),
			Limit: snapshotLimit(r.URL.Query().Get("limit")),
		}
		actor, confined := ledgerConfinement(key)
		if confined {
			if actor == "" {
				// A confinement a snapshot cannot express — a key scoped to a
				// collection or a namespace, neither of which a moment has —
				// is the empty actor, which nothing matches. Ignoring an
				// uncheckable constraint is how a scope quietly becomes wider
				// than it reads.
				writeJSON(w, http.StatusOK, map[string]any{"snapshots": []snapshots.Snapshot{}, "count": 0})
				return
			}
			q.Actor = actor
		}
		list, err := store.List(r.Context(), q)
		if err != nil {
			snapshotError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"snapshots": list, "count": len(list)})

	case http.MethodPost:
		key, ok := s.authorizeSnapshots(w, r, authz.Write)
		if !ok {
			return
		}
		store, ok := s.snapshotsReady(w)
		if !ok {
			return
		}
		var req takeRequest
		if !decodeBody(w, r, &req) {
			return
		}
		snap, err := store.Take(r.Context(), snapshots.Taking{
			Name: req.Name, By: req.By, Actor: key.ID, Note: req.Note,
			Replace: req.Replace,
			Footing: s.footing(r.Context()),
		})
		if err != nil {
			snapshotError(w, err)
			return
		}
		// The moment is named. Everything from here is the record of that, and
		// the record is not allowed to undo it — loads.go's rule, for its
		// reason: a store that loses a moment to protect a note about the
		// moment is a worse store than one whose audit view is behind.
		answer := map[string]any{"snapshot": snap}
		if decision, err := s.recordSnapshot(r.Context(), snap, snapshotTake); err != nil {
			log.Printf("athanor: ledger: snapshot %s: %v", snap.Name, err)
			answer["ledger_error"] = err.Error()
		} else {
			answer["decision"] = decision
		}
		writeJSON(w, http.StatusCreated, answer)

	default:
		httpError(w, http.StatusMethodNotAllowed, "POST a {name, by} document to name this moment, or GET the list")
	}
}

// handleSnapshot is one moment: GET it, or POST `{name}:drop` to retire it.
//
// A verb after the name rather than a subresource, which is the shape
// /athanor/ontologies/{id}:publish already uses and for its reason: a drop is
// not a thing, and `/athanor/snapshots/nightly/drop` would invite a GET and a
// question about what a drop's own identity is.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	segment := r.PathValue("name")
	name, verb := segment, ""
	if at := strings.LastIndex(segment, ":"); at >= 0 && segment[at+1:] == verbDrop {
		name, verb = segment[:at], verbDrop
	}
	name = strings.TrimSpace(name)
	if name == "" {
		httpError(w, http.StatusBadRequest, "a snapshot name is required")
		return
	}

	if verb == "" {
		key, ok := s.authorizeSnapshots(w, r, authz.Read)
		if !ok {
			return
		}
		if r.Method != http.MethodGet {
			httpError(w, http.StatusMethodNotAllowed, "GET a moment, or POST {name}:drop to retire it")
			return
		}
		store, ok := s.snapshotsReady(w)
		if !ok {
			return
		}
		snap, err := store.Get(r.Context(), name)
		if err != nil {
			snapshotError(w, err)
			return
		}
		if !maySee(key, snap) {
			httpError(w, http.StatusNotFound, notFoundSnapshot)
			return
		}
		writeJSON(w, http.StatusOK, snap)
		return
	}

	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "POST to drop a moment")
		return
	}
	key, ok := s.authorizeSnapshots(w, r, authz.Write)
	if !ok {
		return
	}
	store, ok := s.snapshotsReady(w)
	if !ok {
		return
	}
	var req dropRequest
	if !decodeBody(w, r, &req) {
		return
	}
	// Read before acting, so that a confined key dropping somebody else's
	// moment is answered as if the moment did not exist rather than being told
	// it may not. The store would have refused nothing: it does not know about
	// keys, and that is the right division.
	existing, err := store.Get(r.Context(), name)
	if err != nil {
		snapshotError(w, err)
		return
	}
	if !maySee(key, existing) {
		httpError(w, http.StatusNotFound, notFoundSnapshot)
		return
	}
	snap, err := store.Drop(r.Context(), name, snapshots.Dropping{By: req.By, Actor: key.ID, Note: req.Note})
	if err != nil {
		snapshotError(w, err)
		return
	}
	answer := map[string]any{"snapshot": snap}
	if decision, err := s.recordSnapshot(r.Context(), snap, snapshotDrop); err != nil {
		log.Printf("athanor: ledger: snapshot %s dropped: %v", snap.Name, err)
		answer["ledger_error"] = err.Error()
	} else {
		answer["decision"] = decision
	}
	writeJSON(w, http.StatusOK, answer)
}

// verbDrop is the one act written after a name.
const verbDrop = "drop"

// handleSnapshotDiff compares two named moments.
//
// A read, and a read of the whole graph, so it takes any key from the policy
// with read clearance — and both ends have to be moments this key may see, for
// the reason the ledger's chain route checks its root: a diff names facts, and
// a diff whose ends somebody else signed is their account of what changed.
func (s *Server) handleSnapshotDiff(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, http.StatusMethodNotAllowed, "GET /athanor/snapshots/diff?from=&to=")
		return
	}
	key, ok := s.authorizeSnapshots(w, r, authz.Read)
	if !ok {
		return
	}
	store, ok := s.snapshotsReady(w)
	if !ok {
		return
	}
	query := r.URL.Query()
	from, to := ledgerTrim(query.Get("from")), ledgerTrim(query.Get("to"))
	if from == "" || to == "" {
		httpError(w, http.StatusBadRequest, "from and to are both required: they are the names of two moments")
		return
	}
	for _, name := range []string{from, to} {
		snap, err := store.Get(r.Context(), name)
		if err != nil {
			snapshotError(w, err)
			return
		}
		if !maySee(key, snap) {
			httpError(w, http.StatusNotFound, notFoundSnapshot)
			return
		}
	}
	diff, err := store.Diff(r.Context(), from, to, snapshots.DiffOptions{
		Limit:     snapshotLimit(query.Get("limit")),
		Cursor:    ledgerTrim(query.Get("cursor")),
		NodeTypes: csv(query.Get("node_types")),
		EdgeTypes: csv(query.Get("edge_types")),
	})
	if err != nil {
		snapshotError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, diff)
}

// maySee is row confinement on one moment: a confined key sees what it signed.
func maySee(key authz.Key, snap snapshots.Snapshot) bool {
	actor, confined := ledgerConfinement(key)
	if !confined {
		return true
	}
	return actor != "" && snap.Actor == actor
}

// csv splits a comma-separated query parameter, dropping the empties a
// trailing comma leaves behind.
func csv(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

const (
	snapshotDefaultLimit = 50
	snapshotMaxLimit     = 500
)

func snapshotLimit(raw string) int {
	n, err := strconv.Atoi(ledgerTrim(raw))
	if err != nil || n <= 0 {
		return snapshotDefaultLimit
	}
	if n > snapshotMaxLimit {
		return snapshotMaxLimit
	}
	return n
}

// footingLoadCap is how many loads a snapshot names.
//
// A brain that has been fed for a year has thousands of them and a snapshot is
// not a listing; what the footing is for is answering "what was in here, and
// where did it come from" for the recent past, which is the question an
// operator asks after an incident. Past the cap the footing says so rather
// than quietly naming the newest hundred as if they were all of them.
const footingLoadCap = 100

// footing is what the door knows the moment rested on: the vocabulary that was
// current and the loads that had landed.
//
// Every failure here is swallowed into an absent field rather than a refused
// snapshot. The moment is the thing being captured, and the counts are read
// from the graph itself; losing the footing because a side table would not
// answer would be refusing to record what happened because of a note about
// what happened — the same judgement the load path makes about its own ledger
// write.
func (s *Server) footing(ctx context.Context) snapshots.Footing {
	out := snapshots.Footing{}
	if store, err := s.ontologies(); err == nil {
		if versions, err := store.List(ctx, ""); err == nil {
			for _, v := range versions {
				if v.State != ontologies.Published {
					continue
				}
				if out.Ontologies == nil {
					out.Ontologies = map[string]string{}
				}
				out.Ontologies[v.Lineage] = v.ID
			}
		}
	}
	// One more than the cap, so that "there are more" is known rather than
	// guessed from a full page.
	loads, err := s.readLedger(ctx, ledgerQuery{Kind: ledgerKindLoad, Limit: footingLoadCap + 1})
	if err != nil {
		return out
	}
	if len(loads) > footingLoadCap {
		loads, out.MoreLoads = loads[:footingLoadCap], true
	}
	for _, rec := range loads {
		out.Loads = append(out.Loads, rec.ID)
	}
	out.LoadCount = len(out.Loads)
	return out
}

// snapshotError turns the store's refusals into the statuses they deserve.
// They are values rather than strings so this reads them by identity.
func snapshotError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, snapshots.ErrNotFound):
		// Without the name, so that a moment somebody else signed and a moment
		// nobody ever took are one answer. See notFoundSnapshot.
		httpError(w, http.StatusNotFound, notFoundSnapshot)
	case errors.Is(err, snapshots.ErrExists):
		httpError(w, http.StatusConflict, err.Error())
	case errors.Is(err, snapshots.ErrUnsigned), errors.Is(err, snapshots.ErrInvalid):
		httpError(w, http.StatusBadRequest, err.Error())
	default:
		httpError(w, http.StatusInternalServerError, err.Error())
	}
}

// recordSnapshot mirrors one act into the decision ledger and reports the
// entry's id.
//
// # No premises
//
// A snapshot rests on the loads that landed before it, and naming them as
// premises would be the obvious thing: they are decisions, they exist, and
// RecordDecision would write an edge to each. It does not, and the reason is
// what a snapshot is. A hundred `based_on` edges per snapshot are a hundred
// facts added to the graph by the act of measuring it — so the next snapshot
// counts them, every diff across the pair reports them as added, and a nightly
// snapshot would grow the brain faster than the corpus does. The load ids are
// in the entry's structured detail and in the row, both of which are exact and
// neither of which claims to be a graph edge — the argument ledger.go already
// makes about `about`.
//
// The entry itself is one node and it is graded verified, because a named
// actor signed it, so the moment after a snapshot has one more verified record
// than the moment it captured. That is not hidden and is not corrected for: a
// tally of this brain has always counted the ledger — the front page's does —
// and a snapshot whose ladder quietly excluded it would disagree with
// contract_tally about the same shelf.
func (s *Server) recordSnapshot(ctx context.Context, snap snapshots.Snapshot, verb string) (string, error) {
	// The instant is stamped to the nanosecond it was truncated to rather than
	// to the second: it is what a caller would hand to graph_diff to read this
	// moment back, and a stamp rounded to the second names a different one.
	detail := map[string]any{
		"snapshot": snap.Name,
		"at":       snap.At.Format(time.RFC3339Nano),
		"nodes":    snap.Counts.Nodes,
		"edges":    snap.Counts.Edges,
		"verified": snap.Grades.Verified.Nodes + snap.Grades.Verified.Edges,
		"asserted": snap.Grades.Asserted.Nodes + snap.Grades.Asserted.Edges,
	}
	if len(snap.Footing.Ontologies) > 0 {
		detail["ontologies"] = snap.Footing.Ontologies
	}
	if len(snap.Footing.Loads) > 0 {
		detail["loads"] = snap.Footing.Loads
	}
	if snap.Footing.MoreLoads {
		detail["more_loads"] = true
	}

	actor, by, note := snap.Actor, snap.By, snap.Note
	id, line := snapshotDecisionID(snap.Name), fmt.Sprintf(
		"took the snapshot %s of the brain at %s: %d nodes, %d edges",
		snap.Name, snap.At.Format(time.RFC3339Nano), snap.Counts.Nodes, snap.Counts.Edges)
	if snap.Replaced != nil {
		detail["replaced"] = snap.Replaced.Format(time.RFC3339Nano)
	}
	if verb == snapshotDrop {
		actor, by, note = snap.DroppedActor, snap.DroppedBy, snap.DropNote
		id = snapshotDropDecisionID(snap.Name)
		line = fmt.Sprintf("dropped the snapshot %s, which named %s",
			snap.Name, snap.At.Format(time.RFC3339Nano))
	}
	// The name typed into the body, kept beside the key that actually called.
	// See ledger.go on `_by` and `by`.
	if by != "" && by != actor {
		detail["by"] = by
	}
	if note != "" {
		detail["note"] = note
	}

	rec, err := s.ledger.record(ctx, ledgerEntry{
		ID:    id,
		Kind:  ledgerKindSnapshot + verb,
		Actor: actor,
		// The verdict is the act in its own word, which for a take is what the
		// moment is now called: a reader scanning the ledger for "nightly"
		// finds it without opening the detail.
		Verdict: verb,
		Note:    line,
		Detail:  detail,
	})
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}
