package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/liliang-cn/alchemy/pkg/alchemy"
	alchemyv1 "github.com/liliang-cn/alchemy/proto/alchemy/v1"
)

const testOntology = `{"id":"sds-demo@1","parts":{"prose":{
  "entities":[{"name":"Node"},{"name":"DRBDResource"}],
  "relations":[{"name":"promotes","from":["Node"],"to":["DRBDResource"],"at_most_one_in":true}]}}}`

// cannedResult is what the fake pipeline "extracted": two entities and the
// edge between them, each carrying provenance, which is what the connector
// grades. Nobody reviewed it, so every record should land as `asserted`.
func cannedResult() alchemy.Result {
	prov := alchemy.Provenance{Source: "runbook.md", Chunk: 0, Producer: alchemy.ProducerLLMExtract, Ontology: "sds-demo@1", Model: "fake"}
	return alchemy.Result{
		Entities: []alchemy.Entity{
			{ID: "node:hp", Type: "Node", Name: "hp", Provenance: prov},
			{ID: "drbdresource:sds-meta", Type: "DRBDResource", Name: "sds-meta", Provenance: prov},
		},
		Relations: []alchemy.Relation{
			{From: "node:hp", To: "drbdresource:sds-meta", Type: "promotes", Provenance: prov},
		},
	}
}

// uploadAndCreate drives the front door the way a client would: upload one
// document, create a job under an ontology, wait for the pipeline to answer.
func uploadAndCreate(t *testing.T, h *harness) string {
	t.Helper()
	c := alchemyv1.NewAlchemyClient(h.conn)
	up, err := c.UploadSource(asKey("op-secret"))
	if err != nil {
		t.Fatalf("upload open: %v", err)
	}
	if err := up.Send(&alchemyv1.SourceChunk{
		Name: "runbook.md", Kind: alchemyv1.SourceKind_SOURCE_KIND_DOCUMENT,
		MediaType: "text/markdown", Data: []byte("# runbook\n\nsds-meta is promoted on hp.\n"),
	}); err != nil {
		t.Fatalf("upload send: %v", err)
	}
	src, err := up.CloseAndRecv()
	if err != nil {
		t.Fatalf("upload close: %v", err)
	}
	job, err := c.CreateJob(asKey("op-secret"), &alchemyv1.CreateJobRequest{
		SourceIds: []string{src.GetId()}, Ontology: testOntology, Part: "prose",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		j, err := c.GetJob(asKey("op-secret"), &alchemyv1.GetJobRequest{JobId: job.GetId()})
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		switch j.GetState() {
		case alchemyv1.JobState_JOB_STATE_SUCCEEDED, alchemyv1.JobState_JOB_STATE_NEEDS_REVIEW, alchemyv1.JobState_JOB_STATE_FAILED:
			if j.GetState() == alchemyv1.JobState_JOB_STATE_FAILED {
				t.Fatalf("job failed: %s", j.GetError())
			}
			return job.GetId()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("job never settled")
	return ""
}

func TestAFinishedJobLoadsIntoTheBrainWithItsGrades(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	jobID := uploadAndCreate(t, h)

	resp, err := h.http(http.MethodPost, "/athanor/loads", "op-secret", `{"job":"`+jobID+`"}`)
	if err != nil {
		t.Fatalf("loads: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loads answered %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Job, Load, By string
	}
	if err := json.Unmarshal(body, &out); err != nil || out.By != "operator" || out.Load != jobID {
		t.Fatalf("load answer: %s (%v)", body, err)
	}

	// The proof is in the brain, read back through CortexDB's own contract
	// query: a model said it, nobody checked, so it is asserted.
	tally, err := h.srv.db.ContractTally(context.Background())
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if got := tally.Asserted.Nodes + tally.Asserted.Edges; got != 3 {
		t.Fatalf("asserted records = %d, want 2 entities + 1 edge; tally %+v", got, tally)
	}
	if tally.Untagged.Nodes+tally.Untagged.Edges != 0 {
		t.Fatalf("records arrived without a contract: %+v", tally)
	}
}

func TestAReadOnlyKeyCannotLoadAndNoKeyCannotEither(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	jobID := uploadAndCreate(t, h)

	resp, _ := h.http(http.MethodPost, "/athanor/loads", "ro-secret", `{"job":"`+jobID+`"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a reader loaded a graph into the brain: %d", resp.StatusCode)
	}
	resp, _ = h.http(http.MethodPost, "/athanor/loads", "", `{"job":"`+jobID+`"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key loaded a graph: %d", resp.StatusCode)
	}
}

func TestAHeldJobCannotBeLoadedUntilSomeoneAnswers(t *testing.T) {
	// The service decides NEEDS_REVIEW from the result, not from an error:
	// a pipeline that found a conflict returns the graph with the conflict
	// on it, and len(res.Held()) > 0 is the hold (pkg/service/run.go).
	held := cannedResult()
	held.Conflicts = []alchemy.Conflict{{
		Kind: alchemy.ConflictCardinality, Subject: "drbdresource:sds-meta",
		Detail: "two nodes claim to promote it",
	}}
	h := newHarness(t, fakeRunner{result: held})
	jobID := uploadAndCreate(t, h)

	resp, _ := h.http(http.MethodPost, "/athanor/loads", "op-secret", `{"job":"`+jobID+`"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a held job was loaded: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(strings.ToLower(string(body)), "held") && !strings.Contains(strings.ToLower(string(body)), "review") {
		t.Fatalf("the refusal should say the job is held: %s", body)
	}
	tally, _ := h.srv.db.ContractTally(context.Background())
	if tally.Asserted.Nodes+tally.Asserted.Edges+tally.Verified.Edges != 0 {
		t.Fatalf("a held graph leaked into the brain: %+v", tally)
	}
}

func TestAnUnknownJobIsNotFoundAndATypoInTheBodyIsRefused(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	resp, _ := h.http(http.MethodPost, "/athanor/loads", "op-secret", `{"job":"never"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown job: %d", resp.StatusCode)
	}
	resp, _ = h.http(http.MethodPost, "/athanor/loads", "op-secret", `{"jobb":"x"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a misspelled field was accepted: %d", resp.StatusCode)
	}
}
