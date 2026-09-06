// Package server assembles Athanor: CortexDB's brain and alchemy's pipeline
// behind one door.
//
// Nothing here is a second implementation of anything. alchemy's service,
// runner and REST gateway; CortexDB's gRPC services, REST, live graph, scoped
// keys and metrics — every one is a public constructor from the project that
// owns it, and this package's whole job is the order they are called in and
// the two places where combining them needed a decision: one authorization
// policy over two services (auth.go), and the one verb that moves a finished
// graph into the brain (loads.go).
package server

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/liliang-cn/alchemy/pkg/gateway"
	"github.com/liliang-cn/alchemy/pkg/job"
	"github.com/liliang-cn/alchemy/pkg/runner"
	"github.com/liliang-cn/alchemy/pkg/service"
	alchemyv1 "github.com/liliang-cn/alchemy/proto/alchemy/v1"

	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
	"github.com/liliang-cn/cortexdb/v2/pkg/httpapi"
	"github.com/liliang-cn/cortexdb/v2/pkg/liveview"
	"github.com/liliang-cn/cortexdb/v2/pkg/observability"
	"github.com/liliang-cn/cortexdb/v2/pkg/rpcserver"
)

// Server is one running Athanor.
type Server struct {
	opts     Options
	db       *cortexdb.DB
	keys     *authz.KeySet
	internal string
	describe string

	alchemy *service.Server
	ledger  ledger
	// liveDB, when set, is where the live-database routes get their store.
	// Nil is the real one, built once per brain on first use (livedb.go).
	// It is a function rather than a store so that a test can express both
	// halves of what those handlers face — a working store, and a store that
	// could not be built — without either depending on how far pkg/livedb's
	// own implementation has got.
	liveDB func() (livedbStore, error)
	// follows are the background live-database follows this server is
	// running, and followCtx is what they run under: the server's own
	// context, so that the request which started one may end without ending
	// it (livedb_follows.go). stopFollows ends every one of them.
	follows     *livedbFollowSet
	followCtx   context.Context
	stopFollows context.CancelFunc
	metrics     *observability.Registry
	grpc        *grpc.Server
	mux         *http.ServeMux
	http        *http.Server
	view        *liveview.Server

	gatewayConn   *grpc.ClientConn
	stopGateway   context.CancelFunc
	grpcListener  net.Listener
	httpListener  net.Listener
	grpcAddrReady chan struct{}
}

// New builds everything that does not need a bound port. Serve binds them.
func New(ctx context.Context, opts Options) (*Server, error) {
	opts = opts.withDefaults()
	if opts.DBPath == "" {
		return nil, errors.New("athanor: a database path or DSN is required")
	}

	keys, err := authz.Resolve(opts.KeyFile, opts.Token)
	if err != nil {
		return nil, fmt.Errorf("athanor: key policy: %w", err)
	}
	internal, err := internalToken()
	if err != nil {
		return nil, err
	}

	var dbOpts []cortexdb.Option
	if opts.Embedder != nil {
		dbOpts = append(dbOpts, cortexdb.WithEmbedder(opts.Embedder))
	}
	db, err := cortexdb.Open(cortexdb.DefaultConfig(opts.DBPath), dbOpts...)
	if err != nil {
		return nil, fmt.Errorf("athanor: open brain: %w", err)
	}

	run := opts.Runner
	if run == nil {
		r, err := runner.New(runner.Config{Factory: modelFactory{}})
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("athanor: pipeline: %w", err)
		}
		run = r
	}
	svc, err := service.New(service.Config{
		Runner: run,
		Store:  job.New(job.Config{Capacity: opts.JobCapacity}),
		Token:  internal,
		Spool:  opts.Spool,
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("athanor: pipeline service: %w", err)
	}

	metrics := observability.NewRegistry()
	if err := metrics.PublishExpvar("athanor"); err != nil {
		// A second Server in one process (tests) would collide on the name;
		// that is not worth refusing to start over.
		_ = err
	}

	s := &Server{
		opts: opts, db: db, keys: keys, internal: internal,
		describe: opts.DBPath, alchemy: svc, ledger: brainLedger{db: db}, metrics: metrics,
		grpcAddrReady: make(chan struct{}), follows: newLivedbFollowSet(),
	}
	s.followCtx, s.stopFollows = context.WithCancel(ctx)

	// One listener, two services, one policy. Metrics wrap authorization so
	// denials are counted — the lesson CortexDB's own server learned.
	// The ledger hooks come last in each chain, after authorization: an act
	// nobody was allowed to perform is never recorded, and the actor an entry
	// carries is the key the policy resolved (ledger_hooks.go).
	s.grpc = grpc.NewServer(
		grpc.ChainUnaryInterceptor(rpcserver.MetricsInterceptor(metrics), s.unaryAuth(), s.ledgerUnary()),
		grpc.ChainStreamInterceptor(s.streamAuth(), s.ledgerStream()),
	)
	alchemyv1.RegisterAlchemyServer(s.grpc, svc)
	backupDir := opts.BackupDir
	if backupDir == "" && !isDSN(opts.DBPath) {
		backupDir = filepath.Dir(opts.DBPath)
	}
	rpcserver.Register(s.grpc, db, rpcserver.Options{Keys: keys, DBPath: opts.DBPath, BackupDir: backupDir})

	// The live graph reads through the same handle the loads write through.
	view, err := liveview.New(ctx, liveview.SourceFor(db, opts.DBPath), liveview.DefaultInterval, false)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("athanor: live view: %w", err)
	}
	s.view = view

	brainREST, err := httpapi.NewWithPolicy(db, httpapi.Options{Keys: keys, DBPath: opts.DBPath})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("athanor: brain REST: %w", err)
	}

	mux := http.NewServeMux()
	// alchemy's gateway is attached in Serve, once the gRPC port it dials exists.
	mux.Handle("/brain/", http.StripPrefix("/brain", brainREST))
	// The graph and the metrics are reads, but they are reads of everything;
	// they get the same door as every other route (gate.go).
	mux.Handle("/graph/", s.requireKey(http.StripPrefix("/graph", view.Handler())))
	mux.HandleFunc("/athanor/loads", s.handleLoads)
	mux.HandleFunc("/athanor/loads/form", s.handleLoadForm)
	// The vocabulary as a workflow rather than a string pasted into every job:
	// draft, propose from a run, approve, publish (ontologies.go). "current" is
	// a literal segment and an id always carries an "@", so it cannot be one.
	mux.HandleFunc("/athanor/ontologies", s.handleOntologies)
	mux.HandleFunc("/athanor/ontologies/current", s.handleOntologyCurrent)
	mux.HandleFunc("/athanor/ontologies/{id}", s.handleOntologyVersion)
	// A database somebody else runs: propose a plan from its schema, sign the
	// hash you read, and only then import (livedb.go). "current" is a literal
	// segment and Go's mux prefers it to {id}, exactly as it does for the
	// ontologies above.
	mux.HandleFunc("/athanor/livedb/plans", s.handleLivedbPlans)
	mux.HandleFunc("/athanor/livedb/plans/current", s.handleLivedbCurrent)
	mux.HandleFunc("/athanor/livedb/plans/{id}", s.handleLivedbPlan)
	mux.HandleFunc("/athanor/livedb/plans/{id}/signature", s.handleLivedbSignature)
	mux.HandleFunc("/athanor/livedb/runs", s.handleLivedbRuns)
	// Keeping the brain in step with the database afterwards is a job this
	// server owns rather than a request somebody holds open, so it is a
	// resource with a lifetime and not a flag on a run (livedb_follows.go).
	mux.HandleFunc("/athanor/livedb/follows", s.handleLivedbFollows)
	mux.HandleFunc("/athanor/livedb/follows/{id}", s.handleLivedbFollow)
	// The ledger: what this server did, and why. Reads only — an entry is
	// written by performing the act it describes (ledger.go). The id pattern
	// takes the rest of the path because a decision id carries colons and a
	// load name is the caller's own string.
	mux.HandleFunc("/athanor/decisions", s.handleDecisions)
	mux.HandleFunc("/athanor/decisions/{id...}", s.handleDecisionChain)
	mux.Handle("/metrics", s.requireKey(metrics.Handler()))
	mux.Handle("/debug/vars", s.requireKey(expvar.Handler()))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/signin", s.handleSignin)
	mux.HandleFunc("/signout", s.handleSignout)
	mux.HandleFunc("/", s.handleHome)
	s.mux = mux
	return s, nil
}

func isDSN(path string) bool {
	return len(path) > 11 && (path[:11] == "postgres://" || (len(path) > 13 && path[:13] == "postgresql://"))
}

// Serve binds both ports and blocks until ctx ends or a listener fails.
func (s *Server) Serve(ctx context.Context) error {
	gl, err := net.Listen("tcp", s.opts.GRPCAddr)
	if err != nil {
		return fmt.Errorf("athanor: listen grpc %s: %w", s.opts.GRPCAddr, err)
	}
	s.grpcListener = gl
	hl, err := net.Listen("tcp", s.opts.HTTPAddr)
	if err != nil {
		_ = gl.Close()
		return fmt.Errorf("athanor: listen http %s: %w", s.opts.HTTPAddr, err)
	}
	s.httpListener = hl

	// The REST translation of the pipeline dials our own gRPC front door —
	// alchemy's rule, kept: a gateway holding the service could call what no
	// RPC exposes and skip the interceptors that authorize. The caller's
	// Authorization header is forwarded as metadata, so the key a curl user
	// presents is the key the policy checks.
	conn, err := grpc.NewClient(gl.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("athanor: gateway dial: %w", err)
	}
	gctx, cancel := context.WithCancel(ctx)
	gw, err := gateway.New(gctx, conn)
	if err != nil {
		cancel()
		_ = conn.Close()
		return fmt.Errorf("athanor: gateway: %w", err)
	}
	s.gatewayConn, s.stopGateway = conn, cancel
	s.mux.Handle("/v1/", gw)
	// One sign-in for both browser UIs: Athanor's cookie becomes the header
	// alchemy's viewer already accepts (session.go).
	s.mux.Handle("/ui/", s.oneSignIn(gw))
	close(s.grpcAddrReady)

	s.http = &http.Server{Handler: s.mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- s.grpc.Serve(gl) }()
	go func() {
		if err := s.http.Serve(hl); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case <-ctx.Done():
		s.shutdown()
		return nil
	case err := <-errc:
		s.shutdown()
		return err
	}
}

// Addrs reports the bound addresses once Serve has bound them.
func (s *Server) Addrs() (grpcAddr, httpAddr string) {
	<-s.grpcAddrReady
	return s.grpcListener.Addr().String(), s.httpListener.Addr().String()
}

func (s *Server) shutdown() {
	// Before the listeners, so that nothing new starts one, and before the
	// brain is closed either here or in Close.
	s.stopFollowing()
	if s.http != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = s.http.Shutdown(ctx)
		cancel()
	}
	s.grpc.GracefulStop()
	if s.stopGateway != nil {
		s.stopGateway()
	}
	if s.gatewayConn != nil {
		_ = s.gatewayConn.Close()
	}
}

// Close releases what New opened. Serve's shutdown handles the listeners.
func (s *Server) Close() error {
	// Again, because a Server that never reached Serve still started what it
	// started. It is idempotent, and it is what makes closing the brain below
	// safe: a follow writes into it.
	s.stopFollowing()
	s.alchemy.Close()
	if s.view != nil {
		_ = s.view.Close()
	}
	return s.db.Close()
}
