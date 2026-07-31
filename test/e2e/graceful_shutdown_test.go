package e2e_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestGracefulShutdown_DrainsInFlightRunBeforeExiting proves the fix in
// cmd/serve.go: SIGTERM must not abort an in-flight request. It spins up
// its own dedicated control-plane + runner pair (distinct ports from the
// package's shared TestMain stack, same Postgres/Redis) specifically
// because it needs to kill that control-plane process, which would break
// every other test in this package if it shared TestMain's instance.
//
// Before this fix, the only way this process ever stopped was a bare
// http.ListenAndServe returning (never, in practice) or a signal handler
// that flushed telemetry and called os.Exit(0) immediately -- with no
// idea whether an SSE/wait request was still being served. This test
// starts a slow_agent run (~6s), sends SIGTERM about a third of the way
// through it, and asserts both that the run's own HTTP response still
// completes with status "success" and that the process exits cleanly
// within the 15s shutdown budget (see cmd/serve.go's shutdownCtx).
func TestGracefulShutdown_DrainsInFlightRunBeforeExiting(t *testing.T) {
	if runkiteBinaryPath == "" {
		t.Skip("runkite binary not built (TestMain skipped -- see POSTGRES_DSN/REDIS_URL)")
	}

	// Distinct from every other e2e-package port in use (../'s 2027,
	// adapters/'s 2029) -- these packages' TestMains run as separate
	// binaries and Go runs different packages' tests in parallel by
	// default, so a shared port here would flake on that race.
	const httpPort = "2030"
	const grpcPort = "50055"

	serveCmd := exec.Command(runkiteBinaryPath, "serve", "--config", configPath, "--port", httpPort, "--grpc-port", grpcPort)
	serveCmd.Env = append(os.Environ(),
		"POSTGRES_DSN="+os.Getenv("POSTGRES_DSN"),
		"REDIS_URL="+os.Getenv("REDIS_URL"),
	)
	serveLog := &syncBuffer{}
	serveCmd.Stdout = serveLog
	serveCmd.Stderr = serveLog
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start dedicated control plane: %v", err)
	}
	defer func() {
		if serveCmd.ProcessState == nil { // still running -- cleanup on an early failure
			serveCmd.Process.Kill()
			serveCmd.Wait()
		}
	}()

	dedicatedBaseURL := "http://localhost:" + httpPort
	if err := waitForHealth(dedicatedBaseURL+"/health", 20*time.Second); err != nil {
		t.Fatalf("dedicated control plane never became healthy: %v\n--- log ---\n%s", err, serveLog.String())
	}

	venvPython := repoRoot + "/python/.venv/bin/python"
	if _, statErr := os.Stat(venvPython); statErr != nil {
		venvPython = "python3"
	}
	runnerCmd := exec.Command(venvPython, "-m", "runkite_runner",
		"--config", configPath,
		"--grpc-address", "localhost:"+grpcPort,
	)
	runnerCmd.Dir = repoRoot
	runnerCmd.Env = append(os.Environ(), "PYTHONPATH="+repoRoot+"/python")
	runnerLog := &syncBuffer{}
	runnerCmd.Stdout = runnerLog
	runnerCmd.Stderr = runnerLog
	if err := runnerCmd.Start(); err != nil {
		t.Fatalf("start dedicated runner: %v", err)
	}
	defer func() {
		runnerCmd.Process.Kill()
		runnerCmd.Wait()
	}()

	// Poll for the runner registering slow_agent against THIS dedicated
	// control plane, not the shared one -- reuses waitForAgents' retry
	// shape but against a different base URL, so it's inlined here.
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := httpClient.Post(dedicatedBaseURL+"/agents/search", "application/json", strings.NewReader(`{}`))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("dedicated stack never became ready\n--- server log ---\n%s\n--- runner log ---\n%s", serveLog.String(), runnerLog.String())
		}
		time.Sleep(300 * time.Millisecond)
	}
	time.Sleep(2 * time.Second) // runner's own long-poll registration, same margin as TestMain

	type waitResult struct {
		Run map[string]interface{} `json:"run"`
	}
	resultCh := make(chan waitResult, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := httpClient.Post(dedicatedBaseURL+"/threads/shutdown-t1/runs/wait", "application/json",
			strings.NewReader(`{"agent_id":"slow_agent","input":{"messages":[{"role":"user","content":"go"}],"step":0}}`))
		if err != nil {
			errCh <- err
			return
		}
		defer resp.Body.Close()
		var wr waitResult
		if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
			errCh <- err
			return
		}
		resultCh <- wr
	}()

	// slow_agent is ~6s (3x2s steps); land the signal mid-step-1, well
	// before natural completion, so a broken drain would be caught
	// (either the response never arrives, or it arrives truncated/error
	// instead of "success").
	time.Sleep(2 * time.Second)

	shutdownStart := time.Now()
	if err := serveCmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	var wr waitResult
	select {
	case wr = <-resultCh:
	case err := <-errCh:
		t.Fatalf("in-flight run request failed instead of draining: %v\n--- server log ---\n%s", err, serveLog.String())
	case <-time.After(20 * time.Second):
		t.Fatalf("in-flight run request never completed after SIGTERM\n--- server log ---\n%s", serveLog.String())
	}
	if status, _ := wr.Run["status"].(string); status != "success" {
		t.Fatalf("expected in-flight run to complete with status success despite SIGTERM, got %v\n--- server log ---\n%s", wr.Run, serveLog.String())
	}

	if err := serveCmd.Wait(); err != nil {
		t.Fatalf("control plane did not exit cleanly after SIGTERM: %v\n--- server log ---\n%s", err, serveLog.String())
	}
	elapsed := time.Since(shutdownStart)
	// 15s shutdown budget (cmd/serve.go) + generous margin for process
	// teardown itself, not a tight timing assertion.
	if elapsed > 18*time.Second {
		t.Fatalf("shutdown took %v, expected well under the 15s budget in cmd/serve.go", elapsed)
	}
	t.Logf("shutdown completed in %v after SIGTERM (in-flight run drained to success)", elapsed)
}
