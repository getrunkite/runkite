package auth_test

import (
	"os"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
)

func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		old, existed := os.LookupEnv(k)
		os.Setenv(k, v)
		t.Cleanup(func() {
			if existed {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		})
	}
}

func TestRunnerTokens_DisabledByDefault(t *testing.T) {
	rt := &auth.RunnerTokens{}
	if rt.Enabled() {
		t.Fatal("empty RunnerTokens should be disabled")
	}
	// Local mode: any runner_kind/token (even empty) is trusted.
	if !rt.Validate("python-langgraph", "") {
		t.Fatal("local mode should trust any runner")
	}
}

func TestRunnerTokens_NilReceiverIsDisabled(t *testing.T) {
	var rt *auth.RunnerTokens
	if rt.Enabled() {
		t.Fatal("nil RunnerTokens should be disabled")
	}
	if !rt.Validate("anything", "anything") {
		t.Fatal("nil RunnerTokens (local mode) should trust any runner")
	}
}

func TestLoadRunnerTokensFromEnv_HyphenatedKind(t *testing.T) {
	withEnv(t, map[string]string{
		"RUNNER_TOKEN_PYTHON_LANGGRAPH": "secret-abc",
	})
	rt := auth.LoadRunnerTokensFromEnv()
	if !rt.Enabled() {
		t.Fatal("expected production mode with RUNNER_TOKEN_* set")
	}
	if !rt.Validate("python-langgraph", "secret-abc") {
		t.Fatal("expected valid token to authenticate (hyphen<->underscore mapping)")
	}
	if rt.Validate("python-langgraph", "wrong") {
		t.Fatal("wrong token should not authenticate")
	}
	if rt.Validate("typescript-langgraphjs", "secret-abc") {
		t.Fatal("token for one runner_kind must not authenticate a different kind")
	}
}

func TestLoadRunnerTokensFromEnv_MultipleKinds(t *testing.T) {
	withEnv(t, map[string]string{
		"RUNNER_TOKEN_PYTHON_LANGGRAPH":       "tok-py",
		"RUNNER_TOKEN_TYPESCRIPT_LANGGRAPHJS": "tok-ts",
	})
	rt := auth.LoadRunnerTokensFromEnv()
	if !rt.Validate("python-langgraph", "tok-py") {
		t.Fatal("python token should validate")
	}
	if !rt.Validate("typescript-langgraphjs", "tok-ts") {
		t.Fatal("typescript token should validate")
	}
	// Cross-kind token reuse must fail -- a leaked token for one kind
	// must not impersonate a different runner_kind.
	if rt.Validate("python-langgraph", "tok-ts") {
		t.Fatal("ts token must not authenticate as python runner")
	}
}

func TestRunnerTokens_MissingKindRejected(t *testing.T) {
	withEnv(t, map[string]string{"RUNNER_TOKEN_PYTHON_LANGGRAPH": "tok"})
	rt := auth.LoadRunnerTokensFromEnv()
	if rt.Validate("unregistered-kind", "tok") {
		t.Fatal("unregistered runner_kind should never validate, even with a token that matches another kind")
	}
}

func TestRunnerTokens_AllowsTenant_UnboundByDefault(t *testing.T) {
	withEnv(t, map[string]string{"RUNNER_TOKEN_PYTHON_LANGGRAPH": "tok"})
	rt := auth.LoadRunnerTokensFromEnv()
	if !rt.AllowsTenant("python-langgraph", "acme") {
		t.Fatal("without RUNNER_TENANTS_*, any tenant should be allowed")
	}
	if !rt.AllowsTenant("python-langgraph", "") {
		t.Fatal("empty claim (default) should be allowed when unbound")
	}
}

func TestRunnerTokens_AllowsTenant_BoundAllowList(t *testing.T) {
	withEnv(t, map[string]string{
		"RUNNER_TOKEN_PYTHON_LANGGRAPH":   "tok",
		"RUNNER_TENANTS_PYTHON_LANGGRAPH": "acme, beta",
	})
	rt := auth.LoadRunnerTokensFromEnv()
	if !rt.AllowsTenant("python-langgraph", "acme") {
		t.Fatal("acme should be allowed")
	}
	if !rt.AllowsTenant("python-langgraph", "beta") {
		t.Fatal("beta should be allowed")
	}
	if rt.AllowsTenant("python-langgraph", "evil") {
		t.Fatal("evil should be denied")
	}
	if rt.AllowsTenant("python-langgraph", "") {
		t.Fatal("missing header → default should be denied when default not listed")
	}
	if !rt.AllowsTenant("other-kind", "evil") {
		t.Fatal("kinds without an allow-list stay unrestricted")
	}
}

func TestRunnerTokens_AllowsTenant_DefaultExplicit(t *testing.T) {
	withEnv(t, map[string]string{
		"RUNNER_TOKEN_PYTHON_LANGGRAPH":   "tok",
		"RUNNER_TENANTS_PYTHON_LANGGRAPH": "default,acme",
	})
	rt := auth.LoadRunnerTokensFromEnv()
	if !rt.AllowsTenant("python-langgraph", "") {
		t.Fatal("empty claim should match listed default")
	}
}

func TestRunnerTokens_AllowsTenant_LocalMode(t *testing.T) {
	var rt *auth.RunnerTokens
	if !rt.AllowsTenant("python-langgraph", "anything") {
		t.Fatal("local mode must not enforce tenant allow-lists")
	}
}
