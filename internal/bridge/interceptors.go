package bridge

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/getrunkite/runkite/internal/auth"
)

// Runner auth is carried via gRPC metadata (not proto fields) so it applies
// uniformly to every RPC -- unary and streaming -- without touching the
// wire schema. The runner sends these once per call/stream.
const (
	metaRunnerKind  = "runner-kind"
	metaRunnerToken = "runner-token"
)

// authenticateRunner validates the runner-kind/runner-token metadata pair
// against the configured RunnerTokens. In local mode (no tokens configured)
// this always succeeds, a deliberate zero-friction default.
type runnerIdentityKey struct{}

func authenticatedRunner(ctx context.Context) string {
	kind, _ := ctx.Value(runnerIdentityKey{}).(string)
	return kind
}

func authenticateRunner(ctx context.Context, rt *auth.RunnerTokens) (context.Context, error) {
	if !rt.Enabled() {
		return ctx, nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing runner credentials")
	}
	kind := firstOrEmpty(md.Get(metaRunnerKind))
	token := firstOrEmpty(md.Get(metaRunnerToken))
	if kind == "" || token == "" {
		return nil, status.Error(codes.Unauthenticated, "missing runner-kind/runner-token metadata")
	}
	if !rt.Validate(kind, token) {
		return nil, status.Error(codes.Unauthenticated, "invalid runner token")
	}
	return context.WithValue(ctx, runnerIdentityKey{}, kind), nil
}

func firstOrEmpty(vals []string) string {
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// UnaryAuthInterceptor enforces runner auth on unary RPCs (GetJob, ReportStatus).
func UnaryAuthInterceptor(rt *auth.RunnerTokens) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		authenticated, err := authenticateRunner(ctx, rt)
		if err != nil {
			return nil, err
		}
		return handler(authenticated, req)
	}
}

// StreamAuthInterceptor enforces runner auth on streaming RPCs (StreamEvents, WatchCancels).
func StreamAuthInterceptor(rt *auth.RunnerTokens) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		authenticated, err := authenticateRunner(ss.Context(), rt)
		if err != nil {
			return err
		}
		return handler(srv, &authenticatedServerStream{ServerStream: ss, ctx: authenticated})
	}
}

type authenticatedServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authenticatedServerStream) Context() context.Context { return s.ctx }
