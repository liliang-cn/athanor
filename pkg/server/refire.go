package server

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/liliang-cn/athanor/pkg/rules"
)

// What a load does to everything derived from the load before it.
//
// A rule's conclusions are edges between the entities its premises joined, and
// those entities belong to a load. Replace that load — the ordinary way to
// correct a corpus — and the entities go, and every edge on them goes with
// them, including the ones no document ever stated. The graph comes back
// smaller in a way the load report cannot see: sink.Load counts what it wrote
// and knows nothing about what a rule had added on top of it.
//
// It happened twice in one afternoon on a twenty-one person graph, and both
// times the missing facts were the ones a person would notice least — two
// officers whose employment nobody had written down in prose, and eight people
// whose city came from their team's. Nothing reported it. The brain simply
// knew ten fewer things than it had an hour earlier, and the only reason it
// was caught is that somebody happened to count.
//
// So a load re-fires the rules that are in force. Not as a convenience: a
// store whose derived facts survive a reload only when an operator remembers
// to re-run five commands is a store that is quietly wrong most of the time,
// and "quietly" is the part this product exists to refuse.
//
// Three things this deliberately does not do:
//
//   - It does not fire drafts or retired rules. Published means in force, and
//     in force is what a reload has to restore. A draft fires only as a dry run
//     and only when somebody asks.
//   - It does not stop the load. The graph is in the brain by the time this
//     runs, and a rule that fails to fire is reported beside the load report
//     rather than rolled back over — the same rule loads.go states about its
//     ledger write, for the same reason.
//   - It does not sign as a person. Each firing is recorded with the key's own
//     id and a note saying which load caused it, because nobody decided
//     anything here: the decision was made when the rule was published, and
//     crediting the operator who happened to run a load with it would put a
//     name on a judgement they did not make.

// refiring is what happened to one rule.
type refiring struct {
	Rule    string `json:"rule"`
	Derived int    `json:"derived"`
	Act     string `json:"act,omitempty"`
	Error   string `json:"error,omitempty"`
}

// refirePublished re-runs every rule in force and reports what each derived.
//
// The caller is a load that has already succeeded, so this reports and never
// fails. A store with no rules at all — which is most stores — does nothing
// and says nothing, and the answer carries no "rules" key.
func (s *Server) refirePublished(ctx context.Context, actor, because string) []refiring {
	store, err := s.rules()
	if err != nil {
		// No rule store is not an error here. It is a deployment that has
		// never declared a rule, and a load in it has nothing to restore.
		log.Printf("athanor: rules: after %s: %v", because, err)
		return nil
	}
	declared, err := store.List(ctx, "", rules.Published)
	if err != nil {
		log.Printf("athanor: rules: list after %s: %v", because, err)
		return nil
	}
	if len(declared) == 0 {
		return nil
	}
	out := make([]refiring, 0, len(declared))
	for _, rule := range declared {
		rec := refiring{Rule: rule.ID}
		firing, err := store.Apply(ctx, rule.ID, rules.Application{
			By:  actor,
			Key: actor,
			// The note is what a reader of the ledger needs: this firing is
			// not somebody deciding, it is a load putting back what the load
			// took away.
			Note: "re-fired after " + because,
			// The rule's own conclusions from before this load are gone with
			// the entities they were drawn between. Deleting first is what
			// makes re-firing converge rather than accumulate on a store where
			// only half the graph was replaced.
			DeleteExisting: true,
		})
		if err != nil {
			rec.Error = err.Error()
			out = append(out, rec)
			continue
		}
		rec.Act, rec.Derived = firing.Act, len(firing.Edges)
		if firing.LedgerError != "" {
			log.Printf("athanor: ledger: firing %s of rule %s: %s", firing.Act, rule.ID, firing.LedgerError)
		}
		out = append(out, rec)
	}
	return out
}

// derivedCount totals what a round of re-firing put back, for the load's own
// answer and for its ledger entry.
func derivedCount(fired []refiring) int {
	n := 0
	for _, f := range fired {
		n += f.Derived
	}
	return n
}

// refireErrors names the rules that did not fire, so the load's answer can say
// so in one line instead of the caller having to walk the list.
func refireErrors(fired []refiring) string {
	var bad []string
	for _, f := range fired {
		if f.Error != "" {
			bad = append(bad, f.Rule)
		}
	}
	if len(bad) == 0 {
		return ""
	}
	return fmt.Sprintf("%d rule(s) did not fire: %s", len(bad), strings.Join(bad, ", "))
}
