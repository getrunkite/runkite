package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSimFile_UnknownKeyFails(t *testing.T) {
	path := writeSimYAML(t, `
agent: payroll
cases:
  - id: small
    wait: true
    input:
      messages: []
`)
	_, err := parseSimFile(path)
	if err == nil {
		t.Fatal("expected unknown key to fail")
	}
	if !strings.Contains(err.Error(), "wait") && !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want unknown-field error, got %v", err)
	}
}

func TestParseSimFile_UnknownExpectKeyFails(t *testing.T) {
	path := writeSimYAML(t, `
agent: payroll
cases:
  - id: small
    input: {}
    expect:
      policy_effects: []
      foo: 1
`)
	_, err := parseSimFile(path)
	if err == nil {
		t.Fatal("expected unknown expect key to fail")
	}
}

func TestParseSimFile_MissingAgentFails(t *testing.T) {
	path := writeSimYAML(t, `
cases:
  - id: small
    input: {}
`)
	_, err := parseSimFile(path)
	if err == nil {
		t.Fatal("expected missing agent to fail")
	}
}

func TestParseSimFile_OK(t *testing.T) {
	path := writeSimYAML(t, `
agent: payroll
cases:
  - id: small-transfer
    input:
      messages:
        - role: user
          content: send 40
    expect:
      policy_effects: []
  - id: large-transfer
    agent: payroll
    input:
      messages:
        - role: user
          content: send 5000
    expect:
      policy_effects:
        - connector: bank
          tool: transfer
          effect: pending
          reason_code: predicate_amount
`)
	doc, err := parseSimFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) != 2 {
		t.Fatalf("cases = %d", len(doc.Cases))
	}
	if doc.Cases[1].Expect == nil || doc.Cases[1].Expect.PolicyEffects == nil {
		t.Fatal("expected policy_effects on large-transfer")
	}
	got := (*doc.Cases[1].Expect.PolicyEffects)[0]
	if got.ReasonCode != "predicate_amount" {
		t.Fatalf("reason_code = %q", got.ReasonCode)
	}
}

func writeSimYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fix.yaml")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(body)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
