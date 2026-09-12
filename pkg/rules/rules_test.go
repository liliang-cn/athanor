package rules_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liliang-cn/athanor/internal/braintest"
	"github.com/liliang-cn/athanor/pkg/rules"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
	"github.com/liliang-cn/cortexdb/v2/pkg/graph"
)

// The engine is faked here, and the reason is not speed. The two answers this
// package has to get right — a run that derived edges, and a run that hit a cap
// and wrote nothing — are otherwise only reachable by building a graph shaped
// to produce them, which would make every test about the graph rather than
// about the workflow. That the real engine is driven correctly is proved in
// pkg/server, against a real brain and a real load.
type fakeEngine struct {
	mu   sync.Mutex
	got  []cortexdb.RulesApplyRequest
	resp *cortexdb.RulesApplyResponse
	err  error
}

func (f *fakeEngine) ApplyRules(_ context.Context, req cortexdb.RulesApplyRequest) (*cortexdb.RulesApplyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, req)
	if f.err != nil {
		return nil, f.err
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return &cortexdb.RulesApplyResponse{
		RuleIDs: []string{req.Rules[0].ID}, Iterations: 2, CandidateEdges: 7,
		CreatedEdgeIDs: []string{"edge:inferred:one"},
		Edges: []cortexdb.RuleDerivedEdge{{
			EdgeID: "edge:inferred:one", FromNodeID: "a", ToNodeID: "c", EdgeType: "chains",
			Confidence: 0.9, SupportEdgeIDs: []string{"edge:a-b", "edge:b-c"},
		}},
		DryRun: req.DryRun,
	}, nil
}

func (f *fakeEngine) last(t *testing.T) cortexdb.RulesApplyRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) == 0 {
		t.Fatal("the engine was never asked to apply anything")
	}
	return f.got[len(f.got)-1]
}

// brokenLedger is the injected failure: a mirror that refuses every write.
type brokenLedger struct{}

func (brokenLedger) Record(context.Context, rules.Act) error {
	return errors.New("the ledger is unavailable")
}

// countingLedger remembers what it was handed, which is how the tests below
// assert that an act reached the audit view at all.
type countingLedger struct {
	mu   sync.Mutex
	acts []rules.Act
}

func (l *countingLedger) Record(_ context.Context, act rules.Act) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.acts = append(l.acts, act)
	return nil
}

func (l *countingLedger) kinds() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.acts))
	for _, a := range l.acts {
		out = append(out, a.Kind)
	}
	return out
}

// One suite, both backends — internal/braintest's, shared with pkg/ontologies
// and pkg/livedb, which own tables on the same handle and have the same reason
// to care that the SQL is true on each.
type backend struct {
	name   string
	store  *rules.Store
	engine *fakeEngine
	ledger *countingLedger
}

func backends(t *testing.T, opts ...rules.Option) []backend {
	t.Helper()
	var out []backend
	for _, b := range braintest.Backends(t) {
		engine := &fakeEngine{}
		ledger := &countingLedger{}
		all := append([]rules.Option{rules.WithEngine(engine), rules.WithLedger(ledger)}, opts...)
		s, err := rules.New(b.Open(t), all...)
		if err != nil {
			t.Fatalf("rules.New on %s: %v", b.Name, err)
		}
		if got := s.Dialect().Kind(); got != b.Kind {
			t.Fatalf("the %s store speaks %s", b.Name, got)
		}
		out = append(out, backend{name: b.Name, store: s, engine: engine, ledger: ledger})
	}
	return out
}

// declaration is one rule's document, in the form rules_save takes.
func declaration(id string) []byte {
	return []byte(fmt.Sprintf(`{"id":%q,"name":"the chain","text":"IF manages(?a, ?b) AND manages(?b, ?c) THEN manages_chain(?a, ?c)","note":"a manager's manager manages you"}`, id))
}

func lineage(t *testing.T) string {
	t.Helper()
	return braintest.Unique("chain")
}

func TestARuleIsDeclaredPutInForceAndReplaced(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			name := lineage(t)
			first, err := b.store.Draft(ctx, declaration(name+"@1"), "operator", "")
			if err != nil {
				t.Fatalf("draft: %v", err)
			}
			if first.State != rules.Draft || first.Lineage != name || first.Parent != "" {
				t.Fatalf("the draft landed as %+v", first)
			}
			// The text is the engine's rendering, not the caller's spacing.
			if !strings.HasPrefix(first.Text, "IF manages(?a, ?b)") || !strings.Contains(first.Text, "THEN manages_chain(?a, ?c)") {
				t.Fatalf("the rule was not rendered back: %q", first.Text)
			}

			if _, _, err := b.store.Publish(ctx, first.ID, rules.Signature{By: "liliang", Key: "operator"}); err != nil {
				t.Fatalf("publish: %v", err)
			}
			current, err := b.store.Current(ctx, name)
			if err != nil || current.ID != first.ID || current.PublishedBy != "liliang" {
				t.Fatalf("current is %+v (%v)", current, err)
			}

			// A second edit follows the one in force.
			second, err := b.store.Draft(ctx, declaration(name+"@2"), "operator", "tighter")
			if err != nil {
				t.Fatalf("second draft: %v", err)
			}
			if second.Parent != first.ID {
				t.Fatalf("the second edit follows %q, want %q", second.Parent, first.ID)
			}
			published, retired, err := b.store.Publish(ctx, second.ID, rules.Signature{By: "liliang", Key: "operator"})
			if err != nil {
				t.Fatalf("publish the second: %v", err)
			}
			if retired != first.ID || published.State != rules.Published {
				t.Fatalf("publishing %s retired %q", second.ID, retired)
			}
			// Retiring is not deleting: the first rule is still readable, and
			// so is everything it says about itself.
			was, err := b.store.Get(ctx, first.ID)
			if err != nil || was.State != rules.Retired || was.RetiredAt == nil || was.Text == "" {
				t.Fatalf("the replaced rule is %+v (%v)", was, err)
			}

			// Every act is in the table and every act reached the mirror.
			acts, err := b.store.Acts(ctx, "")
			if err != nil {
				t.Fatalf("acts: %v", err)
			}
			if len(acts) != 5 {
				t.Fatalf("acts = %d, want 5 (two drafts, two publishes, one retire): %+v", len(acts), acts)
			}
			if got := len(b.ledger.kinds()); got != 5 {
				t.Fatalf("the ledger saw %d acts, want 5: %v", got, b.ledger.kinds())
			}
		})
	}
}

func TestARuleThatIsNotAVersionedWellFormedDeclarationIsRefused(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			name := lineage(t)
			for _, c := range []struct {
				why, body string
				want      error
			}{
				{"no version half", `{"id":"chain","text":"IF a(?x, ?y) THEN b(?x, ?y)"}`, rules.ErrInvalid},
				{"no rule at all", `{"id":"` + name + `@9"}`, rules.ErrInvalid},
				{"unbound conclusion", `{"id":"` + name + `@9","text":"IF a(?x, ?y) THEN b(?x, ?z)"}`, rules.ErrInvalid},
				{"a misspelled field", `{"id":"` + name + `@9","txt":"IF a(?x, ?y) THEN b(?x, ?y)"}`, rules.ErrInvalid},
				{"its own grade", `{"id":"` + name + `@9","text":"IF a(?x, ?y) THEN b(?x, ?y)","metadata":{"_grade":"verified"}}`, rules.ErrInvalid},
				{"an enabled flag", `{"id":"` + name + `@9","text":"IF a(?x, ?y) THEN b(?x, ?y)","enabled":true}`, rules.ErrInvalid},
			} {
				if _, err := b.store.Draft(ctx, []byte(c.body), "operator", ""); !errors.Is(err, c.want) {
					t.Fatalf("a rule with %s was accepted: %v", c.why, err)
				}
			}
			// And nothing above left a row behind.
			if _, err := b.store.Get(ctx, name+"@9"); !errors.Is(err, rules.ErrNotFound) {
				t.Fatalf("a refused declaration was stored: %v", err)
			}

			if _, err := b.store.Draft(ctx, declaration(name+"@1"), "operator", ""); err != nil {
				t.Fatalf("draft: %v", err)
			}
			if _, err := b.store.Draft(ctx, declaration(name+"@1"), "operator", ""); !errors.Is(err, rules.ErrExists) {
				t.Fatalf("an id was reused: %v", err)
			}
		})
	}
}

func TestAFiringIsSignedGradedAndRecorded(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			name := lineage(t)
			declared, err := b.store.Draft(ctx, declaration(name+"@1"), "operator", "")
			if err != nil {
				t.Fatalf("draft: %v", err)
			}

			// Nobody named: refused, before the engine is asked anything.
			if _, err := b.store.Apply(ctx, declared.ID, rules.Application{Key: "operator", DryRun: true}); !errors.Is(err, rules.ErrUnsigned) {
				t.Fatalf("an unsigned firing was accepted: %v", err)
			}
			// A draft writes nothing, but may be read.
			if _, err := b.store.Apply(ctx, declared.ID, rules.Application{By: "liliang", Key: "operator"}); !errors.Is(err, rules.ErrState) {
				t.Fatalf("a draft fired for real: %v", err)
			}
			if _, err := b.store.Apply(ctx, declared.ID, rules.Application{By: "liliang", Key: "operator", DryRun: true}); err != nil {
				t.Fatalf("a draft could not be dry run: %v", err)
			}

			if _, _, err := b.store.Publish(ctx, declared.ID, rules.Signature{By: "liliang", Key: "operator"}); err != nil {
				t.Fatalf("publish: %v", err)
			}
			firing, err := b.store.Apply(ctx, declared.ID, rules.Application{
				By: "liliang", Key: "operator", Note: "after the march import", Document: "march",
			})
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if firing.Act == "" || firing.LedgerError != "" {
				t.Fatalf("the firing recorded nothing: %+v", firing)
			}
			if firing.By != "liliang" || firing.Actor != "operator" {
				t.Fatalf("the firing names %q under %q", firing.By, firing.Actor)
			}
			if len(firing.Edges) != 1 || len(firing.Edges[0].Supports) != 2 {
				t.Fatalf("the inference does not name its premises: %+v", firing.Edges)
			}

			// The contract travelled with the rule, so what it derived reaches
			// the brain graded and countable.
			meta := b.engine.last(t).Rules[0].Metadata
			for key, want := range map[string]string{
				cortexdb.KeyGrade:    cortexdb.GradeSelfConsistent,
				cortexdb.KeyProducer: cortexdb.ProducerCompiled,
				cortexdb.KeySource:   "athanor:rule:" + declared.ID,
				cortexdb.KeyBy:       "operator",
				"firing":             firing.Act,
			} {
				if meta[key] != want {
					t.Errorf("the derived edges carry %s = %q, want %q", key, meta[key], want)
				}
			}
			if _, err := time.Parse(time.RFC3339, meta[cortexdb.KeyAt]); err != nil {
				t.Errorf("%s is not RFC 3339: %q", cortexdb.KeyAt, meta[cortexdb.KeyAt])
			}
			if b.engine.last(t).DocumentID != "march" {
				t.Errorf("the scope did not reach the engine: %+v", b.engine.last(t))
			}

			// The firing is readable back, with what it did.
			back, err := b.store.Firings(ctx, declared.ID, 10)
			if err != nil {
				t.Fatalf("firings: %v", err)
			}
			if len(back) != 2 {
				t.Fatalf("firings = %d, want 2 (the dry run and the real one): %+v", len(back), back)
			}
			if back[0].Act != firing.Act || back[0].Candidates != 7 || len(back[0].Created) != 1 {
				t.Fatalf("the newest firing came back as %+v", back[0])
			}
			if !back[1].DryRun {
				t.Fatalf("the dry run was recorded as a real one: %+v", back[1])
			}

			// A retired rule does not fire again, and what it derived stays.
			if _, err := b.store.Retire(ctx, declared.ID, rules.Signature{By: "liliang", Key: "operator"}); err != nil {
				t.Fatalf("retire: %v", err)
			}
			if _, err := b.store.Apply(ctx, declared.ID, rules.Application{By: "liliang", Key: "operator"}); !errors.Is(err, rules.ErrState) {
				t.Fatalf("a retired rule fired: %v", err)
			}
			if again, err := b.store.Firings(ctx, declared.ID, 10); err != nil || len(again) != 2 {
				t.Fatalf("retiring lost the firings: %d (%v)", len(again), err)
			}
		})
	}
}

func TestAFiringSurvivesALedgerWriteThatFails(t *testing.T) {
	for _, b := range backends(t, rules.WithLedger(brokenLedger{})) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			name := lineage(t)
			declared, err := b.store.Draft(ctx, declaration(name+"@1"), "operator", "")
			if err != nil {
				t.Fatalf("draft: %v", err)
			}
			if _, _, err := b.store.Publish(ctx, declared.ID, rules.Signature{By: "liliang"}); err != nil {
				t.Fatalf("publish: %v", err)
			}
			firing, err := b.store.Apply(ctx, declared.ID, rules.Application{By: "liliang", Key: "operator"})
			if err != nil {
				t.Fatalf("a ledger failure undid the firing: %v", err)
			}
			if firing.LedgerError == "" {
				t.Fatalf("the firing says nothing about the ledger failure: %+v", firing)
			}
			// The act is in this package's own table regardless: the ledger is
			// the audit view and this is the record.
			acts, err := b.store.Acts(ctx, declared.ID)
			if err != nil {
				t.Fatalf("acts: %v", err)
			}
			var applied int
			for _, a := range acts {
				if a.Kind == rules.ActApply {
					applied++
				}
			}
			if applied != 1 {
				t.Fatalf("the firing is not in the record: %+v", acts)
			}
		})
	}
}

func TestADerivationThatHitsACapIsARefusalAndNotAResult(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			name := lineage(t)
			declared, err := b.store.Draft(ctx, declaration(name+"@1"), "operator", "")
			if err != nil {
				t.Fatalf("draft: %v", err)
			}
			if _, _, err := b.store.Publish(ctx, declared.ID, rules.Signature{By: "liliang"}); err != nil {
				t.Fatalf("publish: %v", err)
			}
			b.engine.err = fmt.Errorf("apply rules: %w", graph.ErrRuleCapExceeded)
			if _, err := b.store.Apply(ctx, declared.ID, rules.Application{By: "liliang", Key: "operator"}); !errors.Is(err, rules.ErrCapped) {
				t.Fatalf("a capped derivation was reported as a result: %v", err)
			}
			// Nothing was recorded: no edges were written, so there is no
			// firing to account for.
			if got, err := b.store.Firings(ctx, declared.ID, 10); err != nil || len(got) != 0 {
				t.Fatalf("a capped derivation recorded a firing: %d (%v)", len(got), err)
			}
		})
	}
}

func TestLineagesArePublishedIndependently(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			one, two := lineage(t), lineage(t)
			for _, id := range []string{one + "@1", two + "@1"} {
				if _, err := b.store.Draft(ctx, declaration(id), "operator", ""); err != nil {
					t.Fatalf("draft %s: %v", id, err)
				}
				if _, retired, err := b.store.Publish(ctx, id, rules.Signature{By: "liliang"}); err != nil || retired != "" {
					t.Fatalf("publish %s: retired %q (%v)", id, retired, err)
				}
			}
			for _, name := range []string{one, two} {
				got, err := b.store.Current(ctx, name)
				if err != nil || got.ID != name+"@1" {
					t.Fatalf("current of %s is %+v (%v)", name, got, err)
				}
				listed, err := b.store.List(ctx, name, "")
				if err != nil || len(listed) != 1 {
					t.Fatalf("list of %s is %+v (%v)", name, listed, err)
				}
			}
			if _, err := b.store.Current(ctx, lineage(t)); !errors.Is(err, rules.ErrNotFound) {
				t.Fatalf("a lineage nobody published has a current rule: %v", err)
			}
		})
	}
}
