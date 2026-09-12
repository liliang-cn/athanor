// Package rules is the workflow around CortexDB's rule engine: which rules
// exist, which one of them is in force, and who fired it.
//
// CortexDB has the hard half. A rule is a Horn clause over graph edges —
// `IF manages(?a, ?b) AND manages(?b, ?c) THEN manages_chain(?a, ?c)` —
// ParseRuleText reads it, Rule.Validate refuses the unsafe ones, ApplyRules
// forward-chains a set of them to a fixpoint, and every edge they derive
// carries the rule's id, the rule's text and the premise edges under it, which
// is what inference_explain reads back. None of that is held as a workflow,
// because CortexDB holds nothing as a workflow: `rules_save` is an upsert into
// a configuration table and `rules_apply` fires whatever is enabled, for
// whoever asked, with no record that anybody asked.
//
// Which is exactly the gap this package fills, and it is the same gap
// pkg/ontologies fills over alchemy's Extend. A rule that adds edges to the
// brain is a claim about what is true; a claim nobody is named for cannot be
// argued with later. So: a rule is declared under a versioned id, one version
// of a lineage is published at a time, retiring is not deleting, and applying
// one is an act with an author that lands in the decision ledger beside the
// loads and the vocabulary decisions.
//
// # Why the rules are not written into CortexDB's own rule table
//
// `rules_save` would put them there, `rules_list` would show them, and
// `rules_apply` with no arguments would fire every enabled one. That last
// sentence is the reason this package does not use it. A rule sitting enabled
// in kg_rules can be fired through the brain's own door by any key with write
// clearance, with no `by`, no ledger entry and no firing recorded — which is
// precisely the thing Athanor exists to make impossible. So the declared rule
// lives in this package's table, and an application hands the engine the rule
// as an ad-hoc definition: same engine, same derivation, same provenance on
// the edges, and no second path that fires it without an author.
package rules

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// State is where a version of a rule stands.
//
// Three, where the vocabulary has five, because the two extra states there are
// alchemy's propose → approve round and a rule has no equivalent: nobody
// proposes a rule, a person writes one. A `draft` is declared and validated and
// fires only as a dry run. A `published` rule is the one in force for its
// lineage. A `retired` one fired in the past and will not fire again, and it
// stays exactly as it was — every edge it derived names its id and its text in
// the edge's own provenance, so deleting the row would orphan the only readable
// account of why those edges are in the graph.
type State string

const (
	Draft     State = "draft"
	Published State = "published"
	Retired   State = "retired"
)

// The acts this package records. Namespaced because the ledger holds the load,
// the review, the vocabulary and the live-database acts beside them.
const (
	ActDraft   = "rule.draft"
	ActPublish = "rule.publish"
	ActRetire  = "rule.retire"
	ActApply   = "rule.apply"
)

// Act is one signed decision, shaped for the decision ledger it becomes an
// entry in: kind, actor, at, subject, note, detail.
type Act struct {
	ID      string         `json:"id"`
	Kind    string         `json:"kind"`
	Actor   string         `json:"actor"`
	At      time.Time      `json:"at"`
	Subject string         `json:"subject"`
	Note    string         `json:"note,omitempty"`
	Detail  map[string]any `json:"detail,omitempty"`

	// Key is the door's own record of who called: the id of the key the
	// request authenticated as. Actor is what the act was signed with, which
	// for an application is a name typed into a request body and checked by
	// nobody; the ledger keeps both rather than choosing.
	//
	// It is not a column, for pkg/ontologies.Act.Key's reason: what Key is for
	// is the ledger mirror, which happens in the call that supplied it. An act
	// read back out of the table carries no Key, which is honest — the table
	// never knew it.
	Key string `json:"-"`
}

// Rule is one version of one declared rule.
type Rule struct {
	// ID is "chain@1": a lineage, an "@", and a version. It is the string
	// written into every edge this rule derives, so an edge in the graph and a
	// row in this table are joinable by eye.
	ID string `json:"id"`
	// Lineage is the half of the id before the "@". Publication is per
	// lineage: "chain@3" replaces "chain@2" and says nothing about "owns@1".
	Lineage string `json:"lineage"`
	State   State  `json:"state"`
	// Parent is the version of the same lineage this one was written after.
	// Empty for the first.
	Parent string `json:"parent,omitempty"`

	// Text is the rule in the written form, and it is the point of the whole
	// table: it is what a person argues with two years from now, and it is
	// what every edge derived under this rule carries in its `rule_text`.
	Text string `json:"text"`
	Name string `json:"name,omitempty"`
	// Confidence multiplies into every derived edge's confidence. Zero is 1.0,
	// CortexDB's own reading.
	Confidence float64 `json:"confidence,omitempty"`
	// Weight overrides the derived edge weight. Zero is the mean of the
	// premises'.
	Weight float64 `json:"weight,omitempty"`
	// Metadata is written onto every edge this rule derives, minus the keys
	// the engine owns and the contract keys the door owns (see Definition).
	Metadata map[string]string `json:"metadata,omitempty"`

	Note        string     `json:"note,omitempty"`
	CreatedBy   string     `json:"created_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	PublishedBy string     `json:"published_by,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	RetiredAt   *time.Time `json:"retired_at,omitempty"`
}

// Definition renders the rule back into the document a caller declared — and
// the one CortexDB's own `rules_save` takes, so what `current` hands back is
// what a client pastes anywhere a rule is accepted.
//
// Enabled is deliberately not set. This package's three states are the enabled
// flag; a second one that could disagree with them is a rule that is retired
// here and firing there.
func (r Rule) Definition() cortexdb.RuleDefinition {
	return cortexdb.RuleDefinition{
		ID: r.ID, Name: r.Name, Text: r.Text,
		Confidence: r.Confidence, Weight: r.Weight,
		Note: r.Note, Metadata: cloneMetadata(r.Metadata),
	}
}

// Application is one request to fire a rule.
type Application struct {
	// By is the person. Empty is refused: a rule that fires adds edges to the
	// brain, and an assertion nobody is named for cannot be argued with later.
	By string
	// Key is the id of the key the request came in on. It is the actor the
	// ledger records — the identity an operator can revoke — and when it
	// disagrees with By the record keeps both.
	Key string
	// Note is why this was run now.
	Note string
	// Document scopes which edges may take part and is stamped onto what is
	// derived. Empty is the whole graph.
	Document string
	// DryRun computes the derivation and writes nothing. It is the only way a
	// draft may be fired: reading what a rule would do is how a person decides
	// whether to publish it.
	DryRun bool
	// DeleteExisting removes the edges this rule derived before rerunning, for
	// a rule whose premises have since been retracted.
	DeleteExisting bool
	// MaxIterations and MaxDerived cap the chaining. Zero takes CortexDB's
	// defaults. A run that hits a cap writes nothing and says so — a half
	// computed closure in the graph is worse than none, because nothing
	// downstream can tell which half it got.
	MaxIterations int
	MaxDerived    int
}

// Derived is one edge a firing produced, with the premises under it.
type Derived struct {
	EdgeID     string  `json:"edge_id"`
	From       string  `json:"from"`
	To         string  `json:"to"`
	Type       string  `json:"type"`
	Confidence float64 `json:"confidence,omitempty"`
	// Supports are the edge ids this conclusion rests on. They are what
	// inference_explain walks, and carrying them out of here is what lets an
	// inference name its premises without a second query.
	Supports []string `json:"supports,omitempty"`
}

// Firing is what one application did.
type Firing struct {
	// Act is the id of the act this firing recorded. It is omitted when empty
	// because the act's own detail is this value marshalled, and an entry
	// naming itself would be a field that is always blank in the record and
	// filled in only on the way out (Firings).
	Act string `json:"act,omitempty"`
	// Rule is the version that fired, Lineage its family.
	Rule    string `json:"rule"`
	Lineage string `json:"lineage"`
	// By is the name it was signed with and Actor the key that presented
	// itself. Both, because they can disagree and the disagreement is the
	// record's most useful part.
	By     string    `json:"by"`
	Actor  string    `json:"actor"`
	At     time.Time `json:"at"`
	Note   string    `json:"note,omitempty"`
	DryRun bool      `json:"dry_run,omitempty"`
	// Document is the scope this ran under, empty for the whole graph.
	Document string `json:"document,omitempty"`

	Iterations int `json:"iterations"`
	// Candidates is how many stored edges took part as facts.
	Candidates int `json:"candidates"`

	Created   []string  `json:"created,omitempty"`
	Unchanged []string  `json:"unchanged,omitempty"`
	Deleted   []string  `json:"deleted,omitempty"`
	Edges     []Derived `json:"edges,omitempty"`
	// Unresolved are literal terms in the rule that match no stored node, so
	// the rule could never fire. Carried because a rule that silently matches
	// nothing looks exactly like a rule that is simply not true of this graph.
	Unresolved []string `json:"unresolved,omitempty"`

	// LedgerError is what the audit mirror said, when it failed.
	//
	// A firing is not undone by the record of it failing to write: the edges
	// are in the brain, and rolling them back to protect a note about them
	// would be a store that loses data to keep its diary tidy. So the failure
	// is reported here, beside the result, and the caller says so out loud.
	LedgerError string `json:"ledger_error,omitempty"`
}

// The refusals, as values, so an HTTP surface turns each into the status it
// deserves rather than reading error strings.
var (
	// ErrNotFound is no such rule, or no published version in a lineage.
	ErrNotFound = errors.New("rules: no such rule")
	// ErrExists is an id already in the table. Ids are not reused: a second
	// rule under one id would leave two texts with one name, and every edge
	// derived under either would name an id that no longer says which.
	ErrExists = errors.New("rules: that rule id is already declared")
	// ErrState is an act the rule's state does not admit.
	ErrState = errors.New("rules: the rule is not in a state for that")
	// ErrUnsigned is a firing nobody is named for.
	ErrUnsigned = errors.New("rules: the decision needs a name")
	// ErrInvalid is a malformed declaration — a rule that does not parse, an
	// id with no version, a conclusion no premise binds.
	ErrInvalid = errors.New("rules: refused")
	// ErrCapped is a derivation that stopped at a cap rather than at a
	// fixpoint. Nothing was written.
	ErrCapped = errors.New("rules: the derivation hit a cap and nothing was written")
)

// split is the lineage and the version half of an id.
//
// A rule id must carry both. CortexDB accepts any string as a rule id and is
// right to — its table is configuration — but a rule this package holds is
// versioned by construction: "the edit that was in force in March" has to be a
// different row from the one in force now, and an id with no version half has
// nowhere to put the next edit.
func split(id string) (lineage, version string, err error) {
	name, rest, ok := strings.Cut(strings.TrimSpace(id), "@")
	name, rest = strings.TrimSpace(name), strings.TrimSpace(rest)
	if !ok || name == "" || rest == "" {
		return "", "", fmt.Errorf("%w: %q is not a versioned id — a rule is declared as lineage@version, e.g. chain@1, so that the next edit has somewhere to go", ErrInvalid, id)
	}
	return name, rest, nil
}

// mintID names an act. Not a sequence: a BIGSERIAL and an INTEGER PRIMARY KEY
// AUTOINCREMENT are the one thing the two dialects cannot be handed the same
// DDL for, and an act does not need an ordered id — `at` orders them.
func mintID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rules: mint act id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func cloneMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// firstNamed is the first of these that is not blank.
func firstNamed(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
