package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
)

func TestRunSim_SendsSimulationHeader(t *testing.T) {
	restore := setSimPoll(t, time.Millisecond)
	defer restore()

	var sawSim, sawBearer, sawTenant bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/runs") {
			if r.Header.Get(auth.HeaderSimulation) == "true" {
				sawSim = true
			}
			if r.Header.Get("Authorization") == "Bearer admin-key" {
				sawBearer = true
			}
			if r.Header.Get(auth.HeaderTenantID) == "acme" {
				sawTenant = true
			}
			writeJSON(w, map[string]any{"run_id": "run-1", "status": "pending"})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/threads" {
			writeJSON(w, map[string]any{"thread_id": "th-1"})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/runs/run-1" {
			writeJSON(w, map[string]any{"run_id": "run-1", "status": "success"})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	path := writeSimYAML(t, `
agent: payroll
cases:
  - id: small-transfer
    input: {}
`)
	code, out := runSimCapture(t, "-f", path, "--url", srv.URL, "--api-key", "admin-key", "--tenant", "acme", "--timeout", "2s")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !sawSim {
		t.Fatal("create-run missing X-Runkite-Simulation")
	}
	if !sawBearer {
		t.Fatal("create-run missing Authorization")
	}
	if !sawTenant {
		t.Fatal("create-run missing tenant header")
	}
	if !strings.Contains(out, "PASS  small-transfer") || !strings.Contains(out, "1 passed, 0 failed") {
		t.Fatalf("stdout:\n%s", out)
	}
}

func TestRunSim_ForbiddenAdmin(t *testing.T) {
	restore := setSimPoll(t, time.Millisecond)
	defer restore()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/threads" {
			writeJSON(w, map[string]any{"thread_id": "th-1"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/runs") {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"message":     "simulation requires admin",
				"reason_code": "simulation_requires_admin",
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	path := writeSimYAML(t, `
agent: payroll
cases:
  - id: blocked
    input: {}
`)
	code, out := runSimCapture(t, "-f", path, "--url", srv.URL, "--timeout", "2s")
	if code != 1 {
		t.Fatalf("exit %d want 1\n%s", code, out)
	}
	if !strings.Contains(out, "FAIL  blocked") || !strings.Contains(out, "simulation_requires_admin") {
		t.Fatalf("stdout:\n%s", out)
	}
}

func TestRunSim_Timeout(t *testing.T) {
	restore := setSimPoll(t, 5*time.Millisecond)
	defer restore()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/threads" {
			writeJSON(w, map[string]any{"thread_id": "th-1"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/runs") {
			writeJSON(w, map[string]any{"run_id": "run-slow", "status": "pending"})
			return
		}
		if r.URL.Path == "/runs/run-slow" {
			writeJSON(w, map[string]any{"run_id": "run-slow", "status": "pending"})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	path := writeSimYAML(t, `
agent: payroll
cases:
  - id: hung
    input: {}
`)
	code, out := runSimCapture(t, "-f", path, "--url", srv.URL, "--timeout", "150ms")
	if code != 1 {
		t.Fatalf("exit %d want 1\n%s", code, out)
	}
	if !strings.Contains(out, "FAIL  hung") || !strings.Contains(out, "timed out") {
		t.Fatalf("stdout:\n%s", out)
	}
}

func TestRunSim_AuditSQLRequired(t *testing.T) {
	restore := setSimPoll(t, time.Millisecond)
	defer restore()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/threads":
			writeJSON(w, map[string]any{"thread_id": "th-1"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/runs"):
			writeJSON(w, map[string]any{"run_id": "run-1", "status": "pending"})
		case r.URL.Path == "/runs/run-1":
			writeJSON(w, map[string]any{"run_id": "run-1", "status": "error"})
		case strings.HasPrefix(r.URL.Path, "/admin-api/audit-events"):
			w.WriteHeader(http.StatusNotImplemented)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "audit search requires a SQL state backend"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	path := writeSimYAML(t, `
agent: payroll
cases:
  - id: large-transfer
    input: {}
    expect:
      policy_effects:
        - connector: bank
          tool: transfer
          effect: pending
`)
	code, out := runSimCapture(t, "-f", path, "--url", srv.URL, "--timeout", "2s")
	if code != 1 {
		t.Fatalf("exit %d want 1\n%s", code, out)
	}
	if !strings.Contains(out, "audit search needs SQL") {
		t.Fatalf("stdout:\n%s", out)
	}
}

func TestRunSim_PolicyEffectsMatch(t *testing.T) {
	restore := setSimPoll(t, time.Millisecond)
	defer restore()

	events := []models.AuditEvent{
		{Action: policy.StageToolCall, Decision: policy.EffectPending, Connector: "bank", Tool: "transfer", ReasonCode: "predicate_amount"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/threads":
			writeJSON(w, map[string]any{"thread_id": "th-1"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/runs"):
			writeJSON(w, map[string]any{"run_id": "run-1", "status": "pending"})
		case r.URL.Path == "/runs/run-1":
			writeJSON(w, map[string]any{"run_id": "run-1", "status": "error"})
		case strings.HasPrefix(r.URL.Path, "/admin-api/audit-events"):
			writeJSON(w, events)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	path := writeSimYAML(t, `
agent: payroll
cases:
  - id: large-transfer
    input: {}
    expect:
      policy_effects:
        - connector: bank
          tool: transfer
          effect: pending
          reason_code: predicate_amount
`)
	code, out := runSimCapture(t, "-f", path, "--url", srv.URL, "--timeout", "2s")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "PASS  large-transfer") {
		t.Fatalf("stdout:\n%s", out)
	}
}

func setSimPoll(t *testing.T, d time.Duration) func() {
	t.Helper()
	old := simPollInterval
	simPollInterval = d
	return func() { simPollInterval = old }
}

func runSimCapture(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var buf strings.Builder
	code := runSim(&buf, io.Discard, args)
	return code, buf.String()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestParseSimFile_ExamplesPredicates(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	path := filepath.Join(filepath.Dir(thisFile), "..", "examples", "sim", "predicates.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := parseSimFile(path); err != nil {
		t.Fatal(err)
	}
}
