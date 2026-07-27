// Package e2e_test runs the real, unmodified runkite binary and the real
// Python runner as subprocesses against real Postgres and Redis, then
// exercises them over plain HTTP/SSE like any external client would.
//
// This is deliberately black-box: no internal packages are imported, no
// in-process handler wiring is used. The whole point is to catch exactly
// the class of bug that unit/integration tests wired against
// apiServer.Handler() in isolation cannot see -- e.g. the SSE streaming
// regression found when the Prometheus metrics middleware was composed
// with the rest of the stack in cmd/serve.go, or the Docker runner image
// that could not even start. Those were both invisible to the (at the
// time) 145-strong unit/integration suite and were only caught by running
// the real, fully-wired stack end to end.
//
// Requires POSTGRES_DSN and REDIS_URL (see docker-compose.test.yml /
// `make infra-up`). Skips entirely if either is unset, same convention as
// internal/state/postgres and internal/transport/redis.
//
// Run with: make test-e2e
package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	baseURL           string
	repoRoot          string
	runkiteBinaryPath string
	grpcTargetAddr    string // "localhost:<grpcPort>", passed to each runner launch
	configPath        string // path to examples/all_agents/langgraph.json

	runnerMu    sync.Mutex
	runnerCmd   *exec.Cmd
	runnerLog   *syncBuffer
	serveLogRef *syncBuffer
)

// currentRunnerLog returns a snapshot of the currently-running runner
// subprocess's combined stdout/stderr, for debugging test failures.
func currentRunnerLog() string {
	runnerMu.Lock()
	log := runnerLog
	runnerMu.Unlock()
	if log == nil {
		return "(no runner log available)"
	}
	return log.String()
}

// syncBuffer wraps bytes.Buffer with a mutex. Plain bytes.Buffer is not
// safe for concurrent use, but a subprocess's stdout is written to it from
// one goroutine (exec's internal io.Copy) while restartRunner polls
// String() from the test goroutine -- a genuine data race without this.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const (
	httpPort = "2027" // distinct from the default 2026 to avoid clashing with a manually-running dev server
	grpcPort = "50052"
)

// TestMain builds the real binary, starts the real control plane and the
// real Python runner as subprocesses against real Postgres/Redis, runs all
// tests, then tears everything down.
func TestMain(m *testing.M) {
	if os.Getenv("POSTGRES_DSN") == "" || os.Getenv("REDIS_URL") == "" {
		fmt.Println("e2e: POSTGRES_DSN and REDIS_URL not set -- skipping (see `make infra-up` / `make test-e2e`)")
		os.Exit(0)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot = filepath.Join(filepath.Dir(thisFile), "..", "..")
	baseURL = "http://localhost:" + httpPort

	cleanup, err := startStack()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: failed to start stack:", err)
		os.Exit(1)
	}

	code := m.Run()
	cleanup()
	os.Exit(code)
}

func startStack() (cleanup func(), err error) {
	var cleanups []func()
	cleanupAll := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	// 1. Build the real binary fresh (not the dev binary a user might have
	// lying around at repo root -- this proves the current source builds
	// and runs, exactly what a fresh `make build` would produce).
	tmpDir, err := os.MkdirTemp("", "runkite-e2e-*")
	if err != nil {
		return nil, fmt.Errorf("mkdir temp: %w", err)
	}
	cleanups = append(cleanups, func() { os.RemoveAll(tmpDir) })

	runkiteBinaryPath = filepath.Join(tmpDir, "runkite")
	buildCmd := exec.Command("go", "build", "-o", runkiteBinaryPath, "./cmd")
	buildCmd.Dir = repoRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		cleanupAll()
		return nil, fmt.Errorf("go build: %w\n%s", err, out)
	}

	// 2. Start the control plane against real Postgres + Redis.
	configPath = filepath.Join(repoRoot, "examples", "all_agents", "langgraph.json")
	serveCmd := exec.Command(runkiteBinaryPath, "serve", "--config", configPath, "--port", httpPort, "--grpc-port", grpcPort)
	serveCmd.Env = append(os.Environ(),
		"POSTGRES_DSN="+os.Getenv("POSTGRES_DSN"),
		"REDIS_URL="+os.Getenv("REDIS_URL"),
	)
	serveLog := &syncBuffer{}
	serveCmd.Stdout = serveLog
	serveCmd.Stderr = serveLog
	serveLogRef = serveLog
	if err := serveCmd.Start(); err != nil {
		cleanupAll()
		return nil, fmt.Errorf("start control plane: %w", err)
	}
	cleanups = append(cleanups, func() {
		serveCmd.Process.Kill()
		serveCmd.Wait()
	})

	if err := waitForHealth(baseURL+"/health", 20*time.Second); err != nil {
		cleanupAll()
		return nil, fmt.Errorf("control plane never became healthy: %w\n--- control plane output ---\n%s", err, serveLog.String())
	}

	// 3. Start the real Python runner against the same control plane.
	grpcTargetAddr = "localhost:" + grpcPort
	runnerLog, err := launchRunner(configPath)
	if err != nil {
		cleanupAll()
		return nil, fmt.Errorf("start runner: %w", err)
	}
	cleanups = append(cleanups, func() { killRunner() })

	// 4. Wait for all example agents to actually be registered and for the
	// runner to be dispatch-ready (poll agents/search, not just a sleep).
	wantAgents := []string{"echo_agent", "slow_agent", "approval_agent", "react_agent", "store_agent"}
	if err := waitForAgents(wantAgents, 20*time.Second); err != nil {
		cleanupAll()
		return nil, fmt.Errorf("agents never registered: %w\n--- control plane output ---\n%s\n--- runner output ---\n%s",
			err, serveLog.String(), runnerLog.String())
	}
	// The runner registers its gRPC connection and starts long-polling
	// almost immediately after the graphs load; give it a moment to reach
	// "Worker ready" so the very first dispatched job doesn't race a
	// runner that's still importing langgraph.
	time.Sleep(2 * time.Second)

	return cleanupAll, nil
}

// launchRunner starts a fresh Python runner subprocess and records it as
// the current shared runner (see restartRunner). Returns the process's
// combined stdout/stderr buffer (still being written to as the process
// runs) for diagnostics.
func launchRunner(configPath string) (*syncBuffer, error) {
	venvPython := filepath.Join(repoRoot, "python", ".venv", "bin", "python")
	if _, statErr := os.Stat(venvPython); statErr != nil {
		venvPython = "python3" // fall back to whatever's on PATH
	}
	cmd := exec.Command(venvPython, "-m", "runkite_runner",
		"--config", configPath,
		"--grpc-address", grpcTargetAddr,
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(repoRoot, "python"))
	logBuf := &syncBuffer{}
	cmd.Stdout = logBuf
	cmd.Stderr = logBuf
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	runnerMu.Lock()
	runnerCmd = cmd
	runnerLog = logBuf
	runnerMu.Unlock()

	return logBuf, nil
}

// killRunner force-kills whatever runner subprocess is currently tracked,
// if any. Safe to call multiple times.
func killRunner() {
	runnerMu.Lock()
	cmd := runnerCmd
	runnerMu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	cmd.Process.Kill()
	cmd.Wait()
}

// restartRunner kills the current runner subprocess (simulating a crash --
// no graceful shutdown, no chance for any in-memory state to be flushed
// anywhere) and starts a brand new one with a fresh PID and zero shared
// memory with the old process. Used to prove checkpoint state actually
// persisted externally (Postgres), not just in the old process's memory.
func restartRunner(t *testing.T, configPath string) {
	t.Helper()
	killRunner()
	// Wait for the control plane to drop the dead runner's in-flight
	// GetJob long-poll (grpc keepalive ~2s+2s). Without this, a resume
	// enqueued immediately can be stolen by the zombie RPC and lost.
	// Inflight reclaim (6s) is the production safety net; this wait
	// keeps the e2e path deterministic and fast.
	time.Sleep(5 * time.Second)
	logBuf, err := launchRunner(configPath)
	if err != nil {
		t.Fatalf("failed to restart runner: %v", err)
	}
	// There's no HTTP-visible "runner connected" signal (the control plane
	// itself never restarted, so /agents/search tells us nothing new) --
	// wait for the runner's own "Worker ready" log line instead.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logBuf.String(), "Worker ready") {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("restarted runner never became ready; log so far:\n%s", logBuf.String())
}

func waitForHealth(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", url)
}

func waitForAgents(want []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastSeen []string
	for time.Now().Before(deadline) {
		resp, err := http.Post(baseURL+"/agents/search", "application/json", strings.NewReader(`{}`))
		if err == nil {
			var agents []map[string]interface{}
			json.NewDecoder(resp.Body).Decode(&agents)
			resp.Body.Close()
			lastSeen = nil
			for _, a := range agents {
				if id, ok := a["agent_id"].(string); ok {
					lastSeen = append(lastSeen, id)
				}
			}
			if containsAll(lastSeen, want) {
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for agents %v, last saw %v", want, lastSeen)
}

func containsAll(haystack, needles []string) bool {
	set := make(map[string]bool, len(haystack))
	for _, h := range haystack {
		set[h] = true
	}
	for _, n := range needles {
		if !set[n] {
			return false
		}
	}
	return true
}

// --- HTTP helpers ---

// httpClient has a bounded timeout so a genuine hang fails fast with a
// clear error instead of silently eating the whole test's -timeout budget
// (the default http.Post has no timeout at all). 30s comfortably covers
// the slowest legitimate case (slow_agent's ~6s run, GetJob's 30s
// long-poll window) while still failing well before a 120s test timeout.
var httpClient = &http.Client{Timeout: 30 * time.Second}

func postJSON(t *testing.T, path string, body interface{}) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := httpClient.Post(baseURL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		serveLogStr := "(no server log available)"
		if serveLogRef != nil {
			serveLogStr = serveLogRef.String()
		}
		t.Fatalf("POST %s: %v\n--- control plane log ---\n%s\n--- runner log ---\n%s", path, err, serveLogStr, currentRunnerLog())
	}
	return resp
}

func getJSON(t *testing.T, path string, out interface{}) {
	t.Helper()
	resp, err := http.Get(baseURL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode GET %s: %v", path, err)
	}
}

func decodeJSON(t *testing.T, resp *http.Response, out interface{}) {
	t.Helper()
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
}

// sseEvent is one parsed "event: X\ndata: Y" block from an SSE response body.
type sseEvent struct {
	Event string
	Data  map[string]interface{}
}

func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				ev.Event = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				json.Unmarshal([]byte(v), &ev.Data)
			}
		}
		if ev.Event != "" {
			events = append(events, ev)
		}
	}
	return events
}

func eventTypes(events []sseEvent) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.Event
	}
	return types
}
