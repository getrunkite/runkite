package auth

import "context"

type authenticatedRunnerKindKey struct{}

// RunnerKindFromContext returns only a kind established by runner-token
// validation. A header in tokenless local mode is not authenticated identity.
func RunnerKindFromContext(ctx context.Context) string {
	kind, _ := ctx.Value(authenticatedRunnerKindKey{}).(string)
	return kind
}

func withAuthenticatedRunnerKind(ctx context.Context, kind string) context.Context {
	return context.WithValue(ctx, authenticatedRunnerKindKey{}, kind)
}
