package server

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	alchemyv1 "github.com/liliang-cn/alchemy/proto/alchemy/v1"
	rpcv1 "github.com/liliang-cn/cortexdb/v2/pkg/rpc/v1"
)

func TestAReadOnlyKeyMayLookAtThePipelineButNotChangeIt(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	c := alchemyv1.NewAlchemyClient(h.conn)

	_, err := c.CreateJob(asKey("ro-secret"), &alchemyv1.CreateJobRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a read-only key created a job: %v", err)
	}
	// GetJob on a job that does not exist: the policy lets the reader through
	// and alchemy answers for itself. NotFound is the proof that both halves
	// ran — a denial would have come back as PermissionDenied.
	_, err = c.GetJob(asKey("ro-secret"), &alchemyv1.GetJobRequest{JobId: "never"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("a read-only key was not let through to GetJob: %v", err)
	}
}

func TestNoKeyIsRefusedBeforeReachingEitherService(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	if _, err := alchemyv1.NewAlchemyClient(h.conn).GetJob(context.Background(), &alchemyv1.GetJobRequest{JobId: "x"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("pipeline without a key: %v", err)
	}
	if _, err := rpcv1.NewAdminServiceClient(h.conn).Health(context.Background(), &rpcv1.HealthRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("brain without a key: %v", err)
	}
}

func TestTheBrainKeepsItsOwnPolicyOnTheSharedListener(t *testing.T) {
	// cortexdb.v1 calls go to CortexDB's interceptor unchanged. A read-only key
	// is refused a write there by CortexDB's rule, with CortexDB's wording —
	// proof the call was routed, not re-implemented.
	h := newHarness(t, fakeRunner{})
	m := rpcv1.NewMemoryServiceClient(h.conn)
	_, err := m.SaveMemory(asKey("ro-secret"), &rpcv1.SaveMemoryRequest{MemoryId: "m1", UserId: "u", Scope: "user", Content: "x"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a read-only key wrote to the brain: %v", err)
	}
	if _, err := m.SaveMemory(asKey("op-secret"), &rpcv1.SaveMemoryRequest{MemoryId: "m1", UserId: "u", Scope: "user", Role: "user", Content: "x"}); err != nil {
		t.Fatalf("the operator was refused a write to the brain: %v", err)
	}
	if _, err := rpcv1.NewAdminServiceClient(h.conn).Health(asKey("ro-secret"), &rpcv1.HealthRequest{}); err != nil {
		t.Fatalf("a read-only key was refused Health: %v", err)
	}
}

func TestTheCallerNeverLearnsTheInternalToken(t *testing.T) {
	// alchemy is shown a token minted at startup. It is not any key in the
	// policy, so presenting it from outside is presenting an unknown key.
	h := newHarness(t, fakeRunner{})
	_, err := alchemyv1.NewAlchemyClient(h.conn).GetJob(asKey(h.srv.internal), &alchemyv1.GetJobRequest{JobId: "x"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("the internal token worked from outside: %v", err)
	}
}

func TestAStreamThatIsNotThePipelinesIsRefused(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	// WatchJob is a pipeline stream and a read: a reader may open it and gets
	// alchemy's own answer about a job that does not exist.
	stream, err := alchemyv1.NewAlchemyClient(h.conn).WatchJob(asKey("ro-secret"), &alchemyv1.WatchJobRequest{JobId: "never"})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) == codes.PermissionDenied || status.Code(err) == codes.Unauthenticated {
		t.Fatalf("a reader was refused a read stream: %v", err)
	}
}
