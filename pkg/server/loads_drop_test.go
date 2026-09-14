package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A load could be written and never listed and never removed.
//
// The only thing that could be said about a load already in the brain was to
// put another graph over it, which answers "this one is wrong" with "here is a
// different one" — and there is not always a different one. These are the two
// routes that were missing, and the signature the second of them requires.

func loadJobAs(t *testing.T, h *harness, name string) {
	t.Helper()
	job := uploadAndCreate(t, h)
	resp, err := h.http(http.MethodPost, "/athanor/loads", "op-secret",
		`{"job":"`+job+`","load":"`+name+`"}`)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("load %s answered %d: %s", name, resp.StatusCode, body)
	}
}

func TestTheCatalogueNamesWhatTheBrainHolds(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	loadJobAs(t, h, "first")
	loadJobAs(t, h, "second")

	resp, err := h.http(http.MethodGet, "/athanor/loads", "op-secret", "")
	if err != nil {
		t.Fatalf("GET loads: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /athanor/loads = %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Count int `json:"count"`
		Loads []struct {
			ID     string `json:"id"`
			Digest string `json:"digest"`
		} `json:"loads"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v — %s", err, body)
	}
	if out.Count != 2 {
		t.Fatalf("the catalogue reports %d loads, want 2: %s", out.Count, body)
	}
	for _, l := range out.Loads {
		if l.Digest == "" {
			t.Errorf("load %q carries no digest, so nothing says which graph it holds", l.ID)
		}
	}
}

func TestDroppingALoadIsSignedAndRecorded(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	loadJobAs(t, h, "wrong")

	before, err := h.srv.db.ContractTally(context.Background())
	if err != nil {
		t.Fatalf("tally: %v", err)
	}

	// Unsigned is refused, and so is unexplained: what a drop takes is not
	// recoverable from anything else here, so the entry is the only place the
	// answer survives.
	for _, body := range []string{
		`{"why":"loaded under the wrong vocabulary"}`,
		`{"by":"liliang"}`,
	} {
		resp, _ := h.http(http.MethodDelete, "/athanor/loads/wrong", "op-secret", body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("drop with %s = %d, want 400", body, resp.StatusCode)
		}
	}
	resp, _ := h.http(http.MethodDelete, "/athanor/loads/wrong", "ro-secret",
		`{"by":"liliang","why":"x"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a read-only key dropped a load: %d", resp.StatusCode)
	}

	resp, err = h.http(http.MethodDelete, "/athanor/loads/wrong", "op-secret",
		`{"by":"liliang","why":"loaded under the wrong vocabulary"}`)
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drop = %d: %s", resp.StatusCode, body)
	}
	var out struct{ Load, By, Decision string }
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v — %s", err, body)
	}
	if out.Decision == "" {
		t.Error("the drop wrote no ledger entry; a removal that leaves no record is the one act that has to")
	}
	if out.By != "liliang" {
		t.Errorf("by = %q, want the name that signed it", out.By)
	}

	after, err := h.srv.db.ContractTally(context.Background())
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if after.Asserted.Nodes+after.Asserted.Edges >= before.Asserted.Nodes+before.Asserted.Edges {
		t.Errorf("the brain holds as much as before: the drop took nothing (%+v then %+v)", before, after)
	}

	// The entry outlives the records, which is the whole reason it is written.
	chain, err := h.srv.db.DecisionChain(context.Background(), out.Decision, 1)
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	if len(chain.Decisions) == 0 || !strings.Contains(chain.Decisions[0].Note, "wrong vocabulary") {
		t.Errorf("the ledger entry does not carry the reason: %+v", chain)
	}

	// Dropping it again is a refusal, not a second success.
	resp, _ = h.http(http.MethodDelete, "/athanor/loads/wrong", "op-secret",
		`{"by":"liliang","why":"again"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("dropping a load that is not there = %d, want 404", resp.StatusCode)
	}
}
