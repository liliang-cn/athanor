// Command athanor serves CortexDB's brain and alchemy's pipeline behind one
// door: one gRPC port carrying alchemy.v1 and cortexdb.v1, one HTTP port
// carrying their REST translations, the review UI, the live graph, metrics,
// and the front page.
//
// Configuration (flags override env; .env is loaded when present):
//
//	ATHANOR_DB          brain path or postgres:// DSN (default ~/.athanor/brain.db)
//	ATHANOR_GRPC_ADDR   default 127.0.0.1:47831
//	ATHANOR_HTTP_ADDR   default 127.0.0.1:47832
//	ATHANOR_KEY_FILE    scoped-key policy; the whole policy when set
//	ATHANOR_TOKEN       legacy single key; ignored when a key file is set
//	ATHANOR_BACKUP_DIR  where AdminService.Backup may write
//	ATHANOR_SPOOL       where uploaded sources wait; default the OS temp dir
//	ATHANOR_LIVEDB_HOSTS  host or host:port list /athanor/livedb may dial,
//	                      comma-separated; empty is unconfined
//	OPENAI_BASE_URL etc the brain's embedder, exactly as cortexdb-grpc reads them
//
// `athanor -health` probes a running server's gRPC port and exits non-zero
// when it is not serving, so the binary is its own liveness check.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	rpcv1 "github.com/liliang-cn/cortexdb/v2/pkg/rpc/v1"

	"github.com/liliang-cn/athanor/pkg/server"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// hostList reads the live-database allow-list. An empty string is no list at
// all, which is unconfined — and not a list of one empty host, which would
// confine the server to nothing and read on the front page as the same thing.
func hostList(raw string) []string {
	var out []string
	for _, entry := range strings.Split(raw, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

func defaultDB() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "brain.db"
	}
	return filepath.Join(home, ".athanor", "brain.db")
}

func main() {
	_ = godotenv.Load()
	var (
		dbPath   = flag.String("db", envOr("ATHANOR_DB", defaultDB()), "brain path or postgres:// DSN")
		grpcAddr = flag.String("addr", envOr("ATHANOR_GRPC_ADDR", "127.0.0.1:47831"), "gRPC listen address")
		httpAddr = flag.String("http-addr", envOr("ATHANOR_HTTP_ADDR", "127.0.0.1:47832"), "HTTP listen address")
		keyFile  = flag.String("keys", envOr("ATHANOR_KEY_FILE", ""), "scoped-key policy file")
		token    = flag.String("token", os.Getenv("ATHANOR_TOKEN"), "legacy single key (ignored when -keys is set)")
		backup   = flag.String("backup-dir", envOr("ATHANOR_BACKUP_DIR", ""), "directory AdminService.Backup may write into")
		spool    = flag.String("spool", envOr("ATHANOR_SPOOL", ""), "directory uploaded sources wait in")
		health   = flag.Bool("health", false, "probe a running server at -addr and exit")
		liveHost = flag.String("livedb-hosts", envOr("ATHANOR_LIVEDB_HOSTS", ""),
			"comma-separated host or host:port list pkg/livedb may dial; empty is unconfined")
	)
	flag.Parse()

	if *health {
		if err := probe(*grpcAddr, *token); err != nil {
			log.Fatalf("unhealthy: %v", err)
		}
		log.Printf("ok %s", *grpcAddr)
		return
	}

	if dir := filepath.Dir(*dbPath); !isDSN(*dbPath) {
		_ = os.MkdirAll(dir, 0o700)
	}
	emb, embDesc, err := server.EmbedderFromEnv()
	if err != nil {
		log.Fatalf("embedder: %v", err)
	}
	if emb != nil {
		log.Printf("embedder: %s", embDesc)
	} else {
		log.Printf("embedder: none (lexical mode)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s, err := server.New(ctx, server.Options{
		DBPath: *dbPath, GRPCAddr: *grpcAddr, HTTPAddr: *httpAddr,
		KeyFile: *keyFile, Token: *token, BackupDir: *backup, Spool: *spool,
		LiveDBHosts: hostList(*liveHost),
		Embedder:    emb,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	policy := "disabled"
	switch {
	case *keyFile != "":
		policy = "scoped keys (" + *keyFile + ")"
	case *token != "":
		policy = "bearer token"
	}
	log.Printf("athanor: brain=%s grpc=%s http=%s auth=%s", *dbPath, *grpcAddr, *httpAddr, policy)
	if err := s.Serve(ctx); err != nil {
		log.Fatal(err)
	}
}

func isDSN(p string) bool {
	return len(p) > 11 && (p[:11] == "postgres://" || (len(p) > 13 && p[:13] == "postgresql://"))
}

// probe asks the brain half whether it is serving. A wildcard listen address
// is probed on loopback, as cortexdb-grpc does.
func probe(addr, token string) error {
	if len(addr) > 0 && (addr[:7] == "0.0.0.0" || addr[:1] == ":") {
		if i := lastColon(addr); i >= 0 {
			addr = "127.0.0.1" + addr[i:]
		}
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}
	h, err := rpcv1.NewAdminServiceClient(conn).Health(ctx, &rpcv1.HealthRequest{})
	if err != nil {
		return err
	}
	if !h.GetOk() {
		return errNotOK
	}
	return nil
}

func lastColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

var errNotOK = &probeError{}

type probeError struct{}

func (*probeError) Error() string { return "server reported not ok" }
