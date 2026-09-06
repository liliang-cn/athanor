package ontologies_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/liliang-cn/alchemy/pkg/alchemy"
	"github.com/liliang-cn/athanor/internal/braintest"
	"github.com/liliang-cn/athanor/pkg/ontologies"
)

// seed is one lineage's first vocabulary. The lineage is a parameter because
// a PostgreSQL run shares one database across every test in the package, and
// two tests both starting from "sds@1" would be one test failing on the id the
// other already took.
func seed(lineage string) []byte {
	return []byte(`{"id":"` + lineage + `@1","parts":{"prose":{
  "entities":[{"name":"Node"},{"name":"DRBDResource"}],
  "relations":[{"name":"promotes","from":["Node"],"to":["DRBDResource"]}]}}}`)
}

// lineages are unique per run, for the same reason.
func newLineage(t *testing.T) string {
	t.Helper()
	return braintest.Unique("l")
}

// proposed is what a run under `sds@1` came back wanting: a type the corpus
// used and the vocabulary does not declare, and the relation it was used on.
// Entity first, which is the order Extend needs — a relation whose ends are
// themselves undeclared is refused, on purpose.
func proposed() []alchemy.Proposal {
	return []alchemy.Proposal{
		{Kind: alchemy.ProposalEntity, Type: "Cluster", Records: 4, Sources: []string{"runbook.md"}},
		{Kind: alchemy.ProposalRelation, Type: "member_of", Records: 6,
			From: []string{"Node"}, To: []string{"Cluster"}, Sources: []string{"runbook.md"}},
	}
}

// One suite, both backends. Which backends those are, and the loud complaint
// when the PostgreSQL half is not covered, are internal/braintest's — shared
// with pkg/livedb, which owns tables on the same handle and has the same
// reason to care.
type backend struct {
	name  string
	store *ontologies.Store
}

func backends(t *testing.T) []backend {
	t.Helper()
	var out []backend
	for _, b := range braintest.Backends(t) {
		s, err := ontologies.New(b.Open(t))
		if err != nil {
			t.Fatalf("ontologies.New on %s: %v", b.Name, err)
		}
		if got := s.Dialect().Kind(); got != b.Kind {
			t.Fatalf("the %s store speaks %s", b.Name, got)
		}
		out = append(out, backend{name: b.Name, store: s})
	}
	return out
}

func TestTheChainFromADraftToAPublishedExtension(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.store
			ln := newLineage(t)
			v1, v2 := ln+"@1", ln+"@2"

			v, err := s.Draft(ctx, seed(ln), "operator", "the first vocabulary")
			if err != nil {
				t.Fatalf("draft: %v", err)
			}
			if v.ID != v1 || v.State != ontologies.Draft || v.Lineage != ln {
				t.Fatalf("draft landed as %+v", v)
			}

			v, err = s.Propose(ctx, v1, "job-7", "prose", proposed(), "operator", "")
			if err != nil {
				t.Fatalf("propose: %v", err)
			}
			if v.State != ontologies.Proposed {
				t.Fatalf("a draft with proposals against it should be proposed, is %q", v.State)
			}
			back, err := s.Get(ctx, v1)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if len(back.Proposals) != 2 || back.ProposedFrom != "job-7" || back.Part != "prose" {
				t.Fatalf("the proposal did not survive the round trip: %+v", back)
			}

			if _, _, err := s.Publish(ctx, v1, ontologies.Publication{By: "liliang"}); err != nil {
				t.Fatalf("publish %s: %v", v1, err)
			}

			next, err := s.Approve(ctx, v1, ontologies.Approval{
				Accept: []string{"Cluster", "member_of"}, By: "liliang", Note: "both are real",
			})
			if err != nil {
				t.Fatalf("approve: %v", err)
			}
			if next.ID != v2 || next.Parent != v1 || next.State != ontologies.Approved {
				t.Fatalf("the extension landed as %+v", next)
			}
			if next.ApprovedBy != "liliang" || next.ApprovedAt == nil {
				t.Fatalf("the approval is unsigned: %+v", next)
			}
			if !strings.Contains(string(next.Document), "Cluster") || !strings.Contains(string(next.Document), "member_of") {
				t.Fatalf("the accepted types are not in the new document: %s", next.Document)
			}
			// Approving does not publish: the first version is still what a
			// client gets.
			if cur, err := s.Current(ctx, ln); err != nil || cur.ID != v1 {
				t.Fatalf("approving changed what is current: %v %+v", err, cur)
			}

			published, retired, err := s.Publish(ctx, v2, ontologies.Publication{By: "liliang", Note: "in force"})
			if err != nil {
				t.Fatalf("publish %s: %v", v2, err)
			}
			if retired != v1 {
				t.Fatalf("publishing %s retired %q, want %s", v2, retired, v1)
			}
			if published.State != ontologies.Published || published.PublishedAt == nil {
				t.Fatalf("published as %+v", published)
			}
			old, err := s.Get(ctx, v1)
			if err != nil || old.State != ontologies.Retired || old.RetiredAt == nil {
				t.Fatalf("%s was not retired: %v %+v", v1, err, old)
			}
			// The version handed back by Publish and the row a later Get
			// reads are the same version, timestamps included. They are not
			// automatically: this store's clock is time.Now, which is
			// nanoseconds, and PostgreSQL's TIMESTAMPTZ is microseconds — so
			// without the truncation in Store.at the two would agree on
			// SQLite and disagree on PostgreSQL, on the one field an auditor
			// asking "when was this in force" reads.
			back, err2 := s.Get(ctx, v2)
			if err2 != nil {
				t.Fatalf("get %s: %v", v2, err2)
			}
			if !back.CreatedAt.Equal(published.CreatedAt) || !back.PublishedAt.Equal(*published.PublishedAt) {
				t.Fatalf("the timestamps did not round trip: %v/%v vs %v/%v",
					back.CreatedAt, back.PublishedAt, published.CreatedAt, published.PublishedAt)
			}
			// Retired is not deleted. Every graph extracted under it names it
			// in its provenance, and its body still answers what those facts
			// were checked against.
			if !json.Valid(old.Document) || strings.Contains(string(old.Document), "Cluster") {
				t.Fatalf("the retired body changed: %s", old.Document)
			}

			cur, err := s.Current(ctx, ln)
			if err != nil {
				t.Fatalf("current: %v", err)
			}
			if cur.ID != v2 || !strings.Contains(string(cur.Document), "Cluster") {
				t.Fatalf("current is %s: %s", cur.ID, cur.Document)
			}

			// Every decision is on the record, in order.
			acts, err := s.Acts(ctx, v1)
			more, err2 := s.Acts(ctx, v2)
			if err2 != nil {
				t.Fatalf("acts: %v", err2)
			}
			acts = append(acts, more...)
			if err != nil {
				t.Fatalf("acts: %v", err)
			}
			var kinds []string
			for _, a := range acts {
				if a.Actor == "" || a.Subject == "" || a.At.IsZero() {
					t.Fatalf("an act with no actor, subject or time: %+v", a)
				}
				kinds = append(kinds, a.Kind)
			}
			for _, want := range []string{ontologies.ActDraft, ontologies.ActPropose, ontologies.ActApprove, ontologies.ActPublish, ontologies.ActRetire} {
				if !contains(kinds, want) {
					t.Fatalf("no %s act was recorded; got %v", want, kinds)
				}
			}
		})
	}
}

func TestTwoLineagesPublishIndependently(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.store
			one, two := newLineage(t), newLineage(t)
			codeDoc := []byte(`{"id":"` + two + `@1","parts":{"code":{"entities":[{"name":"File"}]}}}`)

			if _, err := s.Draft(ctx, seed(one), "operator", ""); err != nil {
				t.Fatalf("draft %s: %v", one, err)
			}
			if _, err := s.Draft(ctx, codeDoc, "operator", ""); err != nil {
				t.Fatalf("draft %s: %v", two, err)
			}
			if _, retired, err := s.Publish(ctx, one+"@1", ontologies.Publication{By: "liliang"}); err != nil || retired != "" {
				t.Fatalf("publish %s@1: %v (retired %q)", one, err, retired)
			}
			if _, retired, err := s.Publish(ctx, two+"@1", ontologies.Publication{By: "liliang"}); err != nil || retired != "" {
				t.Fatalf("publishing %s@1 touched another lineage: %v (retired %q)", two, err, retired)
			}
			for _, ln := range []string{one, two} {
				cur, err := s.Current(ctx, ln)
				if err != nil || cur.ID != ln+"@1" {
					t.Fatalf("current of %s is %+v (%v)", ln, cur, err)
				}
			}
			// Retiring one lineage's version leaves the other's alone.
			ext := []byte(`{"id":"` + two + `@2","parts":{"code":{"entities":[{"name":"File"},{"name":"Package"}]}}}`)
			if _, err := s.Draft(ctx, ext, "operator", ""); err != nil {
				t.Fatalf("draft %s@2: %v", two, err)
			}
			if _, retired, err := s.Publish(ctx, two+"@2", ontologies.Publication{By: "liliang"}); err != nil || retired != two+"@1" {
				t.Fatalf("publish %s@2: %v (retired %q)", two, err, retired)
			}
			if cur, err := s.Current(ctx, one); err != nil || cur.ID != one+"@1" {
				t.Fatalf("the other lineage moved: %+v (%v)", cur, err)
			}
		})
	}
}

func TestTheRefusals(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.store
			ln := newLineage(t)
			v1 := ln + "@1"

			if _, err := s.Draft(ctx, []byte(`{"id":"`+ln+`","parts":{"prose":{"entities":[{"name":"Node"}]}}}`), "operator", ""); !errors.Is(err, ontologies.ErrInvalid) {
				t.Fatalf("an unversioned id was accepted: %v", err)
			}
			if _, err := s.Draft(ctx, seed(ln), "operator", ""); err != nil {
				t.Fatalf("draft: %v", err)
			}
			if _, err := s.Draft(ctx, seed(ln), "operator", ""); !errors.Is(err, ontologies.ErrExists) {
				t.Fatalf("one id held two vocabularies: %v", err)
			}
			if _, err := s.Get(ctx, ln+"@99"); !errors.Is(err, ontologies.ErrNotFound) {
				t.Fatalf("an unknown version was found: %v", err)
			}
			if _, err := s.Current(ctx, ln); !errors.Is(err, ontologies.ErrNotFound) {
				t.Fatalf("a lineage with nothing published has a current version: %v", err)
			}
			// Nothing proposed yet.
			if _, err := s.Approve(ctx, v1, ontologies.Approval{Accept: []string{"Cluster"}, By: "liliang"}); !errors.Is(err, ontologies.ErrState) {
				t.Fatalf("approved with nothing proposed: %v", err)
			}
			if _, err := s.Propose(ctx, v1, "job-7", "prose", nil, "operator", ""); !errors.Is(err, ontologies.ErrInvalid) {
				t.Fatalf("an empty proposal was recorded: %v", err)
			}
			if _, err := s.Propose(ctx, v1, "job-7", "prose", proposed(), "operator", ""); err != nil {
				t.Fatalf("propose: %v", err)
			}
			if _, err := s.Approve(ctx, v1, ontologies.Approval{Accept: []string{"Cluster"}, By: "   "}); !errors.Is(err, ontologies.ErrUnsigned) {
				t.Fatalf("an unsigned approval was accepted: %v", err)
			}
			if _, err := s.Approve(ctx, v1, ontologies.Approval{Accept: nil, By: "liliang"}); !errors.Is(err, ontologies.ErrInvalid) {
				t.Fatalf("an approval accepting nothing was accepted: %v", err)
			}
			if _, err := s.Approve(ctx, v1, ontologies.Approval{Accept: []string{"Rack"}, By: "liliang"}); !errors.Is(err, ontologies.ErrInvalid) {
				t.Fatalf("a type nobody proposed was accepted: %v", err)
			}
			// Accepting only the relation, whose end type is still undeclared,
			// is Extend's own refusal and it is passed through.
			if _, err := s.Approve(ctx, v1, ontologies.Approval{Accept: []string{"member_of"}, By: "liliang"}); err == nil {
				t.Fatal("a relation was declared against an undeclared end")
			}
			if _, _, err := s.Publish(ctx, v1, ontologies.Publication{By: ""}); !errors.Is(err, ontologies.ErrUnsigned) {
				t.Fatalf("an unsigned publication was accepted: %v", err)
			}
			if _, _, err := s.Publish(ctx, v1, ontologies.Publication{By: "liliang"}); err != nil {
				t.Fatalf("publish: %v", err)
			}
			if _, _, err := s.Publish(ctx, v1, ontologies.Publication{By: "liliang"}); !errors.Is(err, ontologies.ErrState) {
				t.Fatalf("the current version was published twice: %v", err)
			}
		})
	}
}

func contains(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}
