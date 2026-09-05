package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
	"github.com/liliang-cn/cortexdb/v2/pkg/rpcserver"
)

// One policy, two services, one chain.
//
// alchemy and CortexDB each arrive with an authorization of their own. alchemy
// checks a single bearer token and refuses to be built without one; CortexDB
// checks a scoped key — clearance, row confinement, per-tool classification —
// and hands the caller's key down to the handlers that check ownership. A
// grpc.Server has one interceptor chain, so mounting both services on one
// listener means one of these has to give way.
//
// Neither does. Athanor's interceptor authenticates every call against the
// scoped-key policy, then routes by service: a cortexdb.v1 call goes to
// CortexDB's own interceptor unchanged, and an alchemy.v1 call is classified
// here (methods.go), authorized against the caller's clearance, and passed to
// alchemy's interceptor carrying a token that never leaves this process. That
// token is minted at startup and known to nobody, so alchemy's invariant — no
// token, no service — holds, and the only credentials a caller can present
// are the ones the key file names.
//
// What this does not do: row confinement on the pipeline. A scoped key
// confined to one user_id has nothing on a CreateJob to be confined by. That
// is a gap the decision ledger closes, not one to paper over here.

// internalToken is the credential alchemy's interceptor is shown. Minted per
// process, never logged, never configurable.
func internalToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint internal token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// bearerFrom pulls the secret out of incoming metadata.
func bearerFrom(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	for _, v := range md.Get("authorization") {
		if secret, found := strings.CutPrefix(v, "Bearer "); found && secret != "" {
			return secret, true
		}
	}
	return "", false
}

// withInternalToken replaces the caller's credential with the process's own
// before an alchemy.v1 call reaches alchemy's interceptor. The caller has
// already been authorized; what alchemy sees is proof that Athanor let them
// through, not their key.
func withInternalToken(ctx context.Context, token string) context.Context {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		md = metadata.MD{}
	} else {
		md = md.Copy()
	}
	md.Set("authorization", "Bearer "+token)
	return metadata.NewIncomingContext(ctx, md)
}

// authorizeAlchemy is the pipeline half of the policy: who is calling, and
// may they call this. Row confinement does not apply — see the package note.
func (s *Server) authorizeAlchemy(ctx context.Context, fullMethod string) error {
	access, classified := alchemyAccess[fullMethod]
	if !classified {
		return status.Errorf(codes.PermissionDenied, "denied: %s is not classified as a read or a write", fullMethod)
	}
	if !s.keys.Enabled() {
		return nil
	}
	secret, ok := bearerFrom(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing authorization metadata")
	}
	key, ok := s.keys.Lookup(secret)
	if !ok {
		return status.Error(codes.Unauthenticated, "invalid token")
	}
	if err := key.AuthorizeOperation(fullMethod, authz.Method{Access: access}); err != nil {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	return nil
}

func (s *Server) unaryAuth() grpc.UnaryServerInterceptor {
	cortex := rpcserver.AuthInterceptor(s.keys, s.db)
	pipeline := s.alchemy.UnaryInterceptor()
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !strings.HasPrefix(info.FullMethod, alchemyPrefix) {
			return cortex(ctx, req, info, handler)
		}
		if err := s.authorizeAlchemy(ctx, info.FullMethod); err != nil {
			return nil, err
		}
		return pipeline(withInternalToken(ctx, s.internal), req, info, handler)
	}
}

// streamAuth is the same routing for streams. CortexDB has no streaming RPCs,
// so a stream that is not alchemy's is refused rather than let through
// unchecked — the safe direction if one is ever added upstream without this
// file learning about it.
func (s *Server) streamAuth() grpc.StreamServerInterceptor {
	pipeline := s.alchemy.StreamInterceptor()
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !strings.HasPrefix(info.FullMethod, alchemyPrefix) {
			return status.Errorf(codes.PermissionDenied, "denied: %s is a stream this server has no policy for", info.FullMethod)
		}
		if err := s.authorizeAlchemy(ss.Context(), info.FullMethod); err != nil {
			return err
		}
		return pipeline(srv, &swappedStream{ServerStream: ss, ctx: withInternalToken(ss.Context(), s.internal)}, info, handler)
	}
}

// swappedStream is a ServerStream whose context carries the internal token.
type swappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *swappedStream) Context() context.Context { return w.ctx }

// httpKey authenticates a plain HTTP request against the same policy, for the
// routes Athanor serves itself. It returns the key, or the status and message
// to answer with.
func (s *Server) httpKey(authorization string) (authz.Key, int, string) {
	if !s.keys.Enabled() {
		return authz.Key{ID: "open", Clearance: authz.ReadWrite}, 0, ""
	}
	secret, found := strings.CutPrefix(authorization, "Bearer ")
	if !found || secret == "" {
		return authz.Key{}, 401, "missing bearer token"
	}
	key, ok := s.keys.Lookup(secret)
	if !ok {
		return authz.Key{}, 401, "invalid token"
	}
	return key, 0, ""
}
