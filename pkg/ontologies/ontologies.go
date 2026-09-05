// Package ontologies is the workflow around alchemy's vocabulary: where
// versions live, who approved what, and which one is current.
//
// alchemy already has the hard half. `ontology.Load` validates a document,
// a run returns `Result.Proposals` — the types the extractor wanted and the
// vocabulary lacked, one entry per type with the ends they were observed
// between — and `Ontology.Extend(part, accept, by, newID)` declares the
// accepted ones and hands back a new document under a new id, because a
// vocabulary that gained a type is a different vocabulary and every record's
// provenance names it. None of that holds anything between calls: alchemy is
// stateless by design (§4), the caller keeps the document and supplies it per
// job.
//
// Which is exactly the gap. "The caller keeps the document" means, in
// practice, a JSON string pasted into every CreateJob, no record of which
// edit is the one in force, and no answer to who accepted the type that let a
// fact in. This package is the missing half and nothing more: five states, a
// parent link, a signed act per decision, and one published version per
// lineage at a time.
//
// It is deliberately not a second implementation of anything. Validation is
// ontology.Load, extension is Ontology.Extend, the proposals are alchemy's
// own values carried through unchanged. What is added is durable: two tables
// on the brain's handle (schema.go).
package ontologies

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/liliang-cn/alchemy/pkg/alchemy"
)

// State is where a version stands.
//
// The five are not decoration; each one answers a different question a person
// actually asks. A `draft` is written and validated and nothing has been said
// about it. A `proposed` draft has a corpus's proposals recorded against it
// and is waiting for somebody to decide. An `approved` version is the output
// of Extend — somebody named accepted named types — and is not yet in force.
// A `published` version is the one `current` hands out. A `retired` one was
// published and was replaced, and it stays exactly as it was: every graph
// extracted under it names it in its provenance, so deleting it would orphan
// the only record of what those facts were checked against.
type State string

const (
	Draft     State = "draft"
	Proposed  State = "proposed"
	Approved  State = "approved"
	Published State = "published"
	Retired   State = "retired"
)

// Act is one signed decision, shaped for the decision ledger this becomes an
// entry in: kind, actor, at, subject, note. Nothing here is inferred — an act
// exists because somebody performed it, and the actor is the name they gave
// or the key they held.
type Act struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Actor   string    `json:"actor"`
	At      time.Time `json:"at"`
	Subject string    `json:"subject"`
	Note    string    `json:"note,omitempty"`

	// Key is the door's own record of who called: the id of the key the
	// request authenticated as. It is the same string as Actor for every act
	// but an approval, which is signed with a person's name typed into the
	// body — a name nobody checked and the one field here that has to survive
	// an argument two years from now.
	//
	// It is not a column. The table has held five acts since it existed and
	// adding one to it would mean either a migration this project does not
	// have or a column that is empty for every row written before now; what
	// Key is for is the ledger mirror (ledger.go), which happens in the same
	// call that supplied it. An act read back out of the table therefore
	// carries no Key, which is honest: the table never knew it.
	Key string `json:"-"`
}

// The kinds this package records. They are namespaced because the ledger will
// hold the load's entries beside them.
const (
	ActDraft   = "ontology.draft"
	ActPropose = "ontology.propose"
	ActApprove = "ontology.approve"
	ActPublish = "ontology.publish"
	ActRetire  = "ontology.retire"
)

// Version is one edit of one vocabulary.
type Version struct {
	// ID is the ontology's own id — "sds@3" — and the primary key. It is the
	// same string alchemy stamps into every record's Provenance.Ontology, so a
	// fact in the brain and a row in this table are joinable by eye.
	ID string `json:"id"`
	// Lineage is the half of the id before the "@". Publication is per
	// lineage: "sds@3" replaces "sds@2" and says nothing about "code@1".
	Lineage string `json:"lineage"`
	State   State  `json:"state"`
	// Parent is the version Extend was run against. Empty for a version
	// somebody wrote by hand.
	Parent string `json:"parent,omitempty"`
	// Document is the JSON a client pastes into CreateJob — Ontology.Document()
	// verbatim for an approved version, and the body as supplied for a draft.
	Document json.RawMessage `json:"document"`

	// Part, Proposals and ProposedFrom are the recorded proposal: which job
	// asked, which part of the vocabulary it was read under, and what it
	// wanted. They are on the version rather than in a table of their own
	// because a proposal is a question about one document and dies with it.
	Part         string             `json:"part,omitempty"`
	Proposals    []alchemy.Proposal `json:"proposals,omitempty"`
	ProposedFrom string             `json:"proposed_from,omitempty"`

	CreatedBy   string     `json:"created_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ApprovedBy  string     `json:"approved_by,omitempty"`
	ApprovedAt  *time.Time `json:"approved_at,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	RetiredAt   *time.Time `json:"retired_at,omitempty"`
	Note        string     `json:"note,omitempty"`
}

// The refusals, as values, so an HTTP surface can turn each into the status it
// deserves rather than reading error strings.
var (
	// ErrNotFound is no such version, or no published version in a lineage.
	ErrNotFound = errors.New("ontologies: no such version")
	// ErrExists is an id already in the table. Ids are not reused: a second
	// document under "sds@3" would leave two vocabularies with one name, which
	// is the falsifiability checkID exists to protect.
	ErrExists = errors.New("ontologies: that version id is already recorded")
	// ErrState is an act the version's state does not admit.
	ErrState = errors.New("ontologies: the version is not in a state for that")
	// ErrUnsigned is a decision nobody is named for.
	ErrUnsigned = errors.New("ontologies: the decision needs a name")
	// ErrInvalid is a malformed request — a document that does not load, a
	// proposal nobody proposed.
	ErrInvalid = errors.New("ontologies: refused")
)

// lineageOf is the half of an id before the "@". ontology.Load has already
// refused an id without one by the time this is called, so the fallback is
// only for a row read back from a table somebody edited by hand.
func lineageOf(id string) string {
	name, _, ok := strings.Cut(id, "@")
	if !ok || strings.TrimSpace(name) == "" {
		return strings.TrimSpace(id)
	}
	return strings.TrimSpace(name)
}

// mintID names an act. Not a sequence: a BIGSERIAL and an INTEGER PRIMARY KEY
// AUTOINCREMENT are the one thing the two dialects cannot be handed the same
// DDL for, and an act does not need its id to be ordered — `at` orders them.
func mintID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("ontologies: mint act id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
