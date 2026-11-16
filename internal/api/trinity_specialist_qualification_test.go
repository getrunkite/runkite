package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/bridge"
	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
)

const (
	trinityClientFixture = "trinity-client-qualification-fixture"
	trinityRunnerFixture = "trinity-runner-qualification-fixture"
)

type trinityQualificationLog struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *trinityQualificationLog) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(data)
}

func (b *trinityQualificationLog) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

type trinityQualificationFixture struct {
	env               *testEnv
	http              *httptest.Server
	python            string
	binary            string
	launcher          string
	dispatches        atomic.Int64
	toolCalls         atomic.Int64
	receiptBeforeSend atomic.Bool
	stateMu           sync.Mutex
	stateDirectories  []string
}

func trinityQualificationEnv(extra map[string]string) []string {
	environment := []string{}
	for _, key := range []string{"PATH", "TMPDIR", "LANG", "SYSTEMROOT"} {
		if value, ok := os.LookupEnv(key); ok {
			environment = append(environment, key+"="+value)
		}
	}
	for key, value := range extra {
		environment = append(environment, key+"="+value)
	}
	return environment
}

func newTrinityQualificationFixture(t *testing.T, binary, python string) *trinityQualificationFixture {
	t.Helper()
	env := newTestEnv(t)
	env.srv.Close()
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &trinityQualificationFixture{
		env: env, python: python, binary: binary,
		launcher: filepath.Join(repository, "examples", "trinity_specialist_mcp", "launch.py"),
	}
	// This test runs alone and never reads ambient runner credentials. The
	// fixed fixture token is valid only on these ephemeral loopback listeners.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, auth.EnvPrefix) || strings.HasPrefix(key, auth.TenantsEnvPrefix) {
			t.Setenv(key, "")
		}
	}
	t.Setenv("RUNNER_TOKEN_PYTHON_LANGGRAPH", trinityRunnerFixture)
	t.Setenv("RUNNER_TENANTS_PYTHON_LANGGRAPH", "default")
	runnerTokens := auth.LoadRunnerTokensFromEnv()
	provider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		trinityClientFixture: {Name: "trinity-qualification", TenantID: "default", Permissions: []string{"read", "write"}},
	})
	ctx := tenant.ScopedContext(context.Background(), "default")
	if err := env.store.UpsertAgent(ctx, &models.Agent{AgentID: "echo_agent", Name: "echo_agent"}); err != nil {
		t.Fatal(err)
	}
	secured := auth.MiddlewareWithOpts(provider, nil, runnerTokens, auth.MiddlewareOpts{StrictPermissions: true, Inflight: env.queue}, env.apiServer.Handler())
	fixture.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/mcp" {
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				http.Error(w, "qualification request read failed", 400)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var call struct {
				Method string `json:"method"`
				Params struct {
					Arguments struct {
						RunID string `json:"run_id"`
					} `json:"arguments"`
				} `json:"params"`
			}
			if json.Unmarshal(body, &call) == nil && call.Method == "tools/call" {
				fixture.toolCalls.Add(1)
				if call.Params.Arguments.RunID == "" || !fixture.hasSavedRunReceipt(call.Params.Arguments.RunID) {
					http.Error(w, "no persisted receipt before dispatch", 500)
					return
				}
				fixture.receiptBeforeSend.Store(true)
			}
		}
		secured.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { fixture.http.CloseClientConnections(); fixture.http.Close() })
	bridgeServer := bridge.NewServer(env.queue, env.broker, env.cancel, nil)
	bridgeServer.SetRunLifecycle(env.apiServer.BeginRunAttempt, env.apiServer.CompleteRun)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	authUnary := bridge.UnaryAuthInterceptor(runnerTokens)
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			return authUnary(ctx, request, info, func(ctx context.Context, request any) (any, error) {
				response, err := handler(ctx, request)
				if result, ok := response.(*pb.GetJobResponse); err == nil && ok && result.HasJob {
					fixture.dispatches.Add(1)
				}
				return response, err
			})
		}),
		grpc.StreamInterceptor(bridge.StreamAuthInterceptor(runnerTokens)),
	)
	pb.RegisterRunnerServiceServer(grpcServer, bridgeServer)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	worker := exec.Command(python, "-m", "runkite_runner", "--config", filepath.Join(repository, "examples", "trinity_specialist_mcp", "qualify_graph.json"), "--grpc-address", listener.Addr().String(), "--http-address", fixture.http.URL)
	worker.Env = trinityQualificationEnv(map[string]string{
		"PYTHONPATH": filepath.Join(repository, "python"), "PYTHONUNBUFFERED": "1",
		"RUNNER_TOKEN": trinityRunnerFixture, "RUNKITE_HTTP_URL": fixture.http.URL,
	})
	workerLog := &trinityQualificationLog{}
	worker.Stdout, worker.Stderr = workerLog, workerLog
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Wait() }()
	t.Cleanup(func() {
		_ = worker.Process.Signal(os.Interrupt)
		select {
		case <-workerDone:
		case <-time.After(5 * time.Second):
			_ = worker.Process.Kill()
			<-workerDone
		}
	})
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if schema, err := env.store.GetAgentSchema(ctx, "echo_agent"); err == nil && schema.InputSchema != nil {
			return fixture
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("real Python worker did not report its schema: %s", workerLog.text())
	return nil
}

func (f *trinityQualificationFixture) hasSavedRunReceipt(runID string) bool {
	f.stateMu.Lock()
	directories := append([]string(nil), f.stateDirectories...)
	f.stateMu.Unlock()
	for _, directory := range directories {
		found := false
		_ = filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err == nil && bytes.Contains(data, []byte(runID)) && json.Valid(data) {
				found = true
			}
			return nil
		})
		if found {
			return true
		}
	}
	return false
}

func (f *trinityQualificationFixture) state(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	f.stateMu.Lock()
	f.stateDirectories = append(f.stateDirectories, directory)
	f.stateMu.Unlock()
	return directory
}

func (f *trinityQualificationFixture) connect(ctx context.Context, t *testing.T, origin, agent, state, key string, launcher bool) (*mcp.ClientSession, error) {
	t.Helper()
	command := exec.Command(f.binary)
	if launcher {
		bundle := t.TempDir()
		for source, name := range map[string]string{f.binary: "runkite-specialist-mcp", f.launcher: "launch.py"} {
			data, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bundle, name), data, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		command = exec.Command(f.python, filepath.Join(bundle, "launch.py"))
		command.Dir = bundle
	}
	command.Env = trinityQualificationEnv(map[string]string{
		"RUNKITE_BRIDGE_URL": origin, "RUNKITE_BRIDGE_AGENT": agent,
		"RUNKITE_BRIDGE_PROFILE": "qualified-specialist", "RUNKITE_BRIDGE_STATE": state,
		"RUNKITE_API_KEY": key, "RUNKITE_BRIDGE_LOCAL_PROOF": "1",
	})
	command.Stderr = &trinityQualificationLog{}
	client := mcp.NewClient(&mcp.Implementation{Name: "trinity-stdio-qualification", Version: "1"}, nil)
	return client.Connect(ctx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
}

type trinityQualificationHandle struct {
	OperationID string `json:"operation_id"`
	RunID       string `json:"run_id"`
	ThreadID    string `json:"thread_id"`
	Outcome     string `json:"outcome"`
}

func trinityQualificationCall(t *testing.T, ctx context.Context, session *mcp.ClientSession, operation, thread, message string) (*mcp.CallToolResult, trinityQualificationHandle) {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "invoke_specialist", Arguments: map[string]any{"operation_id": operation, "thread_id": thread, "message": message}})
	if err != nil {
		t.Fatalf("specialist protocol request failed: %v", err)
	}
	if len(result.Content) == 0 {
		t.Fatal("specialist returned no handle content")
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("specialist handle was %T", result.Content[0])
	}
	var handle trinityQualificationHandle
	if err := json.Unmarshal([]byte(text.Text), &handle); err != nil {
		t.Fatalf("specialist handle was not JSON: %v", err)
	}
	return result, handle
}

func trinityQualificationReply(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) != 2 {
		t.Fatalf("expected bridge metadata followed by the plane's one text result, found %d content items", len(result.Content))
	}
	text, ok := result.Content[1].(*mcp.TextContent)
	if !ok {
		t.Fatalf("upstream specialist reply was %T", result.Content[1])
	}
	return text.Text
}

func (f *trinityQualificationFixture) assertRuns(t *testing.T, expected int) {
	t.Helper()
	rows, err := f.env.store.SearchRuns(tenant.ScopedContext(context.Background(), "default"), &models.RunSearchRequest{Limit: 100})
	if err != nil || len(rows) != expected {
		t.Fatalf("expected %d managed runs, found %d: %v", expected, len(rows), err)
	}
	if got := int(f.dispatches.Load()); got != expected {
		t.Fatalf("expected %d real runner assignments, found %d", expected, got)
	}
}

// TestTrinitySpecialistQualification is opt-in because it starts a real Python
// worker and the separately built example executable. It calls no model service.
func TestTrinitySpecialistQualification(t *testing.T) {
	if os.Getenv("RUNKITE_TEST_TRINITY") != "1" {
		t.Skip("run examples/trinity_specialist_mcp/qualify.py for the real-worker qualification")
	}
	binary := os.Getenv("RUNKITE_TEST_TRINITY_BRIDGE_BINARY")
	python := os.Getenv("RUNKITE_TEST_TRINITY_PYTHON")
	if !filepath.IsAbs(binary) || !filepath.IsAbs(python) {
		t.Fatal("qualification requires absolute bridge and Python executable paths")
	}
	fixture := newTrinityQualificationFixture(t, binary, python)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	state := fixture.state(t)
	session, err := fixture.connect(ctx, t, fixture.http.URL, "echo_agent", state, trinityClientFixture, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "invoke_specialist" {
		t.Fatalf("stdio discovery did not expose the single bridge tool: %+v %v", tools, err)
	}
	firstMessage := "Qualification conversation one"
	result, first := trinityQualificationCall(t, ctx, session, "qualification-turn-one", "qualification-conversation", firstMessage)
	if result.IsError || first.Outcome != "success" || first.RunID == "" || first.ThreadID != "qualification-conversation" {
		t.Fatalf("first invocation did not succeed: %+v error=%v", first, result.IsError)
	}
	firstReply := trinityQualificationReply(t, result)
	if firstReply != firstMessage {
		t.Fatal("initial MCP call did not return the real echo agent's text projection")
	}
	if !fixture.receiptBeforeSend.Load() {
		t.Fatal("no durable operation receipt was observed before first upstream tool dispatch")
	}
	fixture.assertRuns(t, 1)
	if fixture.toolCalls.Load() != 1 {
		t.Fatal("first invocation must make one upstream MCP tool call")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	session, err = fixture.connect(ctx, t, fixture.http.URL, "echo_agent", state, trinityClientFixture, true)
	if err != nil {
		t.Fatal(err)
	}
	result, repeated := trinityQualificationCall(t, ctx, session, "qualification-turn-one", "qualification-conversation", firstMessage)
	if result.IsError || repeated.RunID != first.RunID || repeated.Outcome != "success" {
		t.Fatalf("caller restart changed the operation: %+v", repeated)
	}
	if trinityQualificationReply(t, result) != firstReply {
		t.Fatal("retained retry changed the original MCP text projection")
	}
	fixture.assertRuns(t, 1)
	if fixture.toolCalls.Load() != 1 {
		t.Fatal("caller restart must observe durable HTTP state without another MCP tool call")
	}
	for _, conflict := range [][2]string{{"qualification-conversation", "Changed message"}, {"different-thread", firstMessage}} {
		result, rejected := trinityQualificationCall(t, ctx, session, "qualification-turn-one", conflict[0], conflict[1])
		if !result.IsError || rejected.Outcome != "rejected" {
			t.Fatalf("conflicting proposal was accepted: %+v", rejected)
		}
	}
	fixture.assertRuns(t, 1)
	secondMessage := "Qualification conversation two"
	result, second := trinityQualificationCall(t, ctx, session, "qualification-turn-two", "qualification-conversation", secondMessage)
	if result.IsError || second.Outcome != "success" || second.RunID == first.RunID {
		t.Fatalf("second distinct turn did not create one new run: %+v", second)
	}
	if trinityQualificationReply(t, result) != secondMessage {
		t.Fatal("second turn did not expose its own assistant text")
	}
	fixture.assertRuns(t, 2)
	result, oldTurn := trinityQualificationCall(t, ctx, session, "qualification-turn-one", "qualification-conversation", firstMessage)
	if result.IsError || oldTurn.RunID != first.RunID || trinityQualificationReply(t, result) != firstReply {
		t.Fatal("observing an earlier operation returned newer thread output")
	}
	fixture.assertRuns(t, 2)
	if fixture.toolCalls.Load() != 2 {
		t.Fatal("observing an earlier turn redispatched upstream MCP")
	}
	thread, err := fixture.env.store.GetThread(tenant.ScopedContext(ctx, "default"), first.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	conversation, _ := json.Marshal(thread.Values)
	if !bytes.Contains(conversation, []byte(firstMessage)) || !bytes.Contains(conversation, []byte(secondMessage)) {
		t.Fatal("real proxy checkpoints did not preserve both conversation turns")
	}
	result, failed := trinityQualificationCall(t, ctx, session, "qualification-error", "qualification-error-thread", "__runkite_qualification_error__")
	if !result.IsError || failed.Outcome != "tool_error" {
		t.Fatalf("terminal error was not reported as a stable tool error: %+v", failed)
	}
	result, failureRetry := trinityQualificationCall(t, ctx, session, "qualification-error", "qualification-error-thread", "__runkite_qualification_error__")
	if !result.IsError || failureRetry.RunID != failed.RunID || failureRetry.Outcome != "tool_error" {
		t.Fatalf("terminal error retry created another operation: %+v", failureRetry)
	}
	fixture.assertRuns(t, 3)
	if fixture.toolCalls.Load() != 3 {
		t.Fatal("terminal error retry made another upstream MCP tool call")
	}
	if run, err := fixture.env.store.GetRun(tenant.ScopedContext(ctx, "default"), failed.RunID); err != nil || run.Status != models.RunStatusError {
		t.Fatalf("the real worker's terminal failure was not durable: %v", err)
	}

	var dropDelivery atomic.Bool
	dropDelivery.Store(true)
	var rejectPreflight atomic.Bool
	var rejectWait atomic.Bool
	var waitRejected atomic.Int64
	var dropped atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rejectPreflight.Load() && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/runs/") {
			http.Error(w, "qualification: claimed run unavailable", http.StatusNotFound)
			return
		}
		if rejectWait.Load() && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/runs/") && strings.HasSuffix(r.URL.Path, "/wait") {
			waitRejected.Add(1)
			http.Error(w, "qualification: retained run vanished after lookup", http.StatusNotFound)
			return
		}
		if dropDelivery.Load() && r.Method == http.MethodPost && r.URL.Path == "/mcp" {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			r.Body = io.NopCloser(bytes.NewReader(body))
			var request struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(body, &request) == nil && request.Method == "tools/call" {
				recorder := httptest.NewRecorder()
				fixture.http.Config.Handler.ServeHTTP(recorder, r)
				dropped.Add(1)
				http.Error(w, "qualification: response delivery lost after completion", http.StatusBadGateway)
				return
			}
		}
		fixture.http.Config.Handler.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	lostState := fixture.state(t)
	lostSession, err := fixture.connect(ctx, t, proxy.URL, "echo_agent", lostState, trinityClientFixture, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lostSession.Close() }()
	result, lost := trinityQualificationCall(t, ctx, lostSession, "qualification-lost-delivery", "qualification-lost-thread", "__runkite_qualification_delay__")
	if !result.IsError || lost.Outcome != "delivery_unknown" || lost.RunID == "" || dropped.Load() == 0 {
		t.Fatalf("did not exercise server completion with lost delivery: %+v", lost)
	}
	fixture.assertRuns(t, 4)
	if run, err := fixture.env.store.GetRun(tenant.ScopedContext(ctx, "default"), lost.RunID); err != nil || run.Status != models.RunStatusSuccess {
		t.Fatalf("lost delivery was injected before durable server completion: %v", err)
	}
	if err := lostSession.Close(); err != nil {
		t.Fatal(err)
	}
	dropDelivery.Store(false)
	lostSession, err = fixture.connect(ctx, t, proxy.URL, "echo_agent", lostState, trinityClientFixture, false)
	if err != nil {
		t.Fatal(err)
	}
	result, recovered := trinityQualificationCall(t, ctx, lostSession, "qualification-lost-delivery", "qualification-lost-thread", "__runkite_qualification_delay__")
	if result.IsError || recovered.Outcome != "success" || recovered.RunID != lost.RunID {
		t.Fatalf("lost delivery could not observe the existing result: %+v", recovered)
	}
	if trinityQualificationReply(t, result) != "__runkite_qualification_delay__" {
		t.Fatal("lost delivery recovery changed the successful agent's MCP text projection")
	}
	fixture.assertRuns(t, 4)
	if fixture.toolCalls.Load() != 4 {
		t.Fatal("lost response recovery must not make another upstream MCP tool call")
	}
	toolCallsBefore404 := fixture.toolCalls.Load()
	rejectWait.Store(true)
	result, vanished := trinityQualificationCall(t, ctx, lostSession, "qualification-lost-delivery", "qualification-lost-thread", "__runkite_qualification_delay__")
	if !result.IsError || vanished.RunID != lost.RunID || waitRejected.Load() == 0 {
		t.Fatalf("did not fail closed when the retained run vanished after lookup: %+v", vanished)
	}
	if fixture.toolCalls.Load() != toolCallsBefore404 {
		t.Fatal("a failed wait projection reached upstream MCP again")
	}
	rejectWait.Store(false)
	rejectPreflight.Store(true)
	result, unavailable := trinityQualificationCall(t, ctx, lostSession, "qualification-lost-delivery", "qualification-lost-thread", "__runkite_qualification_delay__")
	if !result.IsError || unavailable.RunID != lost.RunID {
		t.Fatalf("missing claimed run was allowed to dispatch again: %+v", unavailable)
	}
	if fixture.toolCalls.Load() != toolCallsBefore404 {
		t.Fatal("a missing claimed run reached upstream MCP again")
	}
	fixture.assertRuns(t, 4)
	for _, bad := range []struct{ key, agent string }{{"", "echo_agent"}, {"incorrect-fixture-key", "echo_agent"}, {trinityClientFixture, "qualification_missing_agent"}} {
		candidate, err := fixture.connect(ctx, t, fixture.http.URL, bad.agent, fixture.state(t), bad.key, false)
		if err == nil {
			result, _ := trinityQualificationCall(t, ctx, candidate, "qualification-rejected", "qualification-rejected-thread", "No unauthorized run")
			_ = candidate.Close()
			if !result.IsError {
				t.Fatal("missing authentication or an unadvertised agent allowed an invocation")
			}
		}
	}
	fixture.assertRuns(t, 4)
	fixture.stateMu.Lock()
	directories := append([]string(nil), fixture.stateDirectories...)
	fixture.stateMu.Unlock()
	receipts := 0
	for _, directory := range directories {
		if err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			receipts++
			var saved map[string]json.RawMessage
			if err := json.Unmarshal(data, &saved); err != nil {
				return err
			}
			allowed := map[string]bool{"version": true, "endpoint": true, "profile": true, "agent_id": true, "run_id": true, "operation_id": true, "thread_id": true, "message": true}
			if len(saved) != len(allowed) {
				return fmt.Errorf("operation receipt fields differ from the exact input-and-identity contract")
			}
			for field := range saved {
				if !allowed[field] {
					return fmt.Errorf("operation receipt retained a field outside the input-and-identity contract")
				}
			}
			for _, forbidden := range []string{trinityClientFixture, trinityRunnerFixture} {
				if bytes.Contains(data, []byte(forbidden)) {
					return fmt.Errorf("operation receipt retained a credential or tool output")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if receipts < 4 {
		t.Fatalf("expected saved operation receipts, found %d", receipts)
	}
	if output := os.Getenv("RUNKITE_TEST_TRINITY_OUTPUT"); output != "" {
		receipt := map[string]any{
			"passed": true, "plane": "Real SQLite API and gRPC fixture, not the production CLI",
			"caller":       "Official Go MCP SDK stdio client and copied Trinity launcher layout, not the Trinity runtime",
			"worker":       "Actual Python LangGraph worker with provider-free qualification graph and HTTP proxy checkpoints",
			"managed_runs": 4, "real_runner_assignments": fixture.dispatches.Load(), "upstream_mcp_tool_calls": fixture.toolCalls.Load(), "operation_receipts": receipts,
			"checks":         []string{"stdio discovery", "authenticated managed invocation", "caller restart same operation", "two-turn proxy history", "conflicting message and thread reject", "terminal error retry", "completion with lost delivery", "run disappears between lookup and wait", "missing claimed run fails closed", "authentication and unadvertised-agent rejection", "receipt before dispatch", "receipts retain only input and identity, excluding credentials and outputs"},
			"provider_calls": false, "private_credentials_used": false,
		}
		data, _ := json.MarshalIndent(receipt, "", "  ")
		file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(append(data, '\n')); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
