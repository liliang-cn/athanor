package rules

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
	"github.com/liliang-cn/cortexdb/v2/pkg/graph"
)

// Draft records a rule somebody wrote.
//
// The document is the one CortexDB's own rules_save takes, and it is validated
// by CortexDB and by nothing else here: a predicate that is a variable, a
// conclusion binding nothing, a premise with an empty end, text that does not
// parse — every one of those is a rule the engine already enforces, and
// enforcing it a second time in a slightly different way is how two
// implementations of one rule start disagreeing. What this adds is that the
// rule is now somewhere, under a versioned id, with a name against it.
func (s *Store) Draft(ctx context.Context, document []byte, by, note string) (Rule, error) {
	def, err := definitionOf(document)
	if err != nil {
		return Rule{}, err
	}
	lineage, _, err := split(def.ID)
	if err != nil {
		return Rule{}, err
	}
	// Rule() is the whole of validation: it parses the text or reads the
	// structured form, refuses both at once, and calls Validate.
	rule, err := def.Rule()
	if err != nil {
		return Rule{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := checkMetadata(def.Metadata); err != nil {
		return Rule{}, err
	}

	// The parent is what this edit was written after: the rule in force if
	// there is one, and otherwise the most recent draft of the lineage. It is
	// derived rather than declared because a caller who has just read
	// `current` knows the id they are replacing and a caller who has not would
	// have to guess — and a guessed parent is worse than none.
	parent, err := s.parentFor(ctx, lineage)
	if err != nil {
		return Rule{}, err
	}

	at := s.at()
	v := Rule{
		ID: def.ID, Lineage: lineage, State: Draft, Parent: parent,
		// Rendered by the engine rather than stored as typed: two people whose
		// spacing differs would otherwise store two texts for one rule, and
		// this text is what every derived edge carries and what `current`
		// hands back for pasting.
		Text: rule.Text(), Name: def.Name,
		Confidence: def.Confidence, Weight: def.Weight,
		Metadata: cloneMetadata(def.Metadata), Note: firstNamed(note, def.Note),
		CreatedBy: by, CreatedAt: at,
	}
	metadata, err := json.Marshal(v.Metadata)
	if err != nil {
		return Rule{}, fmt.Errorf("rules: draft %s: %w", v.ID, err)
	}
	const q = `INSERT INTO athanor_rules
		(id, lineage, state, parent, rule_text, name, confidence, weight, metadata, note,
		 created_by, created_at, published_by, published_at, retired_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', NULL, NULL)`
	if _, err := s.exec(ctx, q, v.ID, v.Lineage, string(Draft), v.Parent, v.Text, v.Name,
		v.Confidence, v.Weight, string(metadata), v.Note, by, at); err != nil {
		if _, getErr := s.Get(ctx, v.ID); getErr == nil {
			return Rule{}, fmt.Errorf("%w: %s", ErrExists, v.ID)
		}
		return Rule{}, fmt.Errorf("rules: draft %s: %w", v.ID, err)
	}
	act, err := s.record(ctx, nil, Act{
		Kind: ActDraft, Actor: by, Key: by, At: at, Subject: v.ID,
		Note:   draftNote(v),
		Detail: map[string]any{"text": v.Text, "parent": v.Parent},
	})
	if err != nil {
		return Rule{}, err
	}
	// The draft stands whether or not the audit view hears about it.
	_ = s.mirror(ctx, act)
	return v, nil
}

func draftNote(v Rule) string {
	line := "declared " + v.Text
	if v.Parent != "" {
		line += ", after " + v.Parent
	}
	if v.Note != "" {
		line += " — " + v.Note
	}
	return line
}

// definitionOf reads the declared document.
//
// Unknown fields are refused for loads.go's reason: a caller who wrote "txt"
// believes they declared a rule, and a rule with no text matches nothing and
// says nothing about why.
func definitionOf(document []byte) (cortexdb.RuleDefinition, error) {
	var def cortexdb.RuleDefinition
	dec := json.NewDecoder(bytes.NewReader(document))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&def); err != nil {
		return cortexdb.RuleDefinition{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	// `enabled` is CortexDB's own flag for a rule that stays in its table and
	// out of the default set. Here the three states are that flag, and a
	// second one would let a rule be retired in this table and firing in the
	// engine's — so it is refused rather than quietly ignored.
	if def.Enabled != nil {
		return cortexdb.RuleDefinition{}, fmt.Errorf("%w: `enabled` is not a field of a declared rule — publish it to put it in force and retire it to stop it", ErrInvalid)
	}
	return def, nil
}

// checkMetadata refuses a rule that grades its own output.
//
// Metadata is written onto every edge the rule derives, and the contract keys
// under `_` are how a reader decides whether to trust an edge. A rule that
// could set `_grade: verified` on what it derived would be a rule that cannot
// be distrusted, which is the one thing this whole product is for. The door
// writes those keys (see Apply); a declaration may not.
func checkMetadata(metadata map[string]string) error {
	for key := range metadata {
		if strings.HasPrefix(key, cortexdb.ContractPrefix) {
			return fmt.Errorf("%w: metadata %q is a knowledge-contract key, and those are written by the door that fired the rule, not by the rule", ErrInvalid, key)
		}
	}
	return nil
}

// parentFor is the version a new edit of a lineage follows.
func (s *Store) parentFor(ctx context.Context, lineage string) (string, error) {
	current, err := s.Current(ctx, lineage)
	switch {
	case err == nil:
		return current.ID, nil
	case !errors.Is(err, ErrNotFound):
		return "", err
	}
	const q = `SELECT id FROM athanor_rules WHERE lineage = ? ORDER BY created_at DESC, id DESC`
	var id string
	err = s.queryRow(ctx, q, lineage).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("rules: finding what %s follows: %w", lineage, err)
	}
	return id, nil
}

// Signature is who decided, under whose key, and why.
//
// A struct rather than three strings: By and Key are both names, they mean
// different things, and two adjacent string parameters that can be swapped
// without a compile error is a bug waiting for a hurried afternoon.
type Signature struct {
	// By is who decided. Empty is refused.
	By string
	// Key is the id of the key the request came in on. Empty falls back to By,
	// which is what a caller with no door in front of it has.
	Key string
	// Note is why.
	Note string
}

// Publish puts a rule in force and retires the one it replaces.
//
// One transaction, because the two halves are one fact. A retirement that
// committed without its publication would leave a lineage with nothing in
// force; a publication that committed without its retirement would leave two,
// and the partial unique index refuses that outright rather than letting
// `current` become a coin toss.
//
// It returns the published rule and the id it retired, if any.
func (s *Store) Publish(ctx context.Context, id string, req Signature) (Rule, string, error) {
	by := strings.TrimSpace(req.By)
	if by == "" {
		return Rule{}, "", fmt.Errorf("%w: publishing a rule decides what gets derived into the brain from now on", ErrUnsigned)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Rule{}, "", fmt.Errorf("rules: publish %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	v, err := s.get(ctx, tx, id)
	if err != nil {
		return Rule{}, "", err
	}
	if v.State == Published {
		return Rule{}, "", fmt.Errorf("%w: %s is already the rule in force for %s", ErrState, id, v.Lineage)
	}

	at := s.at()
	// Re-publishing a retired rule is a rollback, and it is allowed. The
	// alternative is that a rule found to be wrong can only be undone by
	// declaring a new one that undoes it, which is a worse record of what
	// happened than an act saying somebody went back.
	var acts []Act
	var retired string
	const findCurrent = `SELECT id FROM athanor_rules
		WHERE lineage = ? AND state = ? ORDER BY published_at DESC, id DESC`
	err = s.txQueryRow(ctx, tx, findCurrent, v.Lineage, string(Published)).Scan(&retired)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		retired = ""
	case err != nil:
		return Rule{}, "", fmt.Errorf("rules: publish %s: finding the rule in force: %w", id, err)
	}
	if retired != "" {
		const q = `UPDATE athanor_rules SET state = ?, retired_at = ? WHERE id = ?`
		if _, err := s.txExec(ctx, tx, q, string(Retired), at, retired); err != nil {
			return Rule{}, "", fmt.Errorf("rules: retire %s: %w", retired, err)
		}
		act, err := s.record(ctx, tx, Act{
			Kind: ActRetire, Actor: by, Key: firstNamed(req.Key, by), At: at,
			Subject: retired, Note: "replaced by " + id,
			Detail: map[string]any{"replaced_by": id},
		})
		if err != nil {
			return Rule{}, "", err
		}
		acts = append(acts, act)
	}
	const q = `UPDATE athanor_rules SET state = ?, published_by = ?, published_at = ?, retired_at = NULL WHERE id = ?`
	if _, err := s.txExec(ctx, tx, q, string(Published), by, at, id); err != nil {
		return Rule{}, "", fmt.Errorf("rules: publish %s: %w", id, err)
	}
	act, err := s.record(ctx, tx, Act{
		Kind: ActPublish, Actor: by, Key: firstNamed(req.Key, by), At: at,
		Subject: id, Note: publishNote(v, retired, req.Note),
		Detail: map[string]any{"text": v.Text, "retired": retired},
	})
	if err != nil {
		return Rule{}, "", err
	}
	acts = append(acts, act)
	if err := tx.Commit(); err != nil {
		return Rule{}, "", fmt.Errorf("rules: publish %s: %w", id, err)
	}
	// After the commit, never inside it: see Store.record.
	_ = s.mirror(ctx, acts...)

	v.State = Published
	v.PublishedBy = by
	v.PublishedAt = &at
	v.RetiredAt = nil
	return v, retired, nil
}

func publishNote(v Rule, retired, note string) string {
	line := "in force: " + v.Text
	if retired != "" {
		line += ", retiring " + retired
	}
	if note != "" {
		line += " — " + note
	}
	return line
}

// Retire stops a rule firing, without putting anything in its place.
//
// Nothing it derived is touched, and that is the point of having the verb at
// all: every edge a rule wrote carries the rule's id and its text, so a graph
// derived under `chain@1` can still say what derived it long after `chain@1`
// stopped running. Deleting the row would orphan the only readable account of
// why those edges are there.
func (s *Store) Retire(ctx context.Context, id string, req Signature) (Rule, error) {
	by := strings.TrimSpace(req.By)
	if by == "" {
		return Rule{}, fmt.Errorf("%w: retiring a rule decides that what it derived will not be derived again", ErrUnsigned)
	}
	v, err := s.Get(ctx, id)
	if err != nil {
		return Rule{}, err
	}
	if v.State == Retired {
		return Rule{}, fmt.Errorf("%w: %s is already retired", ErrState, id)
	}
	at := s.at()
	const q = `UPDATE athanor_rules SET state = ?, retired_at = ? WHERE id = ?`
	if _, err := s.exec(ctx, q, string(Retired), at, id); err != nil {
		return Rule{}, fmt.Errorf("rules: retire %s: %w", id, err)
	}
	act, err := s.record(ctx, nil, Act{
		Kind: ActRetire, Actor: by, Key: firstNamed(req.Key, by), At: at,
		Subject: id, Note: retireNote(v, req.Note),
		Detail: map[string]any{"was": string(v.State)},
	})
	if err != nil {
		return Rule{}, err
	}
	_ = s.mirror(ctx, act)
	v.State = Retired
	v.RetiredAt = &at
	return v, nil
}

func retireNote(v Rule, note string) string {
	line := "retired " + v.Text + "; what it derived stays"
	if note != "" {
		line += " — " + note
	}
	return line
}

// Apply fires a rule and records that somebody did.
//
// The derivation is CortexDB's, unchanged: the same ApplyRules the tool
// surface calls, over the same graph, writing the same provenance onto every
// edge — the rule's id, the rule's text, and the premise edges under the
// conclusion, which is what inference_explain reads back. Three things are
// added here, and all three are the product's rather than the engine's.
//
// The rule is handed over as an ad-hoc definition rather than fired out of
// CortexDB's own table, for the reason the package comment gives: a rule
// sitting enabled in kg_rules is a rule that can fire with no author.
//
// Every edge is stamped with the knowledge contract before it is written —
// `_grade: self_consistent`, because a derived edge is coherent with what was
// already stated and has been checked against nothing in the world;
// `_producer: compiled`, because it came deterministically from a declared
// model; `_source`, `_by` and `_at`, so the shelf can say which rule, which
// key and when. Without them a rule's output is the one thing on the shelf
// that contract_tally cannot count, and an uncountable record is one nobody
// distrusts.
//
// And the act is signed. An application with no `by` is refused exactly as an
// ontology approval is; the actor recorded is the key, and the name typed into
// the request is kept verbatim beside it.
func (s *Store) Apply(ctx context.Context, id string, req Application) (Firing, error) {
	by := strings.TrimSpace(req.By)
	if by == "" {
		return Firing{}, fmt.Errorf("%w: a rule that fires asserts new edges into the brain, and an assertion nobody is named for cannot be argued with later", ErrUnsigned)
	}
	v, err := s.Get(ctx, id)
	if err != nil {
		return Firing{}, err
	}
	switch {
	case v.State == Retired:
		return Firing{}, fmt.Errorf("%w: %s is retired; what it derived stays readable, but it does not fire again", ErrState, id)
	case v.State == Draft && !req.DryRun:
		return Firing{}, fmt.Errorf("%w: %s is a draft — fire it as a dry run to see what it would derive, or publish it", ErrState, id)
	}

	at := s.at()
	actor := firstNamed(req.Key, by)
	// The act's id is minted before the act, because every edge this firing
	// derives carries it: an edge can then name the firing that made it, and
	// the firing names the key that signed it. The alternative — stamping the
	// rule id alone — answers "which rule" and not "which run of it", and the
	// two are different questions once a rule has been fired twice.
	actID, err := mintID()
	if err != nil {
		return Firing{}, err
	}

	def := v.Definition()
	def.Metadata = contractMetadata(v, actor, actID, at)
	resp, err := s.engine.ApplyRules(ctx, cortexdb.RulesApplyRequest{
		Rules:          []cortexdb.RuleDefinition{def},
		DocumentID:     req.Document,
		DryRun:         req.DryRun,
		DeleteExisting: req.DeleteExisting,
		MaxIterations:  req.MaxIterations,
		MaxDerived:     req.MaxDerived,
	})
	if err != nil {
		if errors.Is(err, graph.ErrRuleCapExceeded) {
			return Firing{}, fmt.Errorf("%w: %w", ErrCapped, err)
		}
		return Firing{}, fmt.Errorf("rules: apply %s: %w", id, err)
	}
	if resp == nil {
		return Firing{}, fmt.Errorf("rules: apply %s: the engine returned nothing", id)
	}

	f := Firing{
		Rule: v.ID, Lineage: v.Lineage, By: by, Actor: actor, At: at, Note: req.Note,
		DryRun: req.DryRun, Document: req.Document,
		Iterations: resp.Iterations, Candidates: resp.CandidateEdges,
		Created: resp.CreatedEdgeIDs, Unchanged: resp.UnchangedEdgeIDs,
		Deleted: resp.DeletedEdgeIDs, Unresolved: resp.UnresolvedTerms,
	}
	for _, e := range resp.Edges {
		f.Edges = append(f.Edges, Derived{
			EdgeID: e.EdgeID, From: e.FromNodeID, To: e.ToNodeID, Type: e.EdgeType,
			Confidence: e.Confidence, Supports: e.SupportEdgeIDs,
		})
	}

	detail, err := detailOf(f)
	if err != nil {
		return Firing{}, err
	}
	act, err := s.record(ctx, nil, Act{
		ID: actID, Kind: ActApply, Actor: by, Key: actor, At: at, Subject: v.ID,
		Note: applyNote(v, f, req.Note), Detail: detail,
	})
	if err != nil {
		// The edges are already in the graph, and this is the store's own
		// record of them failing to write — not the ledger's. It is returned
		// rather than reported, because a firing whose own table does not know
		// it happened is not a firing anybody can audit, and the caller has to
		// be told the difference.
		return Firing{}, err
	}
	f.Act = act.ID
	if err := s.mirror(ctx, act); err != nil {
		f.LedgerError = err.Error()
	}
	return f, nil
}

// contractMetadata is the knowledge contract, written onto every edge the
// firing derives.
//
// There is deliberately no `_confidence`: the engine already writes a
// `confidence` property that is the premises' confidence times the rule's, and
// a second number under a contract key that could disagree with it is worse
// than none. `_state` carries the producer's own word, which here is the fact
// that the edge was derived rather than read out of a source.
func contractMetadata(v Rule, actor, act string, at time.Time) map[string]string {
	meta := cloneMetadata(v.Metadata)
	if meta == nil {
		meta = map[string]string{}
	}
	meta[cortexdb.KeySource] = "athanor:rule:" + v.ID
	meta[cortexdb.KeyProducer] = cortexdb.ProducerCompiled
	meta[cortexdb.KeyGrade] = cortexdb.GradeSelfConsistent
	meta[cortexdb.KeyState] = "derived"
	meta[cortexdb.KeyBy] = actor
	meta[cortexdb.KeyAt] = at.Format(time.RFC3339)
	meta["firing"] = act
	return meta
}

// detailOf renders a firing as the act's detail. One encoding, both ways:
// Firings reads it back through the same shape (firingFrom).
func detailOf(f Firing) (map[string]any, error) {
	body, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("rules: recording the firing of %s: %w", f.Rule, err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("rules: recording the firing of %s: %w", f.Rule, err)
	}
	return out, nil
}

func applyNote(v Rule, f Firing, note string) string {
	verb := "derived"
	if f.DryRun {
		verb = "would derive"
	}
	line := fmt.Sprintf("fired %s (%s): %s %d edges, %d unchanged, over %d facts in %d rounds",
		v.ID, v.Text, verb, len(f.Created), len(f.Unchanged), f.Candidates, f.Iterations)
	if f.Document != "" {
		line += ", scoped to " + f.Document
	}
	if note != "" {
		line += " — " + note
	}
	return line
}
