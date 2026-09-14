package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// The correction a reviewer was told to make, made.
//
// A decision on a delivered job is refused with a sentence naming the way out:
// "a record in a delivered graph is corrected by asserting the correction and
// naming what it retires — POST /v1/assertions with supersedes". That route
// answers a Result, and nothing could put a Result in the brain: /athanor/loads
// takes a job id, an assertion is not a job, and the road ended at the last
// step. This walks the whole of it — load a graph, assert a correction naming
// one of its records, and check that the correction is in the brain and the
// record it retires is marked as retired rather than deleted.
func TestAnAssertionReachesTheBrainAndRetiresWhatItNames(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	ctx := context.Background()
	jobID := uploadAndCreate(t, h)
	if resp, err := h.http(http.MethodPost, "/athanor/loads", "op-secret",
		`{"job":"`+jobID+`","load":"runbook"}`); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("load: %v %v", err, resp)
	}

	// The record the correction is about, as the brain spells it.
	retires := findNode(t, h, "hp")
	if retires == "" {
		t.Fatal("the loaded graph has no node for hp, so there is nothing to retire")
	}

	body := `{
	  "by": "liliang",
	  "note": "hp was decommissioned",
	  "entities": [{"id": "node:hp2", "type": "Node", "name": "hp2"}],
	  "supersedes": [{"retires": "` + retires + `", "reason": "decommissioned in March"}]
	}`
	resp, err := h.http(http.MethodPost, "/athanor/assertions", "op-secret", body)
	if err != nil {
		t.Fatalf("assert: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("assert = %d: %s", resp.StatusCode, raw)
	}
	var answer struct {
		Load    string `json:"load"`
		By      string `json:"by"`
		Report  struct{ Entities int }
		Retired []struct {
			Retires string `json:"retires"`
			Found   bool   `json:"found"`
			Kind    string `json:"kind"`
			Error   string `json:"error"`
		} `json:"retired"`
		Decision    string `json:"decision"`
		LedgerError string `json:"ledger_error"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("answer: %v (%s)", err, raw)
	}
	if answer.Report.Entities != 1 {
		t.Errorf("the assertion put %d entities in the brain, want 1: %s", answer.Report.Entities, raw)
	}
	if len(answer.Retired) != 1 || !answer.Retired[0].Found {
		t.Fatalf("the named record was not found, so nothing was retired: %s", raw)
	}
	if answer.Retired[0].Kind != "node" || answer.Retired[0].Error != "" {
		t.Errorf("retirement went wrong: %+v", answer.Retired[0])
	}
	if answer.LedgerError != "" || answer.Decision == "" {
		t.Errorf("the act left no ledger entry: %s", raw)
	}

	// Marked, not deleted. A reader meeting the old fact has to meet the
	// retirement on it; a reader meeting nothing cannot tell a retired fact
	// from one nobody ever wrote.
	node, err := h.srv.db.Graph().GetNode(ctx, retires)
	if err != nil || node == nil {
		t.Fatalf("the retired record is gone from the brain: %v", err)
	}
	props := node.Properties
	if got := str(props[cortexdb.KeyState]); got != "superseded" {
		t.Errorf("_state = %q, want superseded", got)
	}
	if got := str(props[cortexdb.KeyWhy]); got != "decommissioned in March" {
		t.Errorf("_why = %q, want the reason that was given", got)
	}
	if got := str(props[cortexdb.KeyBy]); got != "liliang" {
		t.Errorf("_by = %q, want whoever asserted the correction", got)
	}
	if str(props[cortexdb.KeyAt]) == "" {
		t.Error("_at is empty, so nothing says when the record was retired")
	}

	// And the new fact is really there.
	if findNode(t, h, "hp2") == "" {
		t.Error("the asserted entity is not in the brain")
	}
}

// A supersedes naming something the brain does not hold is reported, not
// swallowed. The assertion still lands; what must not happen is a 200 that
// leaves somebody believing the old answer was marked.
func TestAnAssertionSaysWhenTheRecordItRetiresIsNotThere(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	body := `{
	  "by": "liliang",
	  "entities": [{"id": "node:hp3", "type": "Node", "name": "hp3"}],
	  "supersedes": [{"retires": "entity:alchemy:nowhere:node:ghost", "reason": "never existed"}]
	}`
	resp, err := h.http(http.MethodPost, "/athanor/assertions", "op-secret", body)
	if err != nil {
		t.Fatalf("assert: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("assert = %d: %s", resp.StatusCode, raw)
	}
	var answer struct {
		Retired []struct {
			Found bool `json:"found"`
		} `json:"retired"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("answer: %v (%s)", err, raw)
	}
	if len(answer.Retired) != 1 || answer.Retired[0].Found {
		t.Errorf("a record the brain does not hold came back as retired: %s", raw)
	}
	if findNode(t, h, "hp3") == "" {
		t.Error("the assertion did not land, and it is not the retirement's job to stop it")
	}
}

// An assertion nobody signed is refused before the pipeline is called at all.
func TestAnUnsignedAssertionIsRefused(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	resp, err := h.http(http.MethodPost, "/athanor/assertions", "op-secret",
		`{"entities":[{"id":"node:x","type":"Node","name":"x"}]}`)
	if err != nil {
		t.Fatalf("assert: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// findNode is the brain's id for an entity whose name ends in what was asked
// for. The connector namespaces ids by load, so a test that hard-coded one
// would be testing the namespace rather than the road.
func findNode(t *testing.T, h *harness, name string) string {
	t.Helper()
	nodes, err := h.srv.db.Graph().GetAllNodes(context.Background(), nil)
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}
	for _, n := range nodes {
		if strings.HasSuffix(n.ID, ":"+name) {
			return n.ID
		}
	}
	return ""
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
