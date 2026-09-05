package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/liliang-cn/alchemy/pkg/alchemy"
	"github.com/liliang-cn/alchemy/pkg/service"
)

// Two keys: one that may change things and one that may only look. Every
// test that cares about policy uses these, so a denial in one place and an
// allowance in another are about the same two people.
const keysJSON = `{"keys":[
  {"id":"operator","secret":"op-secret","clearance":"read-write"},
  {"id":"reader","secret":"ro-secret","clearance":"read-only"}
]}`

// fakeRunner is a pipeline that returns what it is told to, so the assembly
// is tested without a model, a corpus or a network. What it returns is
// Result-shaped enough for the CortexDB connector to load: entities and
// relations with provenance, which is what the contract grades.
type fakeRunner struct {
	result alchemy.Result
	err    error
}

func (f fakeRunner) Run(_ context.Context, jobID string, _ service.JobSpec, _ chan<- service.Event, _ service.Inbox) (alchemy.Result, error) {
	if f.err != nil {
		return alchemy.Result{}, f.err
	}
	r := f.result
	r.Job = jobID
	return r, nil
}

type harness struct {
	t        *testing.T
	srv      *Server
	grpcAddr string
	httpAddr string
	conn     *grpc.ClientConn
}

func newHarness(t *testing.T, run service.Runner) *harness {
	t.Helper()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(keyFile, []byte(keysJSON), 0o600); err != nil {
		t.Fatalf("keys: %v", err)
	}
	return newHarnessWith(t, run, keyFile, dir)
}

// newHarnessWith is newHarness with the policy named rather than assumed, for
// the tests that need a third key — a confined one — and would otherwise have
// to copy the whole of this.
func newHarnessWith(t *testing.T, run service.Runner, keyFile, dir string) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	srv, err := New(ctx, Options{
		DBPath:   filepath.Join(dir, "brain.db"),
		GRPCAddr: "127.0.0.1:0",
		HTTPAddr: "127.0.0.1:0",
		KeyFile:  keyFile,
		Spool:    dir,
		Runner:   run,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		_ = srv.Close()
	})
	g, h := srv.Addrs()

	conn, err := grpc.NewClient(g, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &harness{t: t, srv: srv, grpcAddr: g, httpAddr: h, conn: conn}
}

func asKey(secret string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+secret)
}

func (h *harness) http(method, path, bearer, body string) (*http.Response, error) {
	req, err := http.NewRequest(method, "http://"+h.httpAddr+path, stringsReader(body))
	if err != nil {
		return nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}
