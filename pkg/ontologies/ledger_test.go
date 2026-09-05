package ontologies_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/liliang-cn/athanor/pkg/ontologies"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// recorder is a Ledger with nothing behind it — which is the point of the
// interface. What a mirror does with an act is Athanor's business; what this
// package owes is that every act it commits is offered, once, after it has
// committed, with the key that called beside the name it was signed with.
type recorder struct {
	acts []ontologies.Act
	err  error
}

func (r *recorder) Record(_ context.Context, act ontologies.Act) error {
	r.acts = append(r.acts, act)
	return r.err
}

func storeWithLedger(t *testing.T, l ontologies.Ledger) *ontologies.Store {
	t.Helper()
	cfg := cortexdb.DefaultConfig(filepath.Join(t.TempDir(), "brain.db"))
	cfg.Dimensions = 4
	db, err := cortexdb.Open(cfg)
	if err != nil {
		t.Fatalf("open brain: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := ontologies.New(db, ontologies.WithLedger(l))
	if err != nil {
		t.Fatalf("ontologies.New: %v", err)
	}
	return s
}

func TestEveryActIsOfferedToTheLedger(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	s := storeWithLedger(t, rec)
	ln := newLineage(t)
	v1 := ln + "@1"

	if _, err := s.Draft(ctx, seed(ln), "operator", "the first one"); err != nil {
		t.Fatalf("draft: %v", err)
	}
	if _, err := s.Propose(ctx, v1, "job-7", "prose", proposed(), "operator", ""); err != nil {
		t.Fatalf("propose: %v", err)
	}
	if _, err := s.Approve(ctx, v1, ontologies.Approval{
		Accept: []string{"Cluster"}, By: "liliang", Key: "operator",
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, _, err := s.Publish(ctx, v1, ontologies.Publication{By: "liliang", Key: "operator"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, _, err := s.Publish(ctx, ln+"@2", ontologies.Publication{By: "liliang", Key: "operator"}); err != nil {
		t.Fatalf("publish the extension: %v", err)
	}

	want := []string{
		ontologies.ActDraft, ontologies.ActPropose, ontologies.ActApprove,
		ontologies.ActPublish, ontologies.ActRetire, ontologies.ActPublish,
	}
	if len(rec.acts) != len(want) {
		t.Fatalf("the ledger was offered %d acts, want %d: %+v", len(rec.acts), len(want), rec.acts)
	}
	for i, kind := range want {
		if rec.acts[i].Kind != kind {
			t.Errorf("act %d is %q, want %q", i, rec.acts[i].Kind, kind)
		}
		if rec.acts[i].ID == "" || rec.acts[i].Subject == "" || rec.acts[i].At.IsZero() {
			t.Errorf("act %d was offered before it was written: %+v", i, rec.acts[i])
		}
		if rec.acts[i].Key != "operator" {
			t.Errorf("act %d carries key %q, want the id of the key that called", i, rec.acts[i].Key)
		}
	}
	// The approval is the one act signed with a name the door did not check,
	// and both survive.
	approve := rec.acts[2]
	if approve.Actor != "liliang" || approve.Key != "operator" {
		t.Fatalf("the approval lost one of its two names: %+v", approve)
	}
}

func TestALedgerThatRefusesDoesNotUndoTheWorkflow(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{err: errors.New("the ledger is unavailable")}
	s := storeWithLedger(t, rec)
	ln := newLineage(t)

	v, err := s.Draft(ctx, seed(ln), "operator", "")
	if err != nil {
		t.Fatalf("a ledger failure undid the draft: %v", err)
	}
	if _, _, err := s.Publish(ctx, v.ID, ontologies.Publication{By: "liliang"}); err != nil {
		t.Fatalf("a ledger failure undid the publication: %v", err)
	}
	// The table is the source of truth and still holds both acts.
	acts, err := s.Acts(ctx, "")
	if err != nil {
		t.Fatalf("acts: %v", err)
	}
	if len(acts) != 2 {
		t.Fatalf("the workflow's own record is %d acts, want 2: %+v", len(acts), acts)
	}
	if cur, err := s.Current(ctx, ln); err != nil || cur.ID != v.ID {
		t.Fatalf("the published version is %+v (%v)", cur, err)
	}
}
