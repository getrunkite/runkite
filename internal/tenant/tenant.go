// Package tenant implements multi-tenancy -- a flat tenant_id scope (not a
// full workspace/org/team hierarchy), threaded through
// context.Context rather than as an explicit parameter on every state.Store
// method. This is deliberate: retrofitting an explicit tenantID parameter
// onto ~25 Store interface methods would touch every call site across the
// API layer and every existing test for a property that's naturally a
// per-request ambient value, exactly like auth.AuthResult already is (see
// internal/auth's FromContext/WithContext, which this mirrors). Every
// handler already passes r.Context() (or a context derived from it) into
// the store -- auth.Middleware populating the tenant into that same context
// is enough to make every existing call site tenant-aware with zero
// changes to internal/api.
//
// DefaultTenant is what every request resolves to when multi-tenancy isn't
// configured (no auth provider, or a provider that doesn't supply a tenant
// claim) -- single-tenant deployments are completely unaffected; this is
// purely additive, not a breaking migration.
package tenant

import "context"

// DefaultTenant is the implicit tenant for requests with no tenant claim.
const DefaultTenant = "default"

type contextKey struct{}
type systemKey struct{}

// FromContext returns the tenant ID for ctx, or DefaultTenant if none was
// set (auth disabled, or a provider that doesn't supply one).
func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(contextKey{}).(string); ok && v != "" {
		return v
	}
	return DefaultTenant
}

// WithContext attaches a tenant ID to ctx. An empty id is treated the same
// as not calling this at all (resolves to DefaultTenant) -- a provider that
// authenticates successfully but has no tenant claim configured doesn't
// accidentally lock every caller out of "default"'s data.
func WithContext(ctx context.Context, tenantID string) context.Context {
	if tenantID == "" {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, tenantID)
}

// SystemContext marks ctx as a trusted, internal, control-plane-driven
// operation that must see across every tenant rather than being filtered
// to one -- NOT a caller-facing concept, and never derived from anything a
// client can influence. Exists because some internal flows genuinely can't
// know a resource's tenant in advance:
//   - The gRPC bridge's StatusCallback / checkpoint persistence path is
//     driven by a runID from a runner's stream, not an authenticated HTTP
//     request -- it must look up the run's actual tenant before it can
//     even know what tenant context to use.
//   - The cron scheduler's tick loop must list schedules across every
//     tenant to service all of them, then dispatch each fire under its
//     OWN schedule's tenant (not system) so the resulting run is
//     correctly attributed.
//
// Store implementations recognize this to skip WHERE tenant_id = ?
// filtering entirely for that call. Never wire this to anything reachable
// from an incoming HTTP request.
func SystemContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemKey{}, true)
}

// IsSystem reports whether ctx was marked via SystemContext.
func IsSystem(ctx context.Context) bool {
	v, _ := ctx.Value(systemKey{}).(bool)
	return v
}
