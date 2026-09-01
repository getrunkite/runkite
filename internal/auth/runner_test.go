package auth_test

import (
	"os"
	"strconv"
	"strings"
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

func TestRunnerTokens_TenantAllowListsComplete(t *testing.T) {
	var nilRT *auth.RunnerTokens
	if !nilRT.TenantAllowListsComplete() {
		t.Fatal("nil receiver is complete (local mode)")
	}
	withEnv(t, map[string]string{"RUNNER_TOKEN_PYTHON_LANGGRAPH": "tok"})
	rt := auth.LoadRunnerTokensFromEnv()
	if rt.TenantAllowListsComplete() {
		t.Fatal("token without tenants must be incomplete")
	}
	withEnv(t, map[string]string{
		"RUNNER_TOKEN_PYTHON_LANGGRAPH":        "tok-py",
		"RUNNER_TOKEN_TYPESCRIPT_LANGGRAPHJS":  "tok-ts",
		"RUNNER_TENANTS_PYTHON_LANGGRAPH":      "default",
	})
	rt = auth.LoadRunnerTokensFromEnv()
	if rt.TenantAllowListsComplete() {
		t.Fatal("partial tenant lists across kinds must be incomplete")
	}
	withEnv(t, map[string]string{
		"RUNNER_TOKEN_PYTHON_LANGGRAPH":        "tok-py",
		"RUNNER_TOKEN_TYPESCRIPT_LANGGRAPHJS":  "tok-ts",
		"RUNNER_TENANTS_PYTHON_LANGGRAPH":      "default",
		"RUNNER_TENANTS_TYPESCRIPT_LANGGRAPHJS": "acme",
	})
	rt = auth.LoadRunnerTokensFromEnv()
	if !rt.TenantAllowListsComplete() {
		t.Fatal("every tokenized kind listed must be complete")
	}
}

func TestRunnerTokens_AllowsTenant_LocalMode(t *testing.T) {
	var rt *auth.RunnerTokens
	if !rt.AllowsTenant("python-langgraph", "anything") {
		t.Fatal("local mode must not enforce tenant allow-lists")
	}
}

func TestLoadRunnerTokensFromEnv_CommaAllowList(t *testing.T) {
	withEnv(t, map[string]string{
		"RUNNER_TOKEN_PYTHON_LANGGRAPH": "fleet-a, fleet-b",
	})
	rt := auth.LoadRunnerTokensFromEnv()
	if !rt.Validate("python-langgraph", "fleet-a") {
		t.Fatal("first allowlist token should validate")
	}
	if !rt.Validate("python-langgraph", "fleet-b") {
		t.Fatal("second allowlist token should validate")
	}
	if rt.Validate("python-langgraph", "fleet-c") {
		t.Fatal("unknown token must not validate")
	}
	if rt.Validate("typescript-langgraphjs", "fleet-a") {
		t.Fatal("allowlist must not cross kinds")
	}
}

func TestLoadRunnerTokensFromEnv_EmptySegmentsIgnored(t *testing.T) {
	withEnv(t, map[string]string{
		"RUNNER_TOKEN_PYTHON_LANGGRAPH": ",tok-a,, tok-b ,",
	})
	rt := auth.LoadRunnerTokensFromEnv()
	if !rt.Validate("python-langgraph", "tok-a") || !rt.Validate("python-langgraph", "tok-b") {
		t.Fatal("trimmed non-empty segments should validate")
	}
	if rt.Validate("python-langgraph", "") {
		t.Fatal("empty presented token must not validate in production mode")
	}
}

func TestLoadRunnerTokensFromEnv_SoftCapTruncates(t *testing.T) {
	// 17 distinct tokens; only the first 16 must validate.
	parts := make([]string, 17)
	for i := range parts {
		parts[i] = "tok-" + strconv.Itoa(i)
	}
	withEnv(t, map[string]string{
		"RUNNER_TOKEN_PYTHON_LANGGRAPH": strings.Join(parts, ","),
	})
	rt := auth.LoadRunnerTokensFromEnv()
	for i := 0; i < 16; i++ {
		if !rt.Validate("python-langgraph", "tok-"+strconv.Itoa(i)) {
			t.Fatalf("token %d within cap should validate", i)
		}
	}
	if rt.Validate("python-langgraph", "tok-16") {
		t.Fatal("17th token must be truncated / rejected")
	}
}

func TestLoadRunnerTokensFromEnv_OnlyCommasSkipsKind(t *testing.T) {
	// Another kind keeps production mode on; the empty allowlist kind
	// must be omitted (missing-kind reject), not treated as local-mode trust.
	withEnv(t, map[string]string{
		"RUNNER_TOKEN_PYTHON_LANGGRAPH":       ",,,",
		"RUNNER_TOKEN_TYPESCRIPT_LANGGRAPHJS": "ts-tok",
	})
	rt := auth.LoadRunnerTokensFromEnv()
	if !rt.Enabled() {
		t.Fatal("expected production mode from typescript token")
	}
	if rt.Validate("python-langgraph", "tok") {
		t.Fatal("kind with empty allowlist must not validate any token")
	}
	if !rt.Validate("typescript-langgraphjs", "ts-tok") {
		t.Fatal("other kind should still validate")
	}
}
