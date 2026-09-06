package ontologies

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/liliang-cn/alchemy/pkg/alchemy"
	"github.com/liliang-cn/alchemy/pkg/ontology"
)

// Draft records a vocabulary somebody wrote.
//
// The document is validated by ontology.Load and by nothing else here: a
// relation pointing at an entity type nobody declared, a misspelled part, an
// id with no version — every one of those is a rule alchemy already enforces,
// and enforcing it a second time in a slightly different way is how two
// implementations of one rule start disagreeing. What this adds is that the
// document is now somewhere, under the id it declares.
func (s *Store) Draft(ctx context.Context, document []byte, by, note string) (Version, error) {
	o, err := ontology.Load(bytes.NewReader(document))
	if err != nil {
		return Version{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	// Round-tripped through Document rather than stored as supplied: two
	// clients whose formatting differs would otherwise store two byte strings
	// for one vocabulary, and `current` hands these bytes straight to a
	// client. Load has already refused anything Document could not render.
	body, err := o.Document()
	if err != nil {
		return Version{}, fmt.Errorf("ontologies: rendering %s: %w", o.ID, err)
	}

	at := s.at()
	v := Version{
		ID: o.ID, Lineage: lineageOf(o.ID), State: Draft, Document: body,
		CreatedBy: by, CreatedAt: at, Note: note,
	}
	const q = `INSERT INTO athanor_ontology_versions
		(id, lineage, body, state, parent, part, proposals, proposed_from,
		 created_by, created_at, approved_by, approved_at, published_at, retired_at, note)
		VALUES (?, ?, ?, ?, '', '', '[]', '', ?, ?, '', NULL, NULL, NULL, ?)`
	if _, err := s.exec(ctx, q, v.ID, v.Lineage, string(body), string(Draft), by, at, note); err != nil {
		if _, getErr := s.Get(ctx, o.ID); getErr == nil {
			return Version{}, fmt.Errorf("%w: %s", ErrExists, o.ID)
		}
		return Version{}, fmt.Errorf("ontologies: draft %s: %w", o.ID, err)
	}
	act, err := s.record(ctx, nil, Act{Kind: ActDraft, Actor: by, Key: by, At: at, Subject: v.ID, Note: note})
	if err != nil {
		return Version{}, err
	}
	// The draft stands whether or not the audit view hears about it.
	_ = s.mirror(ctx, act)
	return v, nil
}

// Propose records a corpus's proposals against a version.
//
// The proposals are not recomputed here and could not be: they are what a run
// under this vocabulary observed, which is a fact about a job. The caller pulls
// them through alchemy's own GetResult — the same door /athanor/loads asks, and
// for the same reason, so a held job is refused by the service that knows it is
// held rather than by a second opinion here.
//
// A draft moves to `proposed`. A published version does not move: retiring the
// vocabulary that is in force because somebody asked a question about it would
// be absurd, and the question is recorded either way. What state a version is
// in and what has been proposed against it are two different facts, and this is
// the one place they could have been confused.
func (s *Store) Propose(ctx context.Context, id, job, part string, proposals []alchemy.Proposal, by, note string) (Version, error) {
	v, err := s.Get(ctx, id)
	if err != nil {
		return Version{}, err
	}
	if v.State == Retired {
		return Version{}, fmt.Errorf("%w: %s is retired; propose against the version that is current", ErrState, id)
	}
	if len(proposals) == 0 {
		return Version{}, fmt.Errorf("%w: that job proposed no types — its corpus used nothing %s does not already declare", ErrInvalid, id)
	}
	if part == "" {
		// CreateJob's own default, kept: empty means prose, because a document
		// is prose and every request written before the field existed meant one.
		part = string(ontology.PartProse)
	}
	encoded, err := json.Marshal(proposals)
	if err != nil {
		return Version{}, fmt.Errorf("ontologies: recording proposals for %s: %w", id, err)
	}

	state := v.State
	if state == Draft {
		state = Proposed
	}
	at := s.at()
	const q = `UPDATE athanor_ontology_versions
		SET state = ?, part = ?, proposals = ?, proposed_from = ? WHERE id = ?`
	if _, err := s.exec(ctx, q, string(state), part, string(encoded), job, id); err != nil {
		return Version{}, fmt.Errorf("ontologies: propose against %s: %w", id, err)
	}
	act, err := s.record(ctx, nil, Act{
		Kind: ActPropose, Actor: by, Key: by, At: at, Subject: id,
		Note: proposeNote(job, part, proposals, note),
	})
	if err != nil {
		return Version{}, err
	}
	_ = s.mirror(ctx, act)
	v.State, v.Part, v.Proposals, v.ProposedFrom = state, part, proposals, job
	return v, nil
}

func proposeNote(job, part string, proposals []alchemy.Proposal, note string) string {
	types := make([]string, 0, len(proposals))
	for _, p := range proposals {
		types = append(types, p.Type)
	}
	line := fmt.Sprintf("from job %s, part %s: %s", job, part, strings.Join(types, ", "))
	if note != "" {
		line += " — " + note
	}
	return line
}

// Approval is the decision: which of the recorded proposals to declare, who
// decided, and why.
type Approval struct {
	// Accept names types from the version's recorded proposals, matched
	// case-insensitively because alchemy folds type names that way and a model
	// that returned MEMBER_OF once and Member_of the next time meant one type.
	// A name nobody proposed is a refusal rather than a silent no-op: the
	// caller believes they accepted something.
	Accept []string
	// By is the person. Empty is refused — here and again inside Extend, which
	// says why better than this comment could: a judgement about what a type
	// means, and one nobody is named for, cannot be argued with later.
	By string
	// Note is why.
	Note string
	// Key is the id of the key the request came in on, when a door supplied
	// one. By is what the approval is signed with and Key is who actually
	// called; when they differ, the ledger keeps both — see Act.Key.
	Key string
	// NewID overrides the id of the new version. Empty increments the version
	// half, which Extend does and refuses to invent when it is not a number.
	NewID string
	// Part overrides which part the accepted types are declared in. Empty is
	// the part recorded when the proposals were.
	Part string
}

// Approve runs Extend over the accepted proposals and stores the result as a
// new version whose parent is the one approved from.
//
// The new version is `approved` and not `published`. That separation is the
// whole point of having states: approving says the types are right, publishing
// says every job from now on is checked against them, and they are decisions a
// person may want to make an hour apart.
func (s *Store) Approve(ctx context.Context, id string, req Approval) (Version, error) {
	by := strings.TrimSpace(req.By)
	if by == "" {
		return Version{}, fmt.Errorf("%w: approving an extension is a judgement about what a type means, and one nobody is named for cannot be argued with later", ErrUnsigned)
	}
	parent, err := s.Get(ctx, id)
	if err != nil {
		return Version{}, err
	}
	if len(parent.Proposals) == 0 {
		return Version{}, fmt.Errorf("%w: nothing has been proposed against %s", ErrState, id)
	}
	accept, err := pick(parent.Proposals, req.Accept)
	if err != nil {
		return Version{}, err
	}

	o, err := ontology.Load(bytes.NewReader(parent.Document))
	if err != nil {
		return Version{}, fmt.Errorf("ontologies: %s no longer loads: %w", id, err)
	}
	part := ontology.Part(req.Part)
	if part == "" {
		part = ontology.Part(parent.Part)
	}
	if part == "" {
		part = ontology.PartProse
	}
	extended, added, err := o.Extend(part, accept, by, req.NewID)
	if err != nil {
		// Extend's refusals are the substantive ones — a relation with an open
		// end, a widening of a type nobody declared — and they are written for
		// a person to read. They are passed through, not reworded.
		return Version{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	body, err := extended.Document()
	if err != nil {
		return Version{}, fmt.Errorf("ontologies: rendering %s: %w", extended.ID, err)
	}

	at := s.at()
	v := Version{
		ID: extended.ID, Lineage: lineageOf(extended.ID), State: Approved,
		Parent: id, Document: body, Part: string(part),
		CreatedBy: by, CreatedAt: at, ApprovedBy: by, ApprovedAt: &at, Note: req.Note,
	}
	const q = `INSERT INTO athanor_ontology_versions
		(id, lineage, body, state, parent, part, proposals, proposed_from,
		 created_by, created_at, approved_by, approved_at, published_at, retired_at, note)
		VALUES (?, ?, ?, ?, ?, ?, '[]', '', ?, ?, ?, ?, NULL, NULL, ?)`
	if _, err := s.exec(ctx, q, v.ID, v.Lineage, string(body), string(Approved), id, string(part),
		by, at, by, at, req.Note); err != nil {
		if _, getErr := s.Get(ctx, extended.ID); getErr == nil {
			return Version{}, fmt.Errorf("%w: %s", ErrExists, extended.ID)
		}
		return Version{}, fmt.Errorf("ontologies: approve %s: %w", extended.ID, err)
	}
	act, err := s.record(ctx, nil, Act{
		Kind: ActApprove, Actor: by, Key: firstNamed(req.Key, by), At: at, Subject: v.ID,
		Note: approveNote(id, added, req.Note),
	})
	if err != nil {
		return Version{}, err
	}
	_ = s.mirror(ctx, act)
	return v, nil
}

func approveNote(parent string, added []string, note string) string {
	line := fmt.Sprintf("extends %s with %s", parent, strings.Join(added, ", "))
	if len(added) == 0 {
		line = fmt.Sprintf("extends %s, declaring nothing new", parent)
	}
	if note != "" {
		line += " — " + note
	}
	return line
}

// pick resolves the accepted names against what was proposed.
func pick(proposals []alchemy.Proposal, names []string) ([]alchemy.Proposal, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: an approval that accepts nothing is not an approval", ErrInvalid)
	}
	byName := make(map[string]alchemy.Proposal, len(proposals))
	for _, p := range proposals {
		byName[strings.ToLower(strings.TrimSpace(p.Type))] = p
	}
	// The order the corpus proposed them in, not the order the caller listed
	// them: Extend applies entity proposals before the relations that name
	// them, and alchemy sorts a result's proposals so that order already holds.
	wanted := make(map[string]bool, len(names))
	for _, n := range names {
		key := strings.ToLower(strings.TrimSpace(n))
		if _, ok := byName[key]; !ok {
			return nil, fmt.Errorf("%w: %q was not proposed against this version", ErrInvalid, n)
		}
		wanted[key] = true
	}
	out := make([]alchemy.Proposal, 0, len(wanted))
	for _, p := range proposals {
		if wanted[strings.ToLower(strings.TrimSpace(p.Type))] {
			out = append(out, p)
		}
	}
	return out, nil
}

// Publication is who made a version current, under whose key, and why.
//
// A struct rather than three strings for Approval's reason: By and Key are
// both names, they mean different things, and two adjacent string parameters
// that can be swapped without a compile error is a bug waiting for a hurried
// afternoon.
type Publication struct {
	// By is who decided. Empty is refused: publishing decides what every job
	// from now on is checked against.
	By string
	// Key is the id of the key the request came in on. Empty falls back to By,
	// which is what a caller with no door in front of it has.
	Key string
	// Note is why.
	Note string
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

// Publish makes a version current and retires the one it replaces.
//
// One transaction, because the two halves are one fact. A retirement that
// committed without its publication would leave a lineage with nothing current
// and every client's next CreateJob without a vocabulary; a publication that
// committed without its retirement would leave two, and the partial unique
// index refuses that outright rather than letting `current` become a coin toss.
//
// It returns the published version and the id it retired, if any.
func (s *Store) Publish(ctx context.Context, id string, req Publication) (Version, string, error) {
	by := strings.TrimSpace(req.By)
	note := req.Note
	if by == "" {
		return Version{}, "", fmt.Errorf("%w: publishing decides what every job from now on is checked against", ErrUnsigned)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Version{}, "", fmt.Errorf("ontologies: publish %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	v, err := s.get(ctx, tx, id)
	if err != nil {
		return Version{}, "", err
	}
	if v.State == Published {
		return Version{}, "", fmt.Errorf("%w: %s is already the current vocabulary of %s", ErrState, id, v.Lineage)
	}

	at := s.at()
	// Re-publishing a retired version is a rollback, and it is allowed. The
	// alternative is that a vocabulary found to be wrong can only be undone by
	// approving a new version that undoes it, which is a worse record of what
	// happened than an act saying somebody went back.
	var acts []Act
	var retired string
	const findCurrent = `SELECT id FROM athanor_ontology_versions
		WHERE lineage = ? AND state = ? ORDER BY published_at DESC, id DESC`
	err = s.txQueryRow(ctx, tx, findCurrent, v.Lineage, string(Published)).Scan(&retired)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		retired = ""
	case err != nil:
		return Version{}, "", fmt.Errorf("ontologies: publish %s: finding the current version: %w", id, err)
	}
	if retired != "" {
		const q = `UPDATE athanor_ontology_versions SET state = ?, retired_at = ? WHERE id = ?`
		if _, err := s.txExec(ctx, tx, q, string(Retired), at, retired); err != nil {
			return Version{}, "", fmt.Errorf("ontologies: retire %s: %w", retired, err)
		}
		act, err := s.record(ctx, tx, Act{
			Kind: ActRetire, Actor: by, Key: firstNamed(req.Key, by), At: at,
			Subject: retired, Note: "replaced by " + id,
		})
		if err != nil {
			return Version{}, "", err
		}
		acts = append(acts, act)
	}
	const q = `UPDATE athanor_ontology_versions SET state = ?, published_at = ?, retired_at = NULL WHERE id = ?`
	if _, err := s.txExec(ctx, tx, q, string(Published), at, id); err != nil {
		return Version{}, "", fmt.Errorf("ontologies: publish %s: %w", id, err)
	}
	act, err := s.record(ctx, tx, Act{
		Kind: ActPublish, Actor: by, Key: firstNamed(req.Key, by), At: at,
		Subject: id, Note: publishNote(retired, note),
	})
	if err != nil {
		return Version{}, "", err
	}
	acts = append(acts, act)
	if err := tx.Commit(); err != nil {
		return Version{}, "", fmt.Errorf("ontologies: publish %s: %w", id, err)
	}
	// After the commit, never inside it: see Store.record.
	_ = s.mirror(ctx, acts...)

	v.State = Published
	v.PublishedAt = &at
	v.RetiredAt = nil
	return v, retired, nil
}

func publishNote(retired, note string) string {
	line := "made current"
	if retired != "" {
		line += ", retiring " + retired
	}
	if note != "" {
		line += " — " + note
	}
	return line
}
