package snapshots

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
	"github.com/liliang-cn/cortexdb/v2/pkg/graph"
)

// Comparing two named moments, in the product's own words.
//
// CortexDB's GraphDiff answers in storage's words — added, retracted, changed
// — and "changed" is where the interesting half hides. A fact whose _grade
// went from verified to asserted is reported by the storage layer as a row
// that says something different, in exactly the same way as a fact whose label
// gained a hyphen. An operator asking what happened to the shelf between March
// and now is asking one question above all others, and it is that one: what
// stopped being checked. So a grade change is its own kind here, it is counted
// separately, and a fall down the ladder is counted again.

// ChangeKind is why a fact is in the diff.
type ChangeKind string

const (
	// Added: in the later moment and not the earlier one.
	Added ChangeKind = "added"
	// Withdrawn: in the earlier moment and not the later one. The storage
	// layer calls it retracted, which is accurate about the row and says more
	// than this layer knows — a fact can leave because it was superseded, or
	// because a load was replaced. Withdrawn is what is true either way.
	Withdrawn ChangeKind = "withdrawn"
	// Regraded: in both moments, saying the same thing, graded differently.
	Regraded ChangeKind = "regraded"
	// Changed: in both moments, saying something different, graded the same.
	Changed ChangeKind = "changed"
)

// Change is one fact that is not the same in the two moments.
type Change struct {
	ID   string     `json:"id"`
	Kind ChangeKind `json:"kind"`
	// Edge distinguishes a node from an edge without a reader guessing from
	// the shape of the id. A graph's assertions are mostly edges, and a diff
	// that made them look like nodes would misdescribe what moved.
	Edge bool   `json:"edge"`
	Type string `json:"type,omitempty"`
	// Label is what a person would call it: a node's content, or an edge's two
	// ends. Carried rather than left to the caller to fetch, because the fact
	// may no longer exist and a second read would come back empty.
	Label string `json:"label,omitempty"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`

	// WasGrade and NowGrade are the knowledge contract's ladder at each end.
	// Empty means untagged — the record carried no _grade at all, which is a
	// different thing from a grade this build does not recognise and is why
	// neither is turned into a word here.
	WasGrade string `json:"was_grade,omitempty"`
	NowGrade string `json:"now_grade,omitempty"`
	// Fell is a grade that moved down the ladder. It is the one number an
	// operator acts on: a fact that quietly stopped being verified is not
	// visible in a count of nodes, and it is the thing a diff exists to say.
	Fell bool `json:"fell,omitempty"`
}

// DiffCounts is the page in one line.
//
// Of the page, not of the whole change set. A diff is paged — CortexDB walks
// two id-ordered streams and never holds a graph in memory, which is what
// makes it safe on a brain with four hundred thousand nodes — and totalling
// the rest would mean walking every page to answer a summary. Truncated says
// there is more; the honest summary of a bounded read is a summary of what was
// read.
type DiffCounts struct {
	Added     int `json:"added"`
	Withdrawn int `json:"withdrawn"`
	Regraded  int `json:"regraded"`
	Changed   int `json:"changed"`
	// Fell is how many of the regrades moved down.
	Fell int `json:"fell"`
}

// Diff is the comparison of two named moments.
type Diff struct {
	// From and To are the snapshots themselves, not just their names: each
	// carries its counts and its grade ladder, so "the shelf lost four hundred
	// verified facts" is readable from the two ends without opening a page of
	// changes at all.
	From Snapshot `json:"from"`
	To   Snapshot `json:"to"`

	Changes []Change   `json:"changes"`
	Counts  DiffCounts `json:"counts"`

	// NextCursor is non-empty when the limit cut the walk short. Pass it back
	// as DiffOptions.Cursor.
	NextCursor string `json:"next_cursor,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

// DiffOptions bounds a comparison. Every field is CortexDB's own, passed
// through: narrowing by type happens in the store, where it costs nothing, and
// a filter applied here after the page was cut would drop rows the cursor has
// already counted as read.
type DiffOptions struct {
	Limit     int
	Cursor    string
	NodeTypes []string
	EdgeTypes []string
}

// Diff compares two named moments.
//
// Both ends are snapshots rather than instants on purpose. An operator can ask
// CortexDB for a diff between two timestamps already; what they cannot do is
// be sure the timestamps are the ones somebody signed for, and a report whose
// ends were typed from memory is a report about a different afternoon.
func (s *Store) Diff(ctx context.Context, from, to string, opts DiffOptions) (Diff, error) {
	earlier, err := s.Get(ctx, from)
	if err != nil {
		return Diff{}, err
	}
	later, err := s.Get(ctx, to)
	if err != nil {
		return Diff{}, err
	}
	if later.At.Before(earlier.At) {
		// Refused rather than silently swapped, which is CortexDB's rule one
		// layer down and right for the same reason: a caller with the ends the
		// wrong way round is asking a different question from the one a swap
		// would answer, and would read "withdrawn" where it meant "added".
		return Diff{}, fmt.Errorf("%w: %s (%s) is after %s (%s) — a diff runs forwards",
			ErrInvalid, earlier.Name, earlier.At.Format(time.RFC3339),
			later.Name, later.At.Format(time.RFC3339))
	}

	result, err := s.db.GraphDiff(ctx, earlier.At, later.At, graph.DiffOptions{
		Limit:     opts.Limit,
		Cursor:    opts.Cursor,
		NodeTypes: opts.NodeTypes,
		EdgeTypes: opts.EdgeTypes,
	})
	if err != nil {
		return Diff{}, fmt.Errorf("snapshots: diff %s..%s: %w", earlier.Name, later.Name, err)
	}

	out := Diff{From: earlier, To: later, Changes: []Change{}}
	for _, c := range result.Nodes {
		change := Change{ID: c.ID}
		if c.Before != nil {
			change.WasGrade = gradeOf(c.Before.Properties)
			change.Type, change.Label = c.Before.NodeType, c.Before.Content
		}
		if c.After != nil {
			change.NowGrade = gradeOf(c.After.Properties)
			change.Type, change.Label = c.After.NodeType, c.After.Content
		}
		out.Changes = append(out.Changes, classify(change, c.Kind))
	}
	for _, c := range result.Edges {
		change := Change{ID: c.ID, Edge: true}
		if c.Before != nil {
			change.WasGrade = gradeOf(c.Before.Properties)
			change.Type, change.From, change.To = c.Before.EdgeType, c.Before.From, c.Before.To
		}
		if c.After != nil {
			change.NowGrade = gradeOf(c.After.Properties)
			change.Type, change.From, change.To = c.After.EdgeType, c.After.From, c.After.To
		}
		change.Label = change.From + " " + change.Type + " " + change.To
		out.Changes = append(out.Changes, classify(change, c.Kind))
	}
	for _, c := range out.Changes {
		switch c.Kind {
		case Added:
			out.Counts.Added++
		case Withdrawn:
			out.Counts.Withdrawn++
		case Regraded:
			out.Counts.Regraded++
		case Changed:
			out.Counts.Changed++
		}
		if c.Fell {
			out.Counts.Fell++
		}
	}
	out.NextCursor, out.Truncated = result.NextCursor, result.Truncated
	return out, nil
}

// classify turns the storage layer's word into this product's, and decides
// whether a fact that changed changed in the way anybody cares about.
func classify(c Change, kind graph.DiffKind) Change {
	switch kind {
	case graph.DiffAdded:
		c.Kind = Added
	case graph.DiffRetracted:
		c.Kind = Withdrawn
		// A withdrawn fact has fallen off the ladder entirely, and saying so
		// would drown the number that matters: Fell is for a fact that is
		// still there and is trusted less than it was. What happened to a
		// withdrawal is visible in its own count.
	default:
		c.Kind = Changed
		if c.WasGrade != c.NowGrade {
			c.Kind = Regraded
			c.Fell = fell(c.WasGrade, c.NowGrade)
		}
	}
	return c
}

// The ladder, highest first. It is the knowledge contract's own listing, not a
// judgement invented here, and the comparison it supports is deliberately one
// bit wide: `fell` draws an operator's eye to a fact that is trusted less than
// it was. Nothing here claims that refused is one step worse than held or that
// the gaps are equal — they are not numbers, and reading them as a score is
// the mistake the contract's own note about flattening warns against.
//
// Untagged — no _grade at all — sits at the bottom because a fact that lost
// its grade is a fact nobody can now say anything about. A grade this build
// does not recognise has no rung at all and so cannot fall: a newer contract
// is not a regression, and guessing would report one.
var ladder = map[string]int{
	cortexdb.GradeVerified:       5,
	cortexdb.GradeSelfConsistent: 4,
	cortexdb.GradeAsserted:       3,
	cortexdb.GradeHeld:           2,
	cortexdb.GradeRefused:        1,
	"":                           0,
}

func fell(was, now string) bool {
	from, knownFrom := ladder[was]
	to, knownTo := ladder[now]
	return knownFrom && knownTo && to < from
}

// gradeOf reads the knowledge contract's grade off a version's properties.
//
// The properties arrive as raw JSON text rather than a map — NodeVersion and
// EdgeVersion carry what the column held, because a diff of a thousand rows
// would otherwise decode a thousand maps nobody looks at — so this decodes the
// one key it needs and reports "untagged" for anything it cannot read. A
// malformed properties column is a fact with no legible grade, which is what
// the empty string already means here, and failing the whole diff over one bad
// row would be a report that stops at the first thing worth seeing.
func gradeOf(properties string) string {
	if properties == "" {
		return ""
	}
	var props map[string]any
	if err := json.Unmarshal([]byte(properties), &props); err != nil {
		return ""
	}
	grade, _ := props[cortexdb.KeyGrade].(string)
	return grade
}
