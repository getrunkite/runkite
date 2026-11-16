package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/getrunkite/runkite/internal/policy"
)

const minimalPolicyFixture = `{"schema_version":1,"policy":{},"cases":[{"id":"default-deny","input":{"stage":"tool.call","tenant_id":"acme","principal":"alice","agent_id":"agent","connector":"tools","tool":"read","args":{}},"expect":{"effect":"deny","reason_code":"policy_default_deny","rule_id":""}}]}`

func fixtureCLI(t *testing.T, raw string) (int, string, string) {
	t.Helper()
	var out, err bytes.Buffer
	status := runPolicyTest(strings.NewReader(raw), &out, &err, []string{"-f", "-"})
	return status, out.String(), err.String()
}

func TestOfflinePolicyExampleMatchesEngineSemantics(t *testing.T) {
	raw, err := os.ReadFile("../examples/policy_fixtures/refunds.json")
	if err != nil {
		t.Fatal(err)
	}
	status, output, diagnostic := fixtureCLI(t, string(raw))
	if status != 0 || diagnostic != "" {
		t.Fatal(status, diagnostic, output)
	}
	var report policyFixtureReport
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatal(err)
	}
	if report.Passed != 9 || report.Failed != 0 || len(report.Cases) != 9 || !strings.HasPrefix(report.PolicyRevision, "policy-v1-") {
		t.Fatal(output)
	}
	// Repeat from identical bytes. Timing, random IDs and mutable state must not
	// affect output, and this report must not contain the test's raw arguments.
	status, repeated, diagnostic := fixtureCLI(t, string(raw))
	if status != 0 || diagnostic != "" || repeated != output {
		t.Fatal("nondeterministic report")
	}
	if strings.Contains(output, `"amount"`) || strings.Contains(output, "declared-alice") {
		t.Fatal("report echoed case input")
	}
}

func TestOfflinePolicyMismatchKeepsAllCases(t *testing.T) {
	raw, err := os.ReadFile("../examples/policy_fixtures/refunds.json")
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(raw), `"effect": "allow", "reason_code": "", "rule_id": "refund-tools"`, `"effect": "deny", "reason_code": "", "rule_id": "refund-tools"`, 1)
	status, out, diagnostic := fixtureCLI(t, changed)
	if status != 1 || diagnostic != "" {
		t.Fatal(status, diagnostic)
	}
	var report policyFixtureReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Passed != 8 || report.Failed != 1 || len(report.Cases) != 9 {
		t.Fatal(out)
	}
}

func TestOfflinePolicyStrictValidation(t *testing.T) {
	cases := map[string]string{
		"unknown root":              strings.Replace(minimalPolicyFixture, `"schema_version":1`, `"url":"http://127.0.0.1/forbidden","schema_version":1`, 1),
		"webhook rejected":          strings.Replace(minimalPolicyFixture, `"policy":{}`, `"policy":{"webhook":{"url":"http://127.0.0.1/forbidden"}}`, 1),
		"audit rejected":            strings.Replace(minimalPolicyFixture, `"policy":{}`, `"policy":{"audit_required":true}`, 1),
		"overlay rejected":          strings.Replace(minimalPolicyFixture, `"policy":{}`, `"policy":{"overlays":[]}`, 1),
		"duplicate":                 strings.Replace(minimalPolicyFixture, `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1),
		"case variant":              strings.Replace(minimalPolicyFixture, `"tenant_id"`, `"Tenant_ID"`, 1),
		"argument duplicate":        strings.Replace(minimalPolicyFixture, `"args":{}`, `"args":{"secret":"never-print-me","secret":"second"}`, 1),
		"null expectation metadata": strings.Replace(minimalPolicyFixture, `"reason_code":"policy_default_deny"`, `"reason_code":null`, 1),
		"null rule list":            strings.Replace(minimalPolicyFixture, `"policy":{}`, `"policy":{"grants":null}`, 1),
		"null policy":               strings.Replace(minimalPolicyFixture, `"policy":{}`, `"policy":null`, 1),
		"version":                   strings.Replace(minimalPolicyFixture, `"schema_version":1`, `"schema_version":2`, 1),
		"unknown stage":             strings.Replace(minimalPolicyFixture, `"stage":"tool.call"`, `"stage":"resources/read"`, 1),
		"null args":                 strings.Replace(minimalPolicyFixture, `"args":{}`, `"args":null`, 1),
		"array args":                strings.Replace(minimalPolicyFixture, `"args":{}`, `"args":[]`, 1),
		"unknown expectation":       strings.Replace(minimalPolicyFixture, `"effect":"deny"`, `"effect":"hitl"`, 1),
		"missing principal":         strings.Replace(minimalPolicyFixture, `"principal":"alice",`, "", 1),
		"trailing document":         minimalPolicyFixture + ` {}`,
		"control identity":          strings.Replace(minimalPolicyFixture, `"principal":"alice"`, `"principal":"a\nb"`, 1),
		"argument bytes":            strings.Replace(minimalPolicyFixture, `"args":{}`, `"args":{"x":"`+strings.Repeat("x", maxPolicyFixtureArgs)+`"}`, 1),
		"nesting":                   strings.Replace(minimalPolicyFixture, `"args":{}`, `"args":{"x":`+strings.Repeat("[", maxPolicyFixtureDepth)+`0`+strings.Repeat("]", maxPolicyFixtureDepth)+`}`, 1),
		"file bytes":                strings.Repeat(" ", maxPolicyFixtureBytes) + minimalPolicyFixture,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			status, out, diagnostic := fixtureCLI(t, raw)
			if status != 2 || out != "" || diagnostic == "" || strings.Contains(diagnostic, "never-print-me") {
				t.Fatalf("status=%d stdout=%s stderr=%s", status, out, diagnostic)
			}
		})
	}
}

func TestOfflinePolicyInvalidPredicateIsNotSilentlyDropped(t *testing.T) {
	for _, predicate := range []string{
		`{"id":"bad","tenant_id":"acme","connector":"tools","when":{"path":"amount","op":"misspelled","value":1},"effect":"deny"}`,
		`{"id":"bad","tenant_id":"acme","connector":"tools","when":{"path":"amount","op":"gt","value":1},"effect":"allow"}`,
		`{"id":"bad","tenant_id":"acme","connector":"tools","when":{"path":"amount","op":"in","value":1},"effect":"deny"}`,
	} {
		raw := strings.Replace(minimalPolicyFixture, `"policy":{}`, `"policy":{"predicates":[`+predicate+`]}`, 1)
		status, out, _ := fixtureCLI(t, raw)
		if status != 2 || out != "" {
			t.Fatal(status, out)
		}
	}
}

func TestOfflinePolicyPreservesExactNumbersAndTypes(t *testing.T) {
	raw := `{"schema_version":1,"policy":{"default_effect":"allow","predicates":[{"id":"exact","tenant_id":"acme","connector":"tools","when":{"path":"amount","op":"gt","value":9007199254740992},"effect":"deny"}]},"cases":[{"id":"large","input":{"stage":"tool.call","tenant_id":"acme","principal":"alice","agent_id":"agent","connector":"tools","tool":"read","args":{"amount":9007199254740993}},"expect":{"effect":"deny","rule_id":"exact"}}]}`
	status, out, diagnostic := fixtureCLI(t, raw)
	if status != 0 || diagnostic != "" {
		t.Fatal(status, out, diagnostic)
	}
	// Floating-point coercion would collapse these adjacent values and fail the
	// denial above. A JSON string must independently remain nonnumeric.
	raw = strings.Replace(raw, `"amount":9007199254740993`, `"amount":"9007199254740993"`, 1)
	raw = strings.Replace(raw, `"expect":{"effect":"deny","rule_id":"exact"}`, `"expect":{"effect":"allow","rule_id":""}`, 1)
	status, out, diagnostic = fixtureCLI(t, raw)
	if status != 0 || diagnostic != "" {
		t.Fatal(status, out, diagnostic)
	}
}

type policyNoNetwork struct{ calls atomic.Int32 }

func (t *policyNoNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("offline policy attempted HTTP")
}

func TestOfflinePolicyIgnoresAmbientConfigSecretsAndNetwork(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "fixtures.json")
	raw := strings.Replace(minimalPolicyFixture, `"policy":{}`, `"policy":{"default_effect":"allow","predicates":[{"id":"literal","tenant_id":"acme","connector":"tools","when":{"path":"literal","op":"eq","value":"${FIXTURE_SECRET}"},"effect":"pending"}]}`, 1)
	raw = strings.Replace(raw, `"args":{}`, `"args":{"literal":"${FIXTURE_SECRET}","secret_ref":"vault:secret/data/must-not-read#token","url":"http://127.0.0.1/must-not-call","private":"sensitive-literal"}`, 1)
	raw = strings.Replace(raw, `"effect":"deny","reason_code":"policy_default_deny","rule_id":""`, `"effect":"pending","reason_code":"policy_pending","rule_id":"literal"`, 1)
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	trap := &policyNoNetwork{}
	prior := http.DefaultTransport
	http.DefaultTransport = trap
	t.Cleanup(func() { http.DefaultTransport = prior })
	for key, value := range map[string]string{"RUNKITE_URL": "http://127.0.0.1/must-not-call", "RUNKITE_API_KEY": "sensitive-ambient-key", "VAULT_ADDR": "http://127.0.0.1/must-not-call", "POSTGRES_DSN": "postgres://127.0.0.1/must-not-call", "SQLITE_PATH": filepath.Join(directory, "must-not-create.db"), "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1/must-not-call", "FIXTURE_SECRET": "sensitive-resolved-secret"} {
		t.Setenv(key, value)
	}
	var out, diagnostic bytes.Buffer
	if status := runPolicyTest(strings.NewReader(""), &out, &diagnostic, []string{"-f", path}); status != 0 {
		t.Fatal(status, out.String(), diagnostic.String())
	}
	if trap.calls.Load() != 0 {
		t.Fatal("offline CLI performed HTTP")
	}
	for _, secret := range []string{"sensitive-literal", "sensitive-resolved-secret", "sensitive-ambient-key", "vault:", "${FIXTURE_SECRET}"} {
		if strings.Contains(out.String()+diagnostic.String(), secret) {
			t.Fatal("report exposed data or secret")
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != "fixtures.json" {
		t.Fatal("CLI wrote files", entries, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != raw {
		t.Fatal("CLI mutated fixture", err)
	}
}

type policyFailedWriter struct{}

func (policyFailedWriter) Write([]byte) (int, error) { return 0, errors.New("blocked output") }

type policyFailedReader struct{}

func (policyFailedReader) Read([]byte) (int, error) { return 0, errors.New("blocked input") }

func TestOfflinePolicyCLIStatuses(t *testing.T) {
	for _, args := range [][]string{{}, {"-f", "-", "extra"}, {"--url", "http://127.0.0.1"}} {
		var out, diagnostic bytes.Buffer
		if status := runPolicyTest(strings.NewReader(minimalPolicyFixture), &out, &diagnostic, args); status != 2 || out.Len() != 0 {
			t.Fatal(args, status, out.String())
		}
	}
	var out, diagnostic bytes.Buffer
	if status := runPolicyTest(strings.NewReader(""), &out, &diagnostic, []string{"--help"}); status != 0 || out.Len() != 0 {
		t.Fatal(status)
	}
	if status := runPolicyTest(policyFailedReader{}, &out, &diagnostic, []string{"-f", "-"}); status != 3 {
		t.Fatal(status)
	}
	if status := runPolicyTest(strings.NewReader(minimalPolicyFixture), policyFailedWriter{}, &diagnostic, []string{"-f", "-"}); status != 3 {
		t.Fatal(status)
	}
	if status := runPolicyTest(strings.NewReader(""), io.Discard, &diagnostic, []string{"-f", filepath.Join(t.TempDir(), "missing.json")}); status != 3 {
		t.Fatal(status)
	}
}

func TestOfflinePolicyDeclaredPrincipalDoesNotCreateAuthz(t *testing.T) {
	a, _ := parsePolicyFixture([]byte(minimalPolicyFixture))
	engine := policy.New(a.engineConfig())
	first := engine.Decide(context.Background(), policy.PolicyInput{Stage: policy.StageToolCall, TenantID: "acme", AgentID: "agent", Connector: "tools", Tool: "read", Principal: "alice", Args: map[string]any{}})
	second := engine.Decide(context.Background(), policy.PolicyInput{Stage: policy.StageToolCall, TenantID: "acme", AgentID: "agent", Connector: "tools", Tool: "read", Principal: "admin", Args: map[string]any{}})
	if first.Effect != second.Effect || first.ReasonCode != second.ReasonCode {
		t.Fatal("principal declarations became authentication")
	}
}

func TestPolicyTestCommandHelper(t *testing.T) {
	if os.Getenv("RUNKITE_POLICY_TEST_HELPER") != "1" {
		return
	}
	path := os.Getenv("RUNKITE_POLICY_TEST_PATH")
	if path == "" {
		path = "-"
	}
	os.Args = []string{"runkite", "policy", "test", "-f", path}
	main()
}

func TestOfflinePolicyRealCommandExitStatuses(t *testing.T) {
	for _, tc := range []struct {
		name, raw, path string
		code            int
	}{
		{name: "pass", raw: minimalPolicyFixture, code: 0},
		{name: "mismatch", raw: strings.Replace(minimalPolicyFixture, `"effect":"deny"`, `"effect":"allow"`, 1), code: 1},
		{name: "invalid", raw: `{"schema_version":2}`, code: 2},
		{name: "io", path: filepath.Join(t.TempDir(), "missing.json"), code: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestPolicyTestCommandHelper$")
			command.Env = append(os.Environ(), "RUNKITE_POLICY_TEST_HELPER=1", "RUNKITE_POLICY_TEST_PATH="+tc.path)
			command.Stdin = strings.NewReader(tc.raw)
			var out, diagnostic bytes.Buffer
			command.Stdout = &out
			command.Stderr = &diagnostic
			err := command.Run()
			code := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				code = exit.ExitCode()
			}
			if code != tc.code {
				t.Fatalf("status=%d want=%d stderr=%s stdout=%s", code, tc.code, diagnostic.String(), out.String())
			}
		})
	}
}
