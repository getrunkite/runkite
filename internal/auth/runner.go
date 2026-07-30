package auth

import (
	"crypto/subtle"
	"os"
	"strings"
)

// RunnerTokens implements two-tier runner authentication:
// local mode (no tokens configured -- runner trusted implicitly, zero setup)
// and production mode (one shared token per runner_kind, so a leaked token
// cannot impersonate a different runner type). This is a distinct trust
// boundary from the client-facing Provider/Middleware above: it protects the
// gRPC bridge (GetJob/StreamEvents/ReportStatus/WatchCancels) and the
// /internal/* HTTP routes that vend connector credentials and run status --
// surfaces the client-facing auth middleware always lets through.
type RunnerTokens struct {
	tokens map[string]string // runner_kind -> token
}

// EnvPrefix is the environment variable prefix for runner tokens, e.g.
// RUNNER_TOKEN_PYTHON_LANGGRAPH for runner_kind "python-langgraph".
const EnvPrefix = "RUNNER_TOKEN_"

// LoadRunnerTokensFromEnv scans the process environment for RUNNER_TOKEN_*
// variables and builds a RunnerTokens set keyed by runner_kind.
func LoadRunnerTokensFromEnv() *RunnerTokens {
	rt := &RunnerTokens{tokens: make(map[string]string)}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(k, EnvPrefix) || v == "" {
			continue
		}
		kind := envKeyToRunnerKind(strings.TrimPrefix(k, EnvPrefix))
		rt.tokens[kind] = v
	}
	return rt
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
// mode. Uses a constant-time comparison to avoid leaking token contents via
// timing side-channels.
func (rt *RunnerTokens) Validate(runnerKind, token string) bool {
	if !rt.Enabled() {
		return true
	}
	want, ok := rt.tokens[runnerKind]
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1
}
