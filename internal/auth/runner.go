package auth

import (
	"crypto/subtle"
	"log/slog"
	"os"
	"strings"

	"github.com/getrunkite/runkite/internal/tenant"
)

// RunnerTokens implements two-tier runner authentication:
// local mode (no tokens configured -- runner trusted implicitly, zero setup)
// and production mode (one or more shared tokens per runner_kind). A leaked
// token cannot impersonate a different runner type. Multiple comma-separated
// values for the same kind form a fleet allowlist: independently revocable
// credentials and dual-token rotation, not unique-per-pod secrets (K8s
// Deployments share one Secret across pods of a fleet).
//
// This is a distinct trust boundary from the client-facing
// Provider/Middleware above: it protects the gRPC bridge
// (GetJob/StreamEvents/ReportStatus/WatchCancels) and the /internal/* HTTP
// routes that vend connector credentials and run status -- surfaces the
// client-facing auth middleware always lets through.
//
// Optional per-kind tenant allow-lists (RUNNER_TENANTS_*) restrict which
// X-Runkite-Tenant-Id values a kind token may claim on /internal/*. When
// unset for a kind, any tenant header is still accepted (today's shared
// multi-tenant runner pool). When set, a missing header is treated as
// "default" for the allow check. Tenant lists are per-kind, not per fleet
// token.
type RunnerTokens struct {
	tokens  map[string][]string            // runner_kind -> accepted tokens (allowlist)
	tenants map[string]map[string]struct{} // runner_kind -> allowed tenant ids (absent/empty = unrestricted)
}

// EnvPrefix is the environment variable prefix for runner tokens, e.g.
// RUNNER_TOKEN_PYTHON_LANGGRAPH for runner_kind "python-langgraph".
// The value may be a single token or a comma-separated allowlist.
const EnvPrefix = "RUNNER_TOKEN_"

// TenantsEnvPrefix is the optional allow-list for X-Runkite-Tenant-Id on
// /internal/*, e.g. RUNNER_TENANTS_PYTHON_LANGGRAPH=acme,beta.
const TenantsEnvPrefix = "RUNNER_TENANTS_"

// maxRunnerTokensPerKind is a soft ceiling for the comma-separated
// allowlist. Extra segments after this are ignored (operators should not
// need dozens of fleets per kind).
const maxRunnerTokensPerKind = 16

// LoadRunnerTokensFromEnv scans the process environment for RUNNER_TOKEN_*
// and optional RUNNER_TENANTS_* variables and builds a RunnerTokens set
// keyed by runner_kind.
func LoadRunnerTokensFromEnv() *RunnerTokens {
	rt := &RunnerTokens{
		tokens:  make(map[string][]string),
		tenants: make(map[string]map[string]struct{}),
	}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			continue
		}
		switch {
		case strings.HasPrefix(k, EnvPrefix):
			kind := envKeyToRunnerKind(strings.TrimPrefix(k, EnvPrefix))
			if list := parseTokenAllowList(kind, v); len(list) > 0 {
				rt.tokens[kind] = list
			}
		case strings.HasPrefix(k, TenantsEnvPrefix):
			kind := envKeyToRunnerKind(strings.TrimPrefix(k, TenantsEnvPrefix))
			rt.tenants[kind] = parseTenantAllowList(v)
		}
	}
	return rt
}

func parseTokenAllowList(kind, v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	truncated := false
	for _, part := range parts {
		tok := strings.TrimSpace(part)
		if tok == "" {
			continue
		}
		if len(out) >= maxRunnerTokensPerKind {
			truncated = true
			break
		}
		out = append(out, tok)
	}
	if truncated {
		slog.Warn("runner token allowlist truncated",
			"runner_kind", kind,
			"max", maxRunnerTokensPerKind,
			"hint", "extra comma-separated tokens after the cap are ignored")
	}
	return out
}

func parseTenantAllowList(v string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, part := range strings.Split(v, ",") {
		tid := strings.TrimSpace(part)
		if tid == "" {
			continue
		}
		out[tid] = struct{}{}
	}
	return out
}

// envKeyToRunnerKind reverses the env-safe encoding: env vars can't contain
// hyphens, so "python-langgraph" is encoded as "PYTHON_LANGGRAPH".
func envKeyToRunnerKind(envKey string) string {
	return strings.ToLower(strings.ReplaceAll(envKey, "_", "-"))
}

// Enabled reports whether production mode is active (any token configured).
// When false, all runners are trusted implicitly (local mode, zero friction).
func (rt *RunnerTokens) Enabled() bool {
	return rt != nil && len(rt.tokens) > 0
}

// Validate checks a runner's claimed kind and token. Always true in local
// mode. Compares against every allowlisted token for that kind with
// constant-time equality (OR of matches) so a leaked fleet A token can be
// revoked without rotating fleet B.
func (rt *RunnerTokens) Validate(runnerKind, token string) bool {
	if !rt.Enabled() {
		return true
	}
	want, ok := rt.tokens[runnerKind]
	if !ok || len(want) == 0 {
		return false
	}
	got := []byte(token)
	match := 0
	for _, cand := range want {
		match |= subtle.ConstantTimeCompare(got, []byte(cand))
	}
	return match == 1
}

// AllowsTenant reports whether runnerKind may claim tenantID on /internal/*
// (via X-Runkite-Tenant-Id). Always true in local mode, when no allow-list
// is configured for that kind, or when the allow-list is empty. A missing
// / blank tenantID is treated as tenant.DefaultTenant for the check.
func (rt *RunnerTokens) AllowsTenant(runnerKind, tenantID string) bool {
	if rt == nil || !rt.Enabled() {
		return true
	}
	allow, ok := rt.tenants[runnerKind]
	if !ok || len(allow) == 0 {
		return true
	}
	tid := strings.TrimSpace(tenantID)
	if tid == "" {
		tid = tenant.DefaultTenant
	}
	_, ok = allow[tid]
	return ok
}

// HasTenantAllowList reports whether any RUNNER_TENANTS_* allow-list is
// configured for a kind that also has a token. Used for a startup warning
// when client-facing auth is on but tenant allow-lists are absent.
func (rt *RunnerTokens) HasTenantAllowList() bool {
	if rt == nil {
		return false
	}
	for kind := range rt.tokens {
		if allow, ok := rt.tenants[kind]; ok && len(allow) > 0 {
			return true
		}
	}
	return false
}
