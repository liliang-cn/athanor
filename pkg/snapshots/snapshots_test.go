package snapshots_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/liliang-cn/athanor/internal/braintest"
	"github.com/liliang-cn/athanor/pkg/snapshots"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
	"github.com/liliang-cn/cortexdb/v2/pkg/graph"
)

// The store's own suite, on both backends. What it is about is the half that
// is this package's: the table, the acts on it, and the translation of
// CortexDB's diff into the product's words. The door is pkg/server's.

type backend struct {
	name  string
	store *snapshots.Store
	db    *cortexdb.DB
	// nodeType is unique per backend so that a PostgreSQL run, which shares
	// one database across every test in the package, can narrow a diff to the
	// facts this test wrote. The counts on a snapshot are over the whole
	// graph, so every assertion about them is a difference and never a total.
	nodeType string
}

func backends(t *testing.T) []backend {
	t.Helper()
	var out []backend
	for _, b := range braintest.Backends(t) {
		db := b.Open(t)
		s, err := snapshots.New(db)
		if err != nil {
			t.Fatalf("snapshots.New on %s: %v", b.Name, err)
		}
		if got := s.Dialect().Kind(); got != b.Kind {
			t.Fatalf("the %s store speaks %s", b.Name, got)
		}
		out = append(out, backend{name: b.Name, store: s, db: db, nodeType: braintest.Unique("Fact")})
	}
	return out
}

// write puts one fact in the graph at the grade it is asserted under. It is
// the shape alchemy's connector writes — a node carrying the knowledge
// contract's keys — without needing alchemy, a model or a corpus.
func write(t *testing.T, db *cortexdb.DB, id, nodeType, content, grade string) {
	t.Helper()
	node := &graph.GraphNode{
		ID: id, Content: content, NodeType: nodeType,
		// The brain under test is opened at four dimensions (braintest), and
		// the graph refuses a node with no vector at all. Nothing here searches
		// by it — the vector is deliberately not versioned, so re-writing this
		// node with a different grade is a change to what it says and not to
		// how it is found.
		Vector: []float32{0, 0, 0, 1},
		Properties: map[string]any{
			cortexdb.KeySource:   "runbook.md",
			cortexdb.KeyProducer: cortexdb.ProducerLLMExtract,
			cortexdb.KeyGrade:    grade,
			cortexdb.KeyAt:       time.Now().UTC().Format(time.RFC3339),
		},
	}
	if err := db.Graph().UpsertNode(context.Background(), node); err != nil {
		t.Fatalf("write %s: %v", id, err)
	}
	// The graph's versions are ordered by a clock, and two facts written
	// inside one microsecond are two facts an as-of read cannot put on either
	// side of an instant between them. A test that raced that would fail once
	// a month on somebody else's machine.
	time.Sleep(2 * time.Millisecond)
}

func TestAMomentIsNamedCountedAndSigned(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			name := braintest.Unique("march")
			write(t, b.db, braintest.Unique("node"), b.nodeType, "hp promotes sds-meta", cortexdb.GradeAsserted)

			snap, err := b.store.Take(ctx, snapshots.Taking{
				Name: name, By: "liliang", Actor: "operator", Note: "before the migration",
				Footing: snapshots.Footing{
					Ontologies: map[string]string{"sds": "sds@2"},
					Loads:      []string{"decision:athanor:load:job-7:march"},
					LoadCount:  1,
				},
			})
			if err != nil {
				t.Fatalf("take: %v", err)
			}
			if snap.State != snapshots.Kept || snap.Actor != "operator" || snap.By != "liliang" {
				t.Fatalf("the moment landed as %+v", snap)
			}
			if snap.At.IsZero() || snap.Counts.Nodes == 0 {
				t.Fatalf("the moment counted nothing: %+v", snap)
			}
			if snap.Grades.Asserted.Nodes == 0 {
				t.Fatalf("the ladder was not recorded: %+v", snap.Grades)
			}

			// Read back: what the row holds is what Take reported, including
			// what the moment rested on.
			got, err := b.store.Get(ctx, name)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.Footing.Ontologies["sds"] != "sds@2" || len(got.Footing.Loads) != 1 || got.Footing.LoadCount != 1 {
				t.Fatalf("the footing did not survive the round trip: %+v", got.Footing)
			}
			if !got.At.Equal(snap.At) {
				t.Fatalf("the instant changed on its way through the column: %s then %s", snap.At, got.At)
			}
			if got.Counts != snap.Counts || !reflect.DeepEqual(got.Grades, snap.Grades) {
				t.Fatalf("the counts changed on their way through the column: %+v then %+v", snap, got)
			}
		})
	}
}

func TestAMomentNobodyIsNamedForIsRefused(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			for _, by := range []string{"", "   "} {
				_, err := b.store.Take(ctx, snapshots.Taking{Name: braintest.Unique("x"), By: by, Actor: "operator"})
				if !errors.Is(err, snapshots.ErrUnsigned) {
					t.Fatalf("a snapshot signed by %q was accepted: %v", by, err)
				}
			}
			// And one no key presented itself for, which is what a door with a
			// hole in it would send.
			if _, err := b.store.Take(ctx, snapshots.Taking{Name: braintest.Unique("x"), By: "liliang"}); !errors.Is(err, snapshots.ErrUnsigned) {
				t.Fatalf("a snapshot with no key was accepted: %v", err)
			}
			// A name the door could not spell an act with.
			if _, err := b.store.Take(ctx, snapshots.Taking{Name: "night:ly", By: "liliang", Actor: "operator"}); !errors.Is(err, snapshots.ErrInvalid) {
				t.Fatalf("a name carrying a colon was accepted: %v", err)
			}
			if _, err := b.store.Take(ctx, snapshots.Taking{Name: "  ", By: "liliang", Actor: "operator"}); !errors.Is(err, snapshots.ErrInvalid) {
				t.Fatalf("a nameless moment was accepted: %v", err)
			}
		})
	}
}

func TestANameIsOneMomentUntilSomebodySaysReplace(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			name := braintest.Unique("nightly")
			first, err := b.store.Take(ctx, snapshots.Taking{Name: name, By: "liliang", Actor: "operator"})
			if err != nil {
				t.Fatalf("take: %v", err)
			}
			time.Sleep(2 * time.Millisecond)
			if _, err := b.store.Take(ctx, snapshots.Taking{Name: name, By: "liliang", Actor: "operator"}); !errors.Is(err, snapshots.ErrExists) {
				t.Fatalf("one name became two moments: %v", err)
			}
			moved, err := b.store.Take(ctx, snapshots.Taking{Name: name, By: "liliang", Actor: "operator", Replace: true})
			if err != nil {
				t.Fatalf("replace: %v", err)
			}
			if !moved.At.After(first.At) {
				t.Fatalf("replacing did not move the moment: %s then %s", first.At, moved.At)
			}
			if moved.Replaced == nil || !moved.Replaced.Equal(first.At) {
				t.Fatalf("the moment it used to name was not kept: %+v", moved.Replaced)
			}
		})
	}
}

func TestADroppedMomentIsRetiredAndNotDeleted(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			name := braintest.Unique("scratch")
			taken, err := b.store.Take(ctx, snapshots.Taking{Name: name, By: "liliang", Actor: "operator"})
			if err != nil {
				t.Fatalf("take: %v", err)
			}
			dropped, err := b.store.Drop(ctx, name, snapshots.Dropping{By: "liliang", Actor: "operator", Note: "taken by mistake"})
			if err != nil {
				t.Fatalf("drop: %v", err)
			}
			if dropped.State != snapshots.Dropped || dropped.DroppedAt == nil || dropped.DroppedBy != "liliang" {
				t.Fatalf("the drop was not recorded: %+v", dropped)
			}
			// The row and its counts survive, which is the whole reason drop is
			// not a delete.
			got, err := b.store.Get(ctx, name)
			if err != nil {
				t.Fatalf("a dropped moment is gone: %v", err)
			}
			if got.Counts != taken.Counts || !got.At.Equal(taken.At) {
				t.Fatalf("dropping changed what the moment held: %+v", got)
			}
			// And it is off the list a person reads.
			kept, err := b.store.List(ctx, snapshots.ListQuery{})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			for _, s := range kept {
				if s.Name == name {
					t.Fatalf("a dropped moment is still offered: %+v", s)
				}
			}
			retired, err := b.store.List(ctx, snapshots.ListQuery{State: snapshots.Dropped})
			if err != nil {
				t.Fatalf("list dropped: %v", err)
			}
			if !named(retired, name) {
				t.Fatalf("a dropped moment cannot be found at all: %+v", retired)
			}
			// Dropping twice is somebody making sure, not an error.
			if _, err := b.store.Drop(ctx, name, snapshots.Dropping{By: "liliang", Actor: "operator"}); err != nil {
				t.Fatalf("dropping twice: %v", err)
			}
			// An unsigned drop is refused, and so is one on nothing.
			if _, err := b.store.Drop(ctx, name, snapshots.Dropping{Actor: "operator"}); !errors.Is(err, snapshots.ErrUnsigned) {
				t.Fatalf("an unsigned drop was accepted: %v", err)
			}
			if _, err := b.store.Drop(ctx, braintest.Unique("never"), snapshots.Dropping{By: "liliang", Actor: "operator"}); !errors.Is(err, snapshots.ErrNotFound) {
				t.Fatalf("dropping a moment nobody named: %v", err)
			}
		})
	}
}

func TestListIsConfinedToOneKeysOwnMoments(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			mine, theirs := braintest.Unique("mine"), braintest.Unique("theirs")
			if _, err := b.store.Take(ctx, snapshots.Taking{Name: mine, By: "hermes", Actor: "hermes"}); err != nil {
				t.Fatalf("take: %v", err)
			}
			if _, err := b.store.Take(ctx, snapshots.Taking{Name: theirs, By: "liliang", Actor: "operator"}); err != nil {
				t.Fatalf("take: %v", err)
			}
			confined, err := b.store.List(ctx, snapshots.ListQuery{Actor: "hermes"})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if !named(confined, mine) || named(confined, theirs) {
				t.Fatalf("a confined listing saw somebody else's moment: %+v", confined)
			}
		})
	}
}

func TestADiffReportsWhatWasAddedWithdrawnAndRegraded(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			kept := braintest.Unique("kept")
			falling := braintest.Unique("falling")
			going := braintest.Unique("going")
			arriving := braintest.Unique("arriving")

			write(t, b.db, kept, b.nodeType, "hp is a node", cortexdb.GradeAsserted)
			write(t, b.db, falling, b.nodeType, "hp promotes sds-meta", cortexdb.GradeVerified)
			write(t, b.db, going, b.nodeType, "sds-meta is on hp", cortexdb.GradeAsserted)

			before := braintest.Unique("before")
			if _, err := b.store.Take(ctx, snapshots.Taking{Name: before, By: "liliang", Actor: "operator"}); err != nil {
				t.Fatalf("take: %v", err)
			}
			time.Sleep(2 * time.Millisecond)

			// A fact arrives, a fact leaves, and — the one that matters — a
			// fact somebody had checked quietly stops being checked.
			write(t, b.db, arriving, b.nodeType, "hp is in rack 4", cortexdb.GradeAsserted)
			write(t, b.db, falling, b.nodeType, "hp promotes sds-meta", cortexdb.GradeAsserted)
			if err := b.db.Graph().RetractNodeAt(ctx, going, time.Now().UTC()); err != nil {
				t.Fatalf("retract: %v", err)
			}
			time.Sleep(2 * time.Millisecond)

			after := braintest.Unique("after")
			if _, err := b.store.Take(ctx, snapshots.Taking{Name: after, By: "liliang", Actor: "operator"}); err != nil {
				t.Fatalf("take: %v", err)
			}

			diff, err := b.store.Diff(ctx, before, after, snapshots.DiffOptions{NodeTypes: []string{b.nodeType}})
			if err != nil {
				t.Fatalf("diff: %v", err)
			}
			byID := map[string]snapshots.Change{}
			for _, c := range diff.Changes {
				byID[c.ID] = c
			}
			if got := byID[arriving].Kind; got != snapshots.Added {
				t.Errorf("the fact that arrived is %q: %+v", got, byID[arriving])
			}
			if got := byID[going].Kind; got != snapshots.Withdrawn {
				t.Errorf("the fact that left is %q: %+v", got, byID[going])
			}
			if _, moved := byID[kept]; moved {
				t.Errorf("a fact nobody touched is in the diff: %+v", byID[kept])
			}
			fall := byID[falling]
			if fall.Kind != snapshots.Regraded {
				t.Fatalf("a fact that stopped being verified reads as %q: %+v", fall.Kind, fall)
			}
			if fall.WasGrade != cortexdb.GradeVerified || fall.NowGrade != cortexdb.GradeAsserted {
				t.Fatalf("the two grades were not carried: %+v", fall)
			}
			if !fall.Fell {
				t.Fatalf("verified to asserted did not read as a fall: %+v", fall)
			}
			if fall.Label == "" || fall.Type != b.nodeType {
				t.Fatalf("the changed fact cannot be recognised: %+v", fall)
			}
			if diff.Counts.Added != 1 || diff.Counts.Withdrawn != 1 || diff.Counts.Regraded != 1 || diff.Counts.Fell != 1 {
				t.Fatalf("the summary does not match the page: %+v", diff.Counts)
			}
			// The two ends carry their own ladders, so the fall is visible
			// without reading a change at all.
			if diff.From.Grades.Verified.Nodes <= diff.To.Grades.Verified.Nodes {
				t.Fatalf("the shelf did not lose a verified fact: %+v then %+v", diff.From.Grades, diff.To.Grades)
			}
		})
	}
}

func TestADiffRunsForwardsAndOnlyBetweenMomentsThatExist(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			first, second := braintest.Unique("first"), braintest.Unique("second")
			if _, err := b.store.Take(ctx, snapshots.Taking{Name: first, By: "liliang", Actor: "operator"}); err != nil {
				t.Fatalf("take: %v", err)
			}
			time.Sleep(2 * time.Millisecond)
			if _, err := b.store.Take(ctx, snapshots.Taking{Name: second, By: "liliang", Actor: "operator"}); err != nil {
				t.Fatalf("take: %v", err)
			}
			if _, err := b.store.Diff(ctx, second, first, snapshots.DiffOptions{}); !errors.Is(err, snapshots.ErrInvalid) {
				t.Fatalf("a backwards diff was answered: %v", err)
			}
			if _, err := b.store.Diff(ctx, braintest.Unique("never"), second, snapshots.DiffOptions{}); !errors.Is(err, snapshots.ErrNotFound) {
				t.Fatalf("a diff against a moment nobody named: %v", err)
			}
			// A dropped moment is still comparable: it named a real instant and
			// the record says who retired it.
			if _, err := b.store.Drop(ctx, first, snapshots.Dropping{By: "liliang", Actor: "operator"}); err != nil {
				t.Fatalf("drop: %v", err)
			}
			if _, err := b.store.Diff(ctx, first, second, snapshots.DiffOptions{}); err != nil {
				t.Fatalf("a dropped moment cannot be compared against: %v", err)
			}
		})
	}
}

func named(list []snapshots.Snapshot, name string) bool {
	for _, s := range list {
		if s.Name == name {
			return true
		}
	}
	return false
}
