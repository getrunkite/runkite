package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/config"
	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

type mcpRetryQueue struct {
	*inprocess.Queue
	enqueued atomic.Int32
}

func (q *mcpRetryQueue) Enqueue(ctx context.Context, a *transport.RunAssignment) error {
	err := q.Queue.Enqueue(ctx, a)
	if err == nil {
		q.enqueued.Add(1)
	}
	return err
}

type mcpRetryFixture struct {
	store  *sqlite.SQLiteStore
	queue  *mcpRetryQueue
	broker *inprocess.Broker
	plane  *Server
}

func newMCPRetryFixture(t *testing.T) *mcpRetryFixture {
	t.Helper()
	store, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	q := &mcpRetryQueue{Queue: inprocess.NewQueue()}
	b := inprocess.NewBroker()
	f := &mcpRetryFixture{store: store, queue: q, broker: b}
	f.plane = NewServer(store, q, b, inprocess.NewCancelBus())
	for _, id := range []string{"mcp-agent", "other-agent"} {
		if err := store.UpsertAgent(context.Background(), &models.Agent{AgentID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *mcpRetryFixture) request(t *testing.T, args map[string]any) *mcp.CallToolRequest {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: raw}}
}

func mcpRetryText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) != 1 {
		t.Fatalf("unexpected tool result: %+v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected tool content: %T", result.Content[0])
	}
	return text.Text
}

func (f *mcpRetryFixture) seedRun(t *testing.T, ctx context.Context, id, threadID, agentID string, status models.RunStatus, input, output string, metadata ...map[string]interface{}) {
	t.Helper()
	now := time.Now().UTC()
	meta := map[string]interface{}{}
	if len(metadata) > 0 {
		meta = metadata[0]
	}
	if err := f.store.CreateThread(ctx, &models.Thread{ThreadID: threadID, Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CreateRun(ctx, &models.Run{RunID: id, ThreadID: threadID, AgentID: agentID, Status: status, Input: json.RawMessage(input), Output: json.RawMessage(output), Metadata: meta, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateRunStatus(ctx, id, status, []byte(output), ""); err != nil {
		t.Fatal(err)
	}
}

func (f *mcpRetryFixture) finish(t *testing.T, ctx context.Context, a *transport.RunAssignment, status models.RunStatus, text string) {
	t.Helper()
	if accepted, err := f.plane.BeginRunAttempt(ctx, a.RunID, a.Generation); err != nil || !accepted {
		t.Fatalf("begin attempt: accepted=%v err=%v", accepted, err)
	}
	values, _ := json.Marshal(map[string]any{"messages": []map[string]any{{"role": "ai", "content": text}}})
	if err := f.broker.Publish(ctx, a.RunID, &transport.RunEvent{EventID: a.RunID + "-values", Generation: a.Generation, Seq: 1, Method: "values", Data: values}); err != nil {
		t.Fatal(err)
	}
	end, _ := json.Marshal(map[string]any{"status": status})
	if err := f.broker.Publish(ctx, a.RunID, &transport.RunEvent{EventID: a.RunID + "-end", Generation: a.Generation, Seq: 2, Method: "end", Data: end}); err != nil {
		t.Fatal(err)
	}
}

func TestMCPRetryConcurrentObserversDispatchOnce(t *testing.T) {
	f := newMCPRetryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request := f.request(t, map[string]any{"message": "one operation", "run_id": "mcp-retry-concurrent"})
	handler := f.plane.mcpToolHandler("mcp-agent", "default", nil)
	type outcome struct {
		result *mcp.CallToolResult
		err    error
	}
	results := make(chan outcome, 6)
	go func() { result, err := handler(ctx, request); results <- outcome{result, err} }()
	assignment, err := f.queue.Dequeue(ctx, "python-langgraph", time.Second)
	if err != nil || assignment == nil || assignment.RunID != "mcp-retry-concurrent" {
		t.Fatal("initial dispatch", assignment, err)
	}
	for range 5 {
		go func() { result, err := handler(ctx, request); results <- outcome{result, err} }()
	}
	select {
	case early := <-results:
		t.Fatalf("pending run returned before durable completion: %+v", early)
	case <-time.After(20 * time.Millisecond):
	}
	f.finish(t, ctx, assignment, models.RunStatusSuccess, "one result")
	for range 6 {
		select {
		case got := <-results:
			if got.err != nil || got.result.IsError || mcpRetryText(t, got.result) != "one result" {
				t.Fatalf("retry result=%+v err=%v", got.result, got.err)
			}
		case <-ctx.Done():
			t.Fatal("retry did not observe completion")
		}
	}
	if got := f.queue.enqueued.Load(); got != 1 {
		t.Fatalf("same operation dispatched %d times", got)
	}
	run, err := f.store.GetRun(ctx, assignment.RunID)
	if err != nil || run.Status != models.RunStatusSuccess || run.ThreadID != assignment.ThreadID {
		t.Fatal("durable result", run, err)
	}
}

func TestMCPRetryTerminalConflictAndJSONIdentity(t *testing.T) {
	f := newMCPRetryFixture(t)
	ctx := context.Background()
	f.seedRun(t, ctx, "mcp-retry-terminal", "original-thread", "mcp-agent", models.RunStatusSuccess,
		`{"amount":9007199254740993,"label":"same"}`, `{"messages":[{"role":"ai","content":"stored reply"}]}`)
	for _, tc := range []struct {
		name, agent string
		args        map[string]any
		wantError   bool
	}{
		{"same input with reordered fields", "mcp-agent", map[string]any{"run_id": "mcp-retry-terminal", "input": json.RawMessage(`{"label":"same","amount":9007199254740993}`)}, false},
		{"different exact number", "mcp-agent", map[string]any{"run_id": "mcp-retry-terminal", "input": json.RawMessage(`{"label":"same","amount":9007199254740992}`)}, true},
		{"different thread", "mcp-agent", map[string]any{"run_id": "mcp-retry-terminal", "thread_id": "replacement-thread", "input": json.RawMessage(`{"label":"same","amount":9007199254740993}`)}, true},
		{"different agent", "other-agent", map[string]any{"run_id": "mcp-retry-terminal", "input": json.RawMessage(`{"label":"same","amount":9007199254740993}`)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := f.plane.mcpToolHandler(tc.agent, "default", nil)(ctx, f.request(t, tc.args))
			if err != nil || result.IsError != tc.wantError {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if !tc.wantError && mcpRetryText(t, result) != "stored reply" {
				t.Fatalf("wrong stored result: %+v", result)
			}
		})
	}
	if f.queue.enqueued.Load() != 0 {
		t.Fatal("terminal retry or conflict dispatched work")
	}
	if _, err := f.store.GetThread(ctx, "replacement-thread"); err == nil {
		t.Fatal("mismatched retry created a replacement thread")
	}
}

func TestMCPRetryTerminalFailuresRemainToolErrors(t *testing.T) {
	for _, status := range []models.RunStatus{models.RunStatusError, models.RunStatusInterrupted, models.RunStatusTimeout} {
		t.Run(string(status), func(t *testing.T) {
			f := newMCPRetryFixture(t)
			f.seedRun(t, context.Background(), "mcp-terminal-failure", "failed-thread", "mcp-agent", status,
				`{"messages":[{"role":"user","content":"same"}]}`, "")
			result, err := f.plane.mcpToolHandler("mcp-agent", "default", nil)(context.Background(), f.request(t, map[string]any{"run_id": "mcp-terminal-failure", "message": "same"}))
			if err != nil || !result.IsError || f.queue.enqueued.Load() != 0 {
				t.Fatalf("terminal %s became success or redispatched: %+v %v", status, result, err)
			}
		})
	}
}

func TestMCPRetryDeliveryCancellationDoesNotRedispatch(t *testing.T) {
	f := newMCPRetryFixture(t)
	handler := f.plane.mcpToolHandler("mcp-agent", "default", nil)
	request := f.request(t, map[string]any{"run_id": "mcp-canceled-delivery", "message": "one operation"})
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan *mcp.CallToolResult, 1)
	go func() { result, _ := handler(ctx, request); first <- result }()
	assignment, err := f.queue.Dequeue(context.Background(), "python-langgraph", time.Second)
	if err != nil || assignment == nil {
		t.Fatal("initial dispatch", assignment, err)
	}
	cancel()
	select {
	case result := <-first:
		if !result.IsError {
			t.Fatal("canceled delivery reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled delivery did not return")
	}
	run, err := f.store.GetRun(context.Background(), assignment.RunID)
	if err != nil || isTerminalStatus(run.Status) {
		t.Fatalf("delivery cancellation settled work: %+v %v", run, err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	second := make(chan *mcp.CallToolResult, 1)
	go func() { result, _ := handler(ctx2, request); second <- result }()
	f.finish(t, ctx2, assignment, models.RunStatusSuccess, "survived delivery loss")
	select {
	case result := <-second:
		if result.IsError || mcpRetryText(t, result) != "survived delivery loss" {
			t.Fatalf("retry failed: %+v", result)
		}
	case <-ctx2.Done():
		t.Fatal("retry did not observe completion")
	}
	if f.queue.enqueued.Load() != 1 {
		t.Fatal("delivery retry dispatched again")
	}
}

func TestMCPRetryTenantAndCurrentAgentAuthority(t *testing.T) {
	f := newMCPRetryFixture(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")
	f.seedRun(t, ctxA, "private-mcp-run", "private-thread", "mcp-agent", models.RunStatusSuccess,
		`{"messages":[{"role":"user","content":"same"}]}`, `{"messages":[{"role":"ai","content":"tenant-a-private"}]}`)
	identityA := &auth.AuthResult{Identity: "app", TenantID: "tenant-a", Permissions: []string{"write"}}
	revoked := &auth.AuthResult{Identity: "app", TenantID: "tenant-a", Permissions: []string{auth.AgentRunPermission("other-agent")}}
	result, err := f.plane.mcpToolHandler("mcp-agent", "tenant-a", identityA)(auth.WithContext(ctxA, revoked), f.request(t, map[string]any{"run_id": "private-mcp-run", "message": "same"}))
	if err != nil || !result.IsError || strings.Contains(mcpRetryText(t, result), "tenant-a-private") {
		t.Fatalf("stale session authority exposed result: %+v %v", result, err)
	}
	identityB := &auth.AuthResult{Identity: "other-app", TenantID: "tenant-b", Permissions: []string{"write"}}
	if err := f.store.UpsertAgent(ctxB, &models.Agent{AgentID: "mcp-agent", Name: "mcp-agent"}); err != nil {
		t.Fatal(err)
	}
	result, err = f.plane.mcpToolHandler("mcp-agent", "tenant-b", identityB)(auth.WithContext(ctxB, identityB), f.request(t, map[string]any{"run_id": "private-mcp-run", "message": "same"}))
	if err != nil || !result.IsError || strings.Contains(mcpRetryText(t, result), "tenant-a-private") || f.queue.enqueued.Load() != 0 {
		t.Fatalf("cross-tenant retry exposed or dispatched work: %+v %v", result, err)
	}
}

func TestRunRetryRequiresStoredAgentAuthorityAcrossCreateRoutes(t *testing.T) {
	f := newMCPRetryFixture(t)
	f.seedRun(t, context.Background(), "sensitive-run", "sensitive-thread", "other-agent", models.RunStatusSuccess,
		`{"sensitive":"input"}`, `{"messages":[{"role":"ai","content":"protected-result"}]}`)
	provider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{"scoped-key": {Name: "scoped", TenantID: "default", Permissions: []string{auth.AgentRunPermission("mcp-agent")}}})
	handler := auth.Middleware(provider, nil, nil, f.plane.Handler())
	for _, path := range []string{"/runs", "/runs/wait", "/runs/stream", "/threads/sensitive-thread/runs", "/threads/sensitive-thread/runs/wait", "/threads/sensitive-thread/runs/stream"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"run_id":"sensitive-run"}`))
			req.Header.Set("Authorization", "Bearer scoped-key")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusForbidden || strings.Contains(recorder.Body.String(), "protected-result") || strings.Contains(recorder.Body.String(), "sensitive\":\"input") {
				t.Fatalf("scoped retry bypassed agent authority: %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if f.queue.enqueued.Load() != 0 {
		t.Fatal("unauthorized retry dispatched work")
	}
}

// Both requests miss the handler precheck. The second reaches the existing
// run only through createRunCtx's thread-claim race path.
type mcpRetryRaceStore struct {
	state.Store
	prechecks atomic.Int32
	entered   chan struct{}
	inserted  chan struct{}
	insert    sync.Once
}

func (s *mcpRetryRaceStore) GetRun(ctx context.Context, id string) (*models.Run, error) {
	check := s.prechecks.Add(1)
	if id == "mcp-first-request-race" && check <= 2 {
		if check == 2 {
			close(s.entered)
		}
		select {
		case <-s.entered:
			return nil, &state.ErrNotFound{Resource: "run", ID: id}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Store.GetRun(ctx, id)
}

func (s *mcpRetryRaceStore) TryClaimThread(ctx context.Context, id string) (bool, error) {
	claimed, err := s.Store.TryClaimThread(ctx, id)
	if !claimed && err == nil {
		select {
		case <-s.inserted:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return claimed, err
}

func (s *mcpRetryRaceStore) CreateRun(ctx context.Context, run *models.Run) error {
	err := s.Store.CreateRun(ctx, run)
	if err == nil {
		s.insert.Do(func() { close(s.inserted) })
	}
	return err
}

func TestMCPRetryConcurrentFirstRequestsUseRaceObserver(t *testing.T) {
	for _, explicitThread := range []bool{true, false} {
		t.Run(fmt.Sprintf("explicit_thread_%v", explicitThread), func(t *testing.T) {
			f := newMCPRetryFixture(t)
			race := &mcpRetryRaceStore{Store: f.store, entered: make(chan struct{}), inserted: make(chan struct{})}
			f.plane.store = race
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			args := map[string]any{"run_id": "mcp-first-request-race", "message": "same"}
			if explicitThread {
				now := time.Now().UTC()
				if err := f.store.CreateThread(ctx, &models.Thread{ThreadID: "race-thread", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
				args["thread_id"] = "race-thread"
			}
			request := f.request(t, args)
			results := make(chan *mcp.CallToolResult, 2)
			for range 2 {
				go func() {
					result, _ := f.plane.mcpToolHandler("mcp-agent", "default", nil)(ctx, request)
					results <- result
				}()
			}
			a, err := f.queue.Dequeue(ctx, "python-langgraph", time.Second)
			if err != nil || a == nil {
				t.Fatal("race dispatch", a, err)
			}
			f.finish(t, ctx, a, models.RunStatusSuccess, "race winner")
			for range 2 {
				select {
				case result := <-results:
					if result.IsError || mcpRetryText(t, result) != "race winner" {
						t.Fatalf("creation race was not observed: %+v", result)
					}
				case <-ctx.Done():
					t.Fatal("creation race never returned")
				}
			}
			if f.queue.enqueued.Load() != 1 {
				t.Fatal("creation race dispatched duplicate work")
			}
			threads, err := f.store.SearchThreads(ctx, &models.ThreadSearchRequest{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			for _, thread := range threads {
				if thread.Status == models.ThreadStatusBusy {
					t.Fatalf("creation race leaked a busy thread claim: %+v", thread)
				}
			}
		})
	}
}

func TestMCPRetryCanceledObserverPreservesContextError(t *testing.T) {
	f := newMCPRetryFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.plane.waitForExistingRunResult(ctx, "does-not-matter")
	if !errors.Is(err, context.Canceled) || f.queue.enqueued.Load() != 0 {
		t.Fatalf("canceled observer=%v", err)
	}
}

func TestMCPRetryLegacyCallsWithoutRunIDStillDispatchNewRuns(t *testing.T) {
	f := newMCPRetryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request := f.request(t, map[string]any{"message": "independent turn"})
	ids := map[string]bool{}
	for i := range 2 {
		result := make(chan *mcp.CallToolResult, 1)
		go func() {
			response, _ := f.plane.mcpToolHandler("mcp-agent", "default", nil)(ctx, request)
			result <- response
		}()
		a, err := f.queue.Dequeue(ctx, "python-langgraph", time.Second)
		if err != nil || a == nil {
			t.Fatal("legacy dispatch", a, err)
		}
		if ids[a.RunID] {
			t.Fatal("legacy call unexpectedly reused a run")
		}
		ids[a.RunID] = true
		reply := fmt.Sprintf("turn %d", i)
		f.finish(t, ctx, a, models.RunStatusSuccess, reply)
		select {
		case got := <-result:
			if got.IsError || mcpRetryText(t, got) != reply {
				t.Fatalf("legacy result=%+v", got)
			}
		case <-ctx.Done():
			t.Fatal("legacy call never returned")
		}
	}
	if f.queue.enqueued.Load() != 2 {
		t.Fatal("legacy calls did not remain independent")
	}
}

func TestMCPRetryPendingObserverCancellationThenDurableResult(t *testing.T) {
	f := newMCPRetryFixture(t)
	f.seedRun(t, context.Background(), "waiting-retry", "waiting-thread", "mcp-agent", models.RunStatusPending,
		`{"messages":[{"role":"user","content":"same"}]}`, "")
	handler := f.plane.mcpToolHandler("mcp-agent", "default", nil)
	request := f.request(t, map[string]any{"run_id": "waiting-retry", "message": "same"})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result, err := handler(ctx, request)
	if err != nil || !result.IsError || f.queue.enqueued.Load() != 0 {
		t.Fatalf("pending observer canceled as success or dispatched: %+v %v", result, err)
	}
	run, err := f.store.GetRun(context.Background(), "waiting-retry")
	if err != nil || run.Status != models.RunStatusPending {
		t.Fatalf("observer changed run state: %+v %v", run, err)
	}
	if err := f.store.UpdateRunStatus(context.Background(), run.RunID, models.RunStatusSuccess,
		[]byte(`{"messages":[{"role":"ai","content":"durable after observer left"}]}`), ""); err != nil {
		t.Fatal(err)
	}
	result, err = handler(context.Background(), request)
	if err != nil || result.IsError || mcpRetryText(t, result) != "durable after observer left" || f.queue.enqueued.Load() != 0 {
		t.Fatalf("durable retry failed: %+v %v", result, err)
	}
}

func TestRunRetryStoredAgentAuthorityCompatibilityAndAlias(t *testing.T) {
	f := newMCPRetryFixture(t)
	f.seedRun(t, context.Background(), "authority-run", "authority-thread", "other-agent", models.RunStatusSuccess, `{}`, `{}`, map[string]interface{}{"requested_alias": "shared-alias"})
	for _, tc := range []struct {
		name     string
		identity *auth.AuthResult
		allowed  bool
	}{
		{"local no auth", nil, true},
		{"compatibility empty permissions", &auth.AuthResult{Identity: "compat"}, true},
		{"write", &auth.AuthResult{Permissions: []string{"write"}}, true},
		{"admin", &auth.AuthResult{Permissions: []string{"admin"}}, true},
		{"stored agent grant", &auth.AuthResult{Permissions: []string{auth.AgentRunPermission("other-agent")}}, true},
		{"different agent grant", &auth.AuthResult{Permissions: []string{auth.AgentRunPermission("mcp-agent")}}, false},
		{"read cannot create or retry", &auth.AuthResult{Permissions: []string{"read"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.identity != nil {
				ctx = auth.WithContext(ctx, tc.identity)
			}
			run, err := f.plane.findRunForRetry(ctx, "authority-run", &models.RunCreate{}, "")
			if (err == nil && run != nil) != tc.allowed {
				t.Fatalf("retry authority allowed=%v: run=%+v err=%v", tc.allowed, run, err)
			}
		})
	}
	run, err := f.store.GetRun(context.Background(), "authority-run")
	if err != nil {
		t.Fatal(err)
	}
	f.plane.aliases = NewAliasResolver(map[string]config.AgentAliasEntry{"shared-alias": {Targets: map[string]int{"mcp-agent": 1}}})
	// A current alias now pointing at an allowed agent must not expose an old
	// run whose actual stored target belongs to a different permission scope.
	ctx := auth.WithContext(context.Background(), &auth.AuthResult{Permissions: []string{auth.AgentRunPermission("mcp-agent")}})
	if found, err := f.plane.findRunForRetry(ctx, run.RunID, &models.RunCreate{AgentID: "shared-alias"}, ""); err == nil || found != nil {
		t.Fatalf("current alias escalated historical target authority: %+v %v", found, err)
	}
	ctx = auth.WithContext(context.Background(), &auth.AuthResult{Permissions: []string{auth.AgentRunPermission("other-agent")}})
	if found, err := f.plane.findRunForRetry(ctx, run.RunID, &models.RunCreate{AgentID: "shared-alias"}, ""); err != nil || found == nil {
		t.Fatalf("stored-target authorized retry rerolled alias: %+v %v", found, err)
	}
}

func TestMCPRetryProtocolClientUsesOptionalRunID(t *testing.T) {
	f := newMCPRetryFixture(t)
	f.seedRun(t, context.Background(), "mcp-wire-retry", "wire-thread", "mcp-agent", models.RunStatusSuccess,
		`{"messages":[{"role":"user","content":"same"}]}`, `{"messages":[{"role":"ai","content":"wire result"}]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	server, err := f.plane.newMCPServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "retry-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range listed.Tools {
		if tool.Name == "mcp-agent" {
			raw, _ := json.Marshal(tool.InputSchema)
			var schema map[string]interface{}
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatal(err)
			}
			properties, _ := schema["properties"].(map[string]interface{})
			if properties["run_id"] == nil {
				t.Fatal("MCP discovery does not advertise run_id")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("configured agent was not advertised")
	}
	for range 2 {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "mcp-agent", Arguments: map[string]any{"run_id": "mcp-wire-retry", "message": "same"}})
		if err != nil || result.IsError || mcpRetryText(t, result) != "wire result" {
			t.Fatalf("protocol retry=%+v err=%v", result, err)
		}
	}
	if f.queue.enqueued.Load() != 0 {
		t.Fatal("protocol retry enqueued work")
	}
}

func TestRunRetryRacePathsRecheckStoredAgentAuthority(t *testing.T) {
	for _, claimConflict := range []bool{true, false} {
		t.Run(fmt.Sprintf("thread_claim_conflict_%v", claimConflict), func(t *testing.T) {
			f := newMCPRetryFixture(t)
			f.seedRun(t, context.Background(), "protected-race-run", "protected-race-thread", "other-agent", models.RunStatusSuccess, `{}`, `{"private":true}`)
			threadID := "losing-generated-thread"
			if claimConflict {
				threadID = "protected-race-thread"
				if err := f.store.SetThreadStatus(context.Background(), threadID, models.ThreadStatusBusy); err != nil {
					t.Fatal(err)
				}
			}
			ctx := auth.WithContext(context.Background(), &auth.AuthResult{Identity: "scoped", Permissions: []string{auth.AgentRunPermission("mcp-agent")}})
			// Exercise the core fallback directly. Production first runs the
			// handler retry lookup and admission, but neither can stand in for
			// checking authority on a winner discovered after that lookup.
			run, assignment, err := f.plane.createRunCtx(ctx, threadID, &models.RunCreate{RunID: "protected-race-run", AgentID: "other-agent", Input: json.RawMessage(`{}`)})
			var denied *hooks.ErrDenied
			if !errors.As(err, &denied) || run != nil || assignment != nil {
				t.Fatalf("race fallback exposed unauthorized winner: run=%+v assignment=%+v err=%v", run, assignment, err)
			}
			if !claimConflict {
				thread, err := f.store.GetThread(context.Background(), threadID)
				if err != nil || thread.Status == models.ThreadStatusBusy {
					t.Fatalf("denied insert race leaked losing claim: %+v %v", thread, err)
				}
			}
			if f.queue.enqueued.Load() != 0 {
				t.Fatal("unauthorized race dispatched work")
			}
		})
	}
}

type mcpRetryCanceledEnqueue struct {
	*mcpRetryQueue
	cancel   context.CancelFunc
	attempts atomic.Int32
}

func (q *mcpRetryCanceledEnqueue) Enqueue(ctx context.Context, _ *transport.RunAssignment) error {
	q.attempts.Add(1)
	q.cancel()
	return ctx.Err()
}

type mcpRetryCanceledSubscribe struct {
	*inprocess.Broker
	cancel context.CancelFunc
}

func (b *mcpRetryCanceledSubscribe) Subscribe(ctx context.Context, _ string) (<-chan *transport.RunEvent, error) {
	b.cancel()
	return nil, ctx.Err()
}

func TestMCPRetryCanceledDispatchFailureReleasesOwnedRunAndThread(t *testing.T) {
	for _, failure := range []string{"subscribe", "enqueue"} {
		t.Run(failure, func(t *testing.T) {
			f := newMCPRetryFixture(t)
			readCtx := tenant.WithContext(context.Background(), "cleanup-tenant")
			if err := f.store.UpsertAgent(readCtx, &models.Agent{AgentID: "mcp-agent", Name: "mcp-agent"}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(readCtx)
			defer cancel()
			failedQueue := &mcpRetryCanceledEnqueue{mcpRetryQueue: f.queue, cancel: cancel}
			if failure == "subscribe" {
				f.plane.broker = &mcpRetryCanceledSubscribe{Broker: f.broker, cancel: cancel}
			} else {
				f.plane.queue = failedQueue
			}
			handler := f.plane.mcpToolHandler("mcp-agent", "cleanup-tenant", nil)
			request := f.request(t, map[string]any{"run_id": "dispatch-canceled-run", "thread_id": "cleanup-thread", "message": "same operation"})
			result, err := handler(ctx, request)
			if err != nil || !result.IsError {
				t.Fatalf("failed dispatch was not reported: %+v %v", result, err)
			}
			run, err := f.store.GetRun(readCtx, "dispatch-canceled-run")
			if err != nil || run.Status != models.RunStatusError {
				t.Fatalf("canceled cleanup stranded a pending run: %+v %v", run, err)
			}
			thread, err := f.store.GetThread(readCtx, "cleanup-thread")
			if err != nil || thread.Status == models.ThreadStatusBusy {
				t.Fatalf("canceled cleanup stranded a busy thread: %+v %v", thread, err)
			}
			// Retrying observes the durable failed operation. It neither hangs
			// waiting on an assignment that was never queued nor dispatches it.
			retryCtx, cancelRetry := context.WithTimeout(readCtx, time.Second)
			defer cancelRetry()
			result, err = handler(retryCtx, request)
			if err != nil || !result.IsError || retryCtx.Err() != nil {
				t.Fatalf("failed-operation retry did not return its error: %+v %v", result, err)
			}
			wantAttempts := int32(0)
			if failure == "enqueue" {
				wantAttempts = 1
			}
			if failedQueue.attempts.Load() != wantAttempts || f.queue.enqueued.Load() != 0 {
				t.Fatal("retry attempted to redispatch the failed operation")
			}
		})
	}
}

type mcpRetryCanceledInsert struct {
	state.Store
	cancel context.CancelFunc
}

func (s *mcpRetryCanceledInsert) CreateRun(ctx context.Context, _ *models.Run) error {
	s.cancel()
	return ctx.Err()
}

func TestMCPRetryCanceledInsertReleasesTenantThreadWithoutRun(t *testing.T) {
	f := newMCPRetryFixture(t)
	readCtx := tenant.WithContext(context.Background(), "insert-cleanup-tenant")
	if err := f.store.UpsertAgent(readCtx, &models.Agent{AgentID: "mcp-agent", Name: "mcp-agent"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(readCtx)
	defer cancel()
	f.plane.store = &mcpRetryCanceledInsert{Store: f.store, cancel: cancel}
	request := f.request(t, map[string]any{"run_id": "canceled-before-insert", "thread_id": "pre-insert-thread", "message": "same"})
	result, err := f.plane.mcpToolHandler("mcp-agent", "insert-cleanup-tenant", nil)(ctx, request)
	if err != nil || !result.IsError || ctx.Err() != context.Canceled {
		t.Fatalf("insert failure was not retained: %+v %v", result, err)
	}
	if run, err := f.store.GetRun(readCtx, "canceled-before-insert"); err == nil || run != nil {
		t.Fatalf("failed insert left a run: %+v %v", run, err)
	}
	thread, err := f.store.GetThread(readCtx, "pre-insert-thread")
	if err != nil || thread.Status != models.ThreadStatusIdle || f.queue.enqueued.Load() != 0 {
		t.Fatalf("canceled pre-insert cleanup leaked authority: %+v %v", thread, err)
	}
}
