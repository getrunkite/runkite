// Package adapters_test runs the real runkite binary and a real
// langchain_adapter runner subprocess against real Postgres/Redis, then
// exercises them over plain HTTP like any external client would.
//
// This closes a real gap found in review: the three new framework
// adapters (CrewAI/LlamaIndex/plain LangChain) had unit tests against
// fakes and a manual (not CI-gated) live verification, but no automated
// end-to-end coverage through a real control plane -- unlike the
// LangGraph runner's VG-001/002/003 in ../ (this package's sibling).
// LangChain is the cheapest of the three to cover this way (shared venv,
// no isolated-venv setup needed in CI), so it's the one this closes for.
//
// Separate package (own TestMain, own ports) from ../ rather than
// folding into the existing suite there -- avoids any risk to that
// already-relied-upon harness while proving the same thing for a second
// runner_kind.
//
// Requires POSTGRES_DSN and REDIS_URL (see docker-compose.test.yml /
// `make infra-up`). Skips entirely if either is unset, same convention
// as the sibling e2e package. Picked up automatically by `make
// test-e2e`'s `go test ./test/e2e/...` (no Makefile change needed).
package adapters_test

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	baseURL    string
	repoRoot   string
	configPath string

	runnerMu  sync.Mutex
	runnerLog *syncBuffer
)

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
	httpPort = "2029" // distinct from ../'s 2027 and any manually-running dev server
	grpcPort = "50053"
)

func TestMain(m *testing.M) {
	if os.Getenv("POSTGRES_DSN") == "" || os.Getenv("REDIS_URL") == "" {
		fmt.Println("e2e/adapters: POSTGRES_DSN and REDIS_URL not set -- skipping (see `make infra-up` / `make test-e2e`)")
		os.Exit(0)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot = filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	baseURL = "http://localhost:" + httpPort
	configPath = filepath.Join(filepath.Dir(thisFile), "testdata", "slow_chain", "langgraph.json")

	cleanup, err := startStack()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e/adapters: failed to start stack:", err)
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

	tmpDir, err := os.MkdirTemp("", "runkite-e2e-adapters-*")
	if err != nil {
		return nil, fmt.Errorf("mkdir temp: %w", err)
	}
	cleanups = append(cleanups, func() { os.RemoveAll(tmpDir) })

	binaryPath := filepath.Join(tmpDir, "runkite")
	buildCmd := exec.Command("go", "build", "-o", binaryPath, "./cmd")
	buildCmd.Dir = repoRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		cleanupAll()
		return nil, fmt.Errorf("go build: %w\n%s", err, out)
	}

	serveCmd := exec.Command(binaryPath, "serve", "--config", configPath, "--port", httpPort, "--grpc-port", grpcPort)
	serveCmd.Env = append(os.Environ(),
		"POSTGRES_DSN="+os.Getenv("POSTGRES_DSN"),
		"REDIS_URL="+os.Getenv("REDIS_URL"),
		"RUNKITE_ALLOW_INSECURE_SERVE=1", // see ../e2e_test.go's startStack for why
	)
	serveLog := &syncBuffer{}
	serveCmd.Stdout = serveLog
	serveCmd.Stderr = serveLog
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

	venvPython := filepath.Join(repoRoot, "python", ".venv", "bin", "python")
	if _, statErr := os.Stat(venvPython); statErr != nil {
		venvPython = "python3"
	}
	runnerCmd := exec.Command(venvPython, "-m", "langchain_adapter",
		"--config", configPath,
		"--grpc-address", "localhost:"+grpcPort,
	)
	runnerCmd.Dir = repoRoot
	runnerCmd.Env = append(os.Environ(),
		"PYTHONPATH="+filepath.Join(repoRoot, "python")+string(os.PathListSeparator)+filepath.Join(repoRoot, "python", "adapters"),
	)
	logBuf := &syncBuffer{}
	runnerCmd.Stdout = logBuf
	runnerCmd.Stderr = logBuf
	runnerMu.Lock()
	runnerLog = logBuf
	runnerMu.Unlock()
	if err := runnerCmd.Start(); err != nil {
		cleanupAll()
		return nil, fmt.Errorf("start langchain_adapter runner: %w", err)
	}
	cleanups = append(cleanups, func() {
		runnerCmd.Process.Kill()
		runnerCmd.Wait()
	})

	if err := waitForAgent("slow_langchain_agent", 20*time.Second); err != nil {
		cleanupAll()
		return nil, fmt.Errorf("agent never registered: %w\n--- control plane output ---\n%s\n--- runner output ---\n%s",
			err, serveLog.String(), logBuf.String())
	}
	time.Sleep(2 * time.Second) // let the runner reach "Worker ready" before the first dispatch

	return cleanupAll, nil
}

func currentRunnerLog() string {
	runnerMu.Lock()
	defer runnerMu.Unlock()
	if runnerLog == nil {
		return "(no runner log available)"
	}
	return runnerLog.String()
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

func waitForAgent(agentID string, timeout time.Duration) error {
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
					if id == agentID {
						return nil
					}
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for agent %q, last saw %v", agentID, lastSeen)
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func postJSON(t *testing.T, path string, body interface{}) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := httpClient.Post(baseURL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v\n--- runner log ---\n%s", path, err, currentRunnerLog())
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
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
}
