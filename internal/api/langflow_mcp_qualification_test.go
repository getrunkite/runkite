package api_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/bridge"
	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
	"google.golang.org/grpc"
)

// Opt-in because this runs the unmodified native lfx client and a real Python
// worker. The example supervisor bounds and cleans the whole process group.
func TestLangflowMCPQualification(t *testing.T) {
	if os.Getenv("RUNKITE_TEST_LANGFLOW_MCP") != "1" {
		t.Skip("run examples/langflow_mcp/qualify.py with separate native client and runner interpreters")
	}
	clientPython := os.Getenv("RUNKITE_TEST_LANGFLOW_PYTHON")
	runnerPython := os.Getenv("RUNKITE_TEST_LANGFLOW_RUNNER_PYTHON")
	if !filepath.IsAbs(clientPython) || !filepath.IsAbs(runnerPython) {
		t.Fatal("qualification requires absolute native client and runner interpreter paths")
	}
	env := newTestEnv(t)
	env.srv.Close()
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, auth.EnvPrefix) || strings.HasPrefix(key, auth.TenantsEnvPrefix) {
			t.Setenv(key, "")
		}
	}
	const runnerKey = "langflow-runner-qualification-fixture"
	const clientKey = "langflow-client-qualification-fixture"
	t.Setenv("RUNNER_TOKEN_PYTHON_LANGGRAPH", runnerKey)
	t.Setenv("RUNNER_TENANTS_PYTHON_LANGGRAPH", "default")
	tokens := auth.LoadRunnerTokensFromEnv()
	permissions := []string{"read", "write"}
	profile := "published-v0.9-read-write"
	if os.Getenv("RUNKITE_TEST_LANGFLOW_SCOPED") == "1" {
		permissions = []string{"agents:echo_agent:run", "agents:echo_agent:observe"}
		profile = "unreleased-scoped-source"
	}
	provider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		clientKey: {Name: "langflow-qualification", TenantID: "default", Permissions: permissions},
	})
	ctx := tenant.ScopedContext(context.Background(), "default")
	if err := env.store.UpsertAgent(ctx, &models.Agent{AgentID: "echo_agent", Name: "echo_agent"}); err != nil {
		t.Fatal(err)
	}
	secured := auth.MiddlewareWithOpts(provider, nil, tokens, auth.MiddlewareOpts{StrictPermissions: true, Inflight: env.queue}, env.apiServer.Handler())
	httpServer := httptest.NewServer(secured)
	t.Cleanup(func() { httpServer.CloseClientConnections(); httpServer.Close() })
	bridgeServer := bridge.NewServer(env.queue, env.broker, env.cancel, nil)
	bridgeServer.SetRunLifecycle(env.apiServer.BeginRunAttempt, env.apiServer.CompleteRun)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var dispatches atomic.Int64
	authUnary := bridge.UnaryAuthInterceptor(tokens)
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			return authUnary(ctx, request, info, func(ctx context.Context, request any) (any, error) {
				response, err := handler(ctx, request)
				if result, ok := response.(*pb.GetJobResponse); err == nil && ok && result.HasJob {
					dispatches.Add(1)
				}
				return response, err
			})
		}),
		grpc.StreamInterceptor(bridge.StreamAuthInterceptor(tokens)),
	)
	pb.RegisterRunnerServiceServer(grpcServer, bridgeServer)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	// Reuse the existing qualification's sanitized environment and synchronized
	// process log, without sharing its bridge, receipt logic or client behavior.
	worker := exec.Command(runnerPython, "-m", "runkite_runner", "--config", filepath.Join(repository, "examples/langflow_mcp/qualify_graph.json"), "--grpc-address", listener.Addr().String(), "--http-address", httpServer.URL)
	worker.Env = trinityQualificationEnv(map[string]string{
		"PYTHONPATH": filepath.Join(repository, "python"), "PYTHONDONTWRITEBYTECODE": "1",
		"RUNNER_TOKEN": runnerKey, "RUNKITE_HTTP_URL": httpServer.URL,
	})
	workerLog := &trinityQualificationLog{}
	worker.Stdout, worker.Stderr = workerLog, workerLog
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- worker.Wait() }()
	t.Cleanup(func() {
		_ = worker.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = worker.Process.Kill()
			<-done
		}
	})
	ready := false
	for deadline := time.Now().Add(25 * time.Second); time.Now().Before(deadline); {
		if schema, err := env.store.GetAgentSchema(ctx, "echo_agent"); err == nil && schema.InputSchema != nil {
			ready = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("real worker did not report its schema: %s", workerLog.text())
	}
	state := t.TempDir()
	receiptPath := filepath.Join(state, "native-receipt.json")
	probeCtx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	probe := exec.CommandContext(probeCtx, clientPython, "-B", filepath.Join(repository, "examples/langflow_mcp/qualify_native.py"))
	probe.Env = trinityQualificationEnv(map[string]string{
		"LANGFLOW_CONFIG_DIR": state, "LANGFLOW_KNOWLEDGE_BASES_DIR": filepath.Join(state, "knowledge"),
		"LANGFLOW_DO_NOT_TRACK": "true", "LANGFLOW_LOG_LEVEL": "ERROR", "LANGFLOW_SSRF_ALLOWED_HOSTS": "127.0.0.1",
		"RUNKITE_LANGFLOW_FIXTURE_URL": httpServer.URL, "RUNKITE_LANGFLOW_FIXTURE_KEY": clientKey,
		"RUNKITE_LANGFLOW_FIXTURE_RECEIPT": receiptPath, "PYTHONDONTWRITEBYTECODE": "1",
	})
	if output, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("native Langflow probe failed: %v\n%s\nworker: %s", err, output, workerLog.text())
	}
	runs, err := env.store.SearchRuns(ctx, &models.RunSearchRequest{Limit: 100})
	if err != nil || len(runs) != 2 || dispatches.Load() != 2 {
		t.Fatalf("retry redispatched work: rows=%d assignments=%d error=%v", len(runs), dispatches.Load(), err)
	}
	first, err := env.store.GetRun(ctx, "langflow-qualification-operation")
	if err != nil || first.Status != models.RunStatusSuccess {
		t.Fatalf("native success was not durable: %+v %v", first, err)
	}
	failed, err := env.store.GetRun(ctx, "langflow-error-operation")
	if err != nil || failed.Status != models.RunStatusError {
		t.Fatalf("native tool error was not durable: %+v %v", failed, err)
	}
	data, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(data, &receipt); err != nil || receipt["passed"] != true {
		t.Fatalf("native probe did not produce a valid receipt: %v", err)
	}
	receipt["credential_profile"] = profile
	receipt["managed_runs"], receipt["runner_assignments"] = len(runs), dispatches.Load()
	if output := os.Getenv("RUNKITE_TEST_LANGFLOW_OUTPUT"); output != "" {
		file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := json.NewEncoder(file).Encode(receipt); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("Native Langflow passed: profile=%s managed_runs=%d assignments=%d", profile, len(runs), dispatches.Load())
}
