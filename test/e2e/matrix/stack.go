package matrix

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
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// repoRoot is resolved once from this file's own location -- every other
// e2e package in this repo (../e2e_test.go, ../adapters/main_test.go)
// does the same via runtime.Caller rather than assuming a CWD.
var repoRoot = func() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}()

// sharedBinary builds ./cmd exactly once per test run (not once per
// cell) -- 32 matrix cells rebuilding an identical binary would triple
// the suite's wall time for zero additional coverage.
var sharedBinary = sync.OnceValues(func() (string, error) {
	tmpDir, err := os.MkdirTemp("", "runkite-matrix-bin-*")
	if err != nil {
		return "", fmt.Errorf("mkdir temp: %w", err)
	}
	binPath := filepath.Join(tmpDir, "runkite")
	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd")
	buildCmd.Dir = repoRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %w\n%s", err, out)
	}
	return binPath, nil
})

// portCounter hands out a fresh HTTP/gRPC port pair per cell so
// sequential cells never race a predecessor's TIME_WAIT sockets --
// cheaper and more robust than a fixed pair reused across 30+ cells.
var portCounter atomic.Int32

func init() {
	portCounter.Store(2100)
}

func nextPortPair() (httpPort, grpcPort string) {
	base := int(portCounter.Add(2))
	return fmt.Sprintf("%d", base), fmt.Sprintf("%d", base+10000)
}

func lookupEnv(name string) string { return os.Getenv(name) }

// syncBuffer wraps bytes.Buffer with a mutex -- a subprocess's stdout is
// written to it from exec's internal io.Copy goroutine while the test
// goroutine reads String() for diagnostics, a genuine data race
// otherwise. Identical convention to ../e2e_test.go's own syncBuffer.
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

// LaunchedProcess is what a RunnerSpec.Launch func hands back to
// startCell: the running command plus its captured log, so a cell's
// diagnostics() can print the runner's own output on failure without
// startCell needing to know anything about how that runner was invoked.
type LaunchedProcess struct {
	Cmd *exec.Cmd
	Log *syncBuffer
}

// launchPythonModule returns a Launch func for `python -m <moduleName>`,
// covering every Python runner this matrix exercises: the LangGraph
// runner itself (moduleName="runkite_runner", extraPathRel="",
// venvRelPath="python/.venv") and each framework adapter
// (moduleName="crewai_adapter" etc., extraPathRel="python/adapters" so
// the adapter package -- itself a sibling of runkite_runner, not nested
// under it -- resolves).
//
// venvRelPath matters because it is NOT always "python/.venv": CrewAI,
// LlamaIndex, and AutoGen each pull in heavy, occasionally
// version-conflicting dependencies, so (mirroring `make test-adapters`)
// they each get their own isolated venv at
// python/adapters/<name>/.venv, separate from the shared venv every
// other Python runner uses. Using the wrong venv here doesn't fail
// loudly with "wrong package version" -- it fails with
// ModuleNotFoundError for the framework package entirely, since it was
// never installed in the shared venv in the first place.
func launchPythonModule(moduleName, extraPathRel, venvRelPath string) func(launchEnv) (LaunchedProcess, error) {
	return func(env launchEnv) (LaunchedProcess, error) {
		venvPython := filepath.Join(env.RepoRoot, venvRelPath, "bin", "python")
		if _, err := os.Stat(venvPython); err != nil {
			return LaunchedProcess{}, fmt.Errorf(
				"isolated venv not set up at %s (see python/adapters/*/README.md, or `make test-adapters`'s own skip-if-missing convention): %w",
				venvPython, err)
		}
		cmd := exec.Command(venvPython, "-m", moduleName,
			"--config", env.ConfigPath,
			"--grpc-address", env.GRPCAddr,
		)
		cmd.Dir = env.RepoRoot
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		pythonPath := filepath.Join(env.RepoRoot, "python")
		if extraPathRel != "" {
			pythonPath += string(os.PathListSeparator) + filepath.Join(env.RepoRoot, extraPathRel)
		}
		cmd.Env = append(os.Environ(), "PYTHONPATH="+pythonPath)
		logBuf := &syncBuffer{}
		cmd.Stdout = logBuf
		cmd.Stderr = logBuf
		if err := cmd.Start(); err != nil {
			return LaunchedProcess{}, err
		}
		return LaunchedProcess{Cmd: cmd, Log: logBuf}, nil
	}
}

// launchTypeScriptRunner starts the TS runner via its local tsx dev
// binary (typescript/runkite-runner/node_modules/.bin/tsx src/cli.ts) --
// the same "npx runkite-runner" CLI contract documented in cli.ts's own
// header comment, just invoked from source instead of a published
// package so this matrix always exercises current source, not whatever
// was last published to npm.
func launchTypeScriptRunner(env launchEnv) (LaunchedProcess, error) {
	tsRunnerDir := filepath.Join(env.RepoRoot, "typescript", "runkite-runner")
	tsx := filepath.Join(tsRunnerDir, "node_modules", ".bin", "tsx")
	if _, err := os.Stat(tsx); err != nil {
		return LaunchedProcess{}, fmt.Errorf("tsx not found at %s (run `npm install` in typescript/runkite-runner): %w", tsx, err)
	}
	cmd := exec.Command(tsx, "src/cli.ts",
		"--config", env.ConfigPath,
		"--grpc-address", env.GRPCAddr,
	)
	cmd.Dir = tsRunnerDir
	cmd.Env = os.Environ()
	// tsx spawns a child Node process to run the transformed TS; killing
	// only the tsx wrapper process leaves that child alive holding the
	// stdout/stderr pipes open, which hangs Cmd.Wait() forever during
	// cleanup. Setpgid + killing the whole process group (see
	// killProcessGroup) takes the child down with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logBuf := &syncBuffer{}
	cmd.Stdout = logBuf
	cmd.Stderr = logBuf
	if err := cmd.Start(); err != nil {
		return LaunchedProcess{}, err
	}
	return LaunchedProcess{Cmd: cmd, Log: logBuf}, nil
}

// cell is one running (backend, runner) combination: a live control
// plane + runner subprocess pair, torn down via t.Cleanup when the
// subtest that created it finishes. Every scenario function in
// scenarios.go operates on a *cell.
type cell struct {
	t       *testing.T
	baseURL string
	runner  RunnerSpec
	backend BackendSpec

	serveLog  *syncBuffer
	runnerLog *syncBuffer
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// startCell builds (once) and starts a fresh control plane against the
// given backend, launches the given runner against it, and waits for
// the runner's agent(s) to actually register -- polling, not sleeping,
// so a slow CI runner doesn't produce a flaky false failure the way a
// fixed sleep would. Skips the subtest (not a failure) if the backend's
// required env vars aren't set, matching every other conformance suite
// in this repo's skip-if-infra-unavailable convention.
func startCell(t *testing.T, backend BackendSpec, runner RunnerSpec) *cell {
	t.Helper()

	backendEnv, skipReason := backend.Env()
	if skipReason != "" {
		t.Skipf("backend %s unavailable: %s", backend.Name, skipReason)
	}
	if runner.RequiredVenvRelPath != "" {
		venvPython := filepath.Join(repoRoot, runner.RequiredVenvRelPath, "bin", "python")
		if _, err := os.Stat(venvPython); err != nil {
			t.Skipf("runner %s unavailable: isolated venv not set up at %s (see python/adapters/*/README.md)",
				runner.Name, runner.RequiredVenvRelPath)
		}
	}

	binPath, err := sharedBinary()
	if err != nil {
		t.Fatalf("build shared runkite binary: %v", err)
	}

	httpPort, grpcPort := nextPortPair()
	baseURL := "http://localhost:" + httpPort
	configPath := filepath.Join(repoRoot, runner.ConfigRelPath)

	serveCmd := exec.Command(binPath, "serve", "--config", configPath, "--port", httpPort, "--grpc-port", grpcPort)
	// This matrix intentionally runs every cell with no auth/runner-tokens
	// configured (isolating the framework x backend integration seam it
	// actually tests) -- checkProductionAdmission (cmd/serve.go) would
	// otherwise refuse to start `serve` with that posture, and some
	// cells (sqlite_inprocess) also have no durable/shared backend by
	// design.
	serveCmd.Env = append(append(os.Environ(), backendEnv...), "RUNKITE_ALLOW_INSECURE_SERVE=1")
	serveCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	serveLog := &syncBuffer{}
	serveCmd.Stdout = serveLog
	serveCmd.Stderr = serveLog
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start control plane: %v", err)
	}
	t.Cleanup(func() { killProcessGroup(serveCmd) })

	if err := waitForHealth(baseURL+"/health", 20*time.Second); err != nil {
		t.Fatalf("control plane never became healthy: %v\n--- control plane output ---\n%s", err, serveLog.String())
	}

	proc, err := runner.Launch(launchEnv{RepoRoot: repoRoot, ConfigPath: configPath, GRPCAddr: "localhost:" + grpcPort})
	if err != nil {
		t.Fatalf("start %s runner: %v", runner.Name, err)
	}
	t.Cleanup(func() { killProcessGroup(proc.Cmd) })

	if err := waitForAgent(baseURL, runner.AgentID, runner.StartupTimeout); err != nil {
		t.Fatalf("agent never registered: %v\n--- control plane output ---\n%s\n--- runner output ---\n%s",
			err, serveLog.String(), proc.Log.String())
	}
	// Let the runner reach its dispatch-ready poll loop before the first
	// job is created -- same fixed grace period ../e2e_test.go and
	// ../adapters/main_test.go both use after agent registration.
	time.Sleep(2 * time.Second)

	return &cell{
		t:         t,
		baseURL:   baseURL,
		runner:    runner,
		backend:   backend,
		serveLog:  serveLog,
		runnerLog: proc.Log,
	}
}

// killProcessGroup sends SIGKILL to the whole process group cmd was
// started in (see the Setpgid: true SysProcAttr set on every launched
// command in this package) rather than just cmd.Process itself --
// needed because tsx (and potentially other launchers in the future)
// spawn a child process that survives killing only the direct child,
// leaving stdout/stderr pipes open and hanging Cmd.Wait() forever.
// Best-effort: if the process already exited, Kill/Wait errors are
// expected and ignored, matching every sibling e2e package's cleanup.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	} else {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
}

func (c *cell) diagnostics() string {
	return fmt.Sprintf("--- control plane log (%s/%s) ---\n%s\n--- runner log ---\n%s",
		c.backend.Name, c.runner.Name, c.serveLog.String(), c.runnerLog.String())
}

func (c *cell) postJSON(t *testing.T, path string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := httpClient.Post(c.baseURL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v\n%s", path, err, c.diagnostics())
	}
	return resp
}

func (c *cell) getJSON(t *testing.T, path string, out any) {
	t.Helper()
	resp, err := http.Get(c.baseURL + path)
	if err != nil {
		t.Fatalf("GET %s: %v\n%s", path, err, c.diagnostics())
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode GET %s: %v\n%s", path, err, c.diagnostics())
	}
}

func (c *cell) decodeJSON(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode response body: %v\n%s", err, c.diagnostics())
	}
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

func waitForAgent(baseURL, agentID string, timeout time.Duration) error {
	// Filters server-side by name (substring match, see
	// internal/state/*'s SearchAgents) and raises the limit to the
	// server's own max (100) instead of relying on the unfiltered
	// default page of 10 -- an unfiltered "{}" search only happened to
	// work here because every AgentID this matrix waits for sorted into
	// that first page by luck of the alphabet; a name filter makes this
	// correct regardless of how many agents accumulate in a shared
	// backend (see cleanupSharedBackends's doc comment for why that
	// accumulation is a real, live-proven failure mode this harness
	// itself caused for ../e2e_test.go and ../adapters/main_test.go).
	body := fmt.Sprintf(`{"name":%q,"limit":100}`, agentID)
	deadline := time.Now().Add(timeout)
	var lastSeen []string
	for time.Now().Before(deadline) {
		resp, err := http.Post(baseURL+"/agents/search", "application/json", strings.NewReader(body))
		if err == nil {
			var agents []map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&agents)
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
