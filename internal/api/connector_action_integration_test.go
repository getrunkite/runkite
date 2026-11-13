package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

type actionFailureStore struct {
	*sqlite.SQLiteStore
	failCreate     atomic.Bool
	failDecision   atomic.Bool
	failPrepare    atomic.Bool
	finishFailures atomic.Int32
	finishCalls    atomic.Int32
	finishHook     func(context.Context)
}

func (s *actionFailureStore) CreateConnectorAction(ctx context.Context, a *models.ConnectorAction, mutation string) (*models.ConnectorAction, error) {
	if s.failCreate.Load() {
		return nil, errors.New("fixture creation failure")
	}
	return s.SQLiteStore.CreateConnectorAction(ctx, a, mutation)
}

func (s *actionFailureStore) RecordConnectorActionDecision(ctx context.Context, id string, version int64, dec models.ConnectorActionDecision, mutation string) (*models.ConnectorAction, error) {
	if s.failDecision.Load() {
		return nil, errors.New("fixture decision failure")
	}
	return s.SQLiteStore.RecordConnectorActionDecision(ctx, id, version, dec, mutation)
}

func (s *actionFailureStore) PrepareConnectorActionDispatch(ctx context.Context, id string, version int64, approval, source string, deadline time.Time, mutation string) (*state.ConnectorActionDispatch, error) {
	if s.failPrepare.Load() {
		return nil, errors.New("fixture intent failure")
	}
	return s.SQLiteStore.PrepareConnectorActionDispatch(ctx, id, version, approval, source, deadline, mutation)
}

func (s *actionFailureStore) FinishConnectorAction(ctx context.Context, id string, version int64, dispatch string, ob models.ConnectorActionObservation, mutation string) (*models.ConnectorAction, error) {
	s.finishCalls.Add(1)
	if s.finishHook != nil {
		s.finishHook(ctx)
	}
	if s.finishFailures.Add(-1) >= 0 {
		return nil, errors.New("fixture observation failure")
	}
	return s.SQLiteStore.FinishConnectorAction(ctx, id, version, dispatch, ob, mutation)
}

type actionAPIEnv struct {
	s       *Server
	store   *actionFailureStore
	queue   transport.JobQueue
	ctx     context.Context
	binding *auth.RunBinding
	effects atomic.Int32
	toolMu  sync.RWMutex
	tool    func(http.ResponseWriter, *http.Request, json.RawMessage)
}

func newActionAPIEnv(t *testing.T, mode, authority string) *actionAPIEnv {
	return newActionAPIEnvAtPath(t, "", mode, authority)
}

func newActionAPIEnvAtPath(t *testing.T, path, mode, authority string) *actionAPIEnv {
	t.Helper()
	db, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := tenant.WithContext(context.Background(), "acme")
	if err := db.Init(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.CreateThread(ctx, &models.Thread{ThreadID: "thread", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	manifest := models.RunManifest{SchemaVersion: 1, TenantID: "acme", AgentID: "agent", AgentVersion: 7, Principal: &models.RunManifestPrincipal{Identity: "alice"}}
	if err := db.CreateRun(ctx, &models.Run{RunID: "run", ThreadID: "thread", AgentID: "agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{"run_manifest": manifest}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.BeginRunAttempt(ctx, "run", 2); err != nil || !ok {
		t.Fatalf("begin attempt: accepted=%v error=%v", ok, err)
	}
	store := &actionFailureStore{SQLiteStore: db}
	queue := inprocess.NewQueue()
	assignment := &transport.RunAssignment{RunID: "run", ThreadID: "thread", GraphID: "agent", TenantID: "acme", RunnerKind: "python-langgraph", Generation: 2, User: &transport.UserContext{Identity: "alice"}}
	if err := queue.Enqueue(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	if live, err := queue.Dequeue(ctx, assignment.RunnerKind, time.Second); err != nil || live == nil {
		t.Fatalf("dequeue attempt: %v", err)
	}
	binding := &auth.RunBinding{RunID: "run", ThreadID: "thread", AgentID: "agent", TenantID: "acme", Generation: 2, User: assignment.User}
	env := &actionAPIEnv{store: store, queue: queue, ctx: auth.WithRunBinding(ctx, binding), binding: binding}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&request)
		}
		w.Header().Set("Content-Type", "application/json")
		if request.Method == "tools/call" {
			env.effects.Add(1)
			env.toolMu.RLock()
			tool := env.tool
			env.toolMu.RUnlock()
			if tool != nil {
				tool(w, r, request.ID)
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":{"content":[{"type":"text","text":"synthetic-sensitive-result"}]}}`))
			return
		}
		var result any = map[string]any{}
		switch request.Method {
		case "server/discover":
			result = map[string]any{"supportedVersions": []string{"2026-07-28"}, "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "write", "inputSchema": map[string]any{"type": "object"}}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	t.Cleanup(target.Close)
	s := NewServer(store, queue, inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetConnectorRegistry(connector.NewRegistry(map[string]connector.ConnectorConfig{
		"fixture": {Auth: connector.AuthConfig{Type: "bearer", BearerToken: "synthetic-sensitive-credential"}, MCP: &connector.MCPConfig{URL: target.URL, Transport: mode}},
	}))
	cfg := policy.Config{ForceEnable: true, DefaultEffect: policy.EffectAllow, AuditRequired: true, ActionEvidence: true, Auditor: policy.NewStoreAuditor(store)}
	if authority == "approved" {
		cfg.MandatoryHITL = []policy.MandatoryHITLRule{{ID: "review", TenantID: "acme", Connector: "fixture", Tools: []string{"write"}}}
	}
	if authority == "break_glass" {
		cfg.DefaultEffect = policy.EffectDeny
		if err := db.CreateBreakGlassWindow(ctx, &models.BreakGlassWindow{ID: "fixture-window", TenantID: "acme", Reason: "fixture", StartsAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	s.SetPolicyEngine(policy.New(cfg))
	env.s = s
	ensureConnectorSessions(s)
	return env
}

const recordedToolCall = `{"jsonrpc":"2.0","id":"synthetic-sensitive-request-id","method":"tools/call","params":{"name":"write","arguments":{"amount":1,"access_token":"synthetic-sensitive-argument"}}}`

func (e *actionAPIEnv) setTool(tool func(http.ResponseWriter, *http.Request, json.RawMessage)) {
	e.toolMu.Lock()
	defer e.toolMu.Unlock()
	e.tool = tool
}

func (e *actionAPIEnv) call(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/internal/connectors/fixture/mcp", strings.NewReader(recordedToolCall)).WithContext(e.ctx)
	r.SetPathValue("name", "fixture")
	r.Header.Set(headerConnectorActionID, "untrusted-client-action-id")
	attachConnectorSession(t, e.s, e.binding, "fixture", r)
	w := httptest.NewRecorder()
	e.s.handleProxyMCPRequest(w, r)
	if uuid.Validate(w.Header().Get(headerConnectorActionID)) != nil {
		t.Fatalf("missing server-generated action identity: code=%d body=%s", w.Code, w.Body.String())
	}
	return w
}

func (e *actionAPIEnv) approve(t *testing.T) string {
	t.Helper()
	in := e.s.policyInput(e.ctx, policy.StageToolCall, "fixture", "write")
	var err error
	in.Args, in.ArgsDigest, in.ArgsMeta, err = policy.BindArgsStrict([]byte(`{"amount":1,"access_token":"synthetic-sensitive-argument"}`))
	if err != nil {
		t.Fatal(err)
	}
	id, err := e.s.persistPendingAction(e.ctx, in, "fixture", "write", policy.PolicyDecision{Effect: policy.EffectPending}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetPendingActionApproved(e.ctx, id, "reviewer"); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRecordedConnectorActionsObserveBothTransportsAndAuthorities(t *testing.T) {
	for _, mode := range []string{"json", "streamable_http"} {
		for _, authority := range []string{"policy", "approved", "break_glass"} {
			t.Run(mode+"/"+authority, func(t *testing.T) {
				env := newActionAPIEnv(t, mode, authority)
				approvalID := ""
				if authority == "approved" {
					approvalID = env.approve(t)
				}
				response := env.call(t)
				if response.Code != http.StatusOK || env.effects.Load() != 1 || !strings.Contains(response.Body.String(), "synthetic-sensitive-result") {
					t.Fatalf("dispatch failed: code=%d effects=%d body=%s", response.Code, env.effects.Load(), response.Body.String())
				}
				row, err := env.store.GetConnectorAction(env.ctx, response.Header().Get(headerConnectorActionID))
				if err != nil || row.State != models.ConnectorActionResponseObserved || row.AgentVersion != 7 || row.Generation != 2 || row.Principal != "alice" || row.HTTPStatus == nil || *row.HTTPStatus != http.StatusOK {
					t.Fatalf("wrong action evidence: row=%+v error=%v", row, err)
				}
				if authority == "approved" && (row.PendingActionID != approvalID || row.AuthorizationSource != "approved_capability" || row.Decision != policy.EffectPending) {
					t.Fatalf("approval replaced the original decision: %+v", row)
				}
				if authority == "break_glass" && (row.BreakGlassID != "fixture-window" || row.AuthorizationSource != "break_glass" || row.Decision != policy.EffectDeny) {
					t.Fatalf("break-glass replaced the original decision: %+v", row)
				}
				events, err := env.store.ListConnectorActionEvents(env.ctx, row.ID, 0, 100)
				if err != nil || len(events) != 4 {
					t.Fatalf("incomplete action timeline: count=%d error=%v", len(events), err)
				}
				raw, _ := json.Marshal([]any{row, events})
				if strings.Contains(string(raw), "synthetic-sensitive") {
					t.Fatal("action evidence retained a credential, argument, request ID or response")
				}
				if row.TargetRevision != nil || row.ToolRevision != nil || row.CredentialRef != nil || row.PolicyRevision != nil || row.ResponseDigest != "" {
					t.Fatal("action evidence invented unavailable provenance or hashed a response")
				}
			})
		}
	}
}

func TestRecordedConnectorActionsFailClosedBeforeEffectsAndKeepApproval(t *testing.T) {
	for _, stage := range []string{"create", "decision", "intent"} {
		t.Run(stage, func(t *testing.T) {
			env := newActionAPIEnv(t, "json", "approved")
			approvalID := env.approve(t)
			switch stage {
			case "create":
				env.store.failCreate.Store(true)
			case "decision":
				env.store.failDecision.Store(true)
			case "intent":
				env.store.failPrepare.Store(true)
			}
			response := env.call(t)
			if response.Code != http.StatusServiceUnavailable || env.effects.Load() != 0 {
				t.Fatalf("evidence failure escaped: code=%d effects=%d", response.Code, env.effects.Load())
			}
			pending, err := env.store.GetPendingAction(env.ctx, approvalID)
			if err != nil || pending.Status != models.PendingStatusApproved {
				t.Fatalf("evidence failure burned approval: pending=%+v error=%v", pending, err)
			}
		})
	}
}

func TestRecordedConnectorActionsConcurrentApprovalDispatchesOnce(t *testing.T) {
	env := newActionAPIEnv(t, "json", "approved")
	approvalID := env.approve(t)
	var wg sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); responses <- env.call(t) }()
	}
	wg.Wait()
	close(responses)
	ids := map[string]bool{}
	for response := range responses {
		ids[response.Header().Get(headerConnectorActionID)] = true
	}
	pending, err := env.store.GetPendingAction(env.ctx, approvalID)
	if len(ids) != 2 || env.effects.Load() != 1 || err != nil || pending.Status != models.PendingStatusConsumed {
		t.Fatalf("approval dispatch repeated: actions=%d effects=%d pending=%+v error=%v", len(ids), env.effects.Load(), pending, err)
	}
}

func TestRecordedConnectorActionSeparatesLocalDenialsAndProtocolObservations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    string
		kind     string
		status   int
		effects  int32
		upstream int
		rpcCode  int64
	}{
		{name: "static_filter", state: models.ConnectorActionNotDispatched, status: http.StatusOK},
		{name: "credentials", state: models.ConnectorActionNotDispatched, status: http.StatusBadGateway},
		{name: "expired_assignment", state: models.ConnectorActionNotDispatched, status: http.StatusServiceUnavailable},
		{name: "mismatched_response", state: models.ConnectorActionOutcomeUnknown, kind: "unverified_response", status: http.StatusBadGateway, effects: 1, upstream: http.StatusOK},
		{name: "rpc_error", state: models.ConnectorActionResponseObserved, kind: "jsonrpc_error", status: http.StatusOK, effects: 1, upstream: http.StatusOK, rpcCode: -32042},
		{name: "tool_error", state: models.ConnectorActionResponseObserved, kind: "tool_error_reported", status: http.StatusOK, effects: 1, upstream: http.StatusOK},
		{name: "upstream_unauthorized", state: models.ConnectorActionResponseObserved, kind: "jsonrpc_error", status: http.StatusBadGateway, effects: 1, upstream: http.StatusUnauthorized, rpcCode: -32042},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newActionAPIEnv(t, "json", "policy")
			switch tc.name {
			case "static_filter":
				c, _ := env.s.connectors.Get("fixture")
				c.Config.Tools = &connector.ToolFilter{Deny: []string{"write"}}
			case "credentials":
				c, _ := env.s.connectors.Get("fixture")
				c.Config.Auth = connector.AuthConfig{Type: "oauth2_client_credentials"}
			case "expired_assignment":
				if _, err := env.queue.Ack(env.ctx, "run", 2); err != nil {
					t.Fatal(err)
				}
			case "mismatched_response":
				env.setTool(func(w http.ResponseWriter, _ *http.Request, _ json.RawMessage) {
					_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"different","result":{"content":[]}}`)
				})
			case "rpc_error", "upstream_unauthorized":
				env.setTool(func(w http.ResponseWriter, _ *http.Request, id json.RawMessage) {
					w.WriteHeader(tc.upstream)
					_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(id)+`,"error":{"code":-32042,"message":"synthetic-sensitive-provider-error"}}`)
				})
			case "tool_error":
				env.setTool(func(w http.ResponseWriter, _ *http.Request, id json.RawMessage) {
					_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(id)+`,"result":{"isError":true,"content":[]}}`)
				})
			}
			response := env.call(t)
			row, err := env.store.GetConnectorAction(env.ctx, response.Header().Get(headerConnectorActionID))
			if err != nil || response.Code != tc.status || env.effects.Load() != tc.effects || row.State != tc.state || row.ResponseKind != tc.kind {
				t.Fatalf("incorrect observation: code=%d effects=%d row=%+v error=%v", response.Code, env.effects.Load(), row, err)
			}
			if tc.upstream != 0 && (row.HTTPStatus == nil || *row.HTTPStatus != tc.upstream) {
				t.Fatal("observation replaced upstream HTTP status with API status")
			}
			if tc.rpcCode != 0 && (row.RPCErrorCode == nil || *row.RPCErrorCode != tc.rpcCode) {
				t.Fatal("observation lost numeric JSON-RPC error code")
			}
			raw, _ := json.Marshal(row)
			if strings.Contains(string(raw), "synthetic-sensitive") {
				t.Fatal("protocol observation retained provider error text")
			}
		})
	}
}

func TestRecordedConnectorActionsKeepUnknownWithoutReplaying(t *testing.T) {
	for _, mode := range []string{"json", "streamable_http"} {
		t.Run(mode, func(t *testing.T) {
			env := newActionAPIEnv(t, mode, "policy")
			env.setTool(func(w http.ResponseWriter, r *http.Request, id json.RawMessage) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
			})
			response := env.call(t)
			row, err := env.store.GetConnectorAction(env.ctx, response.Header().Get(headerConnectorActionID))
			if response.Code != http.StatusBadGateway || env.effects.Load() != 1 || err != nil || row.State != models.ConnectorActionOutcomeUnknown {
				t.Fatalf("ambiguous effect lost or replayed: code=%d effects=%d row=%+v error=%v", response.Code, env.effects.Load(), row, err)
			}
		})
	}
}

func TestRecordedConnectorActionsRetryOnlyObservationPersistence(t *testing.T) {
	for _, failures := range []int32{1, 2} {
		t.Run(string(rune('0'+failures)), func(t *testing.T) {
			env := newActionAPIEnv(t, "json", "policy")
			env.store.finishFailures.Store(failures)
			response := env.call(t)
			row, err := env.store.GetConnectorAction(env.ctx, response.Header().Get(headerConnectorActionID))
			if err != nil || env.effects.Load() != 1 || env.store.finishCalls.Load() != 2 {
				t.Fatalf("observation persistence replayed execution: effects=%d calls=%d error=%v", env.effects.Load(), env.store.finishCalls.Load(), err)
			}
			if failures == 1 && (response.Code != http.StatusOK || row.State != models.ConnectorActionResponseObserved) {
				t.Fatalf("observation recovery failed: code=%d row=%+v", response.Code, row)
			}
			if failures == 2 && (response.Code != http.StatusServiceUnavailable || row.State != models.ConnectorActionDispatchIntent || !strings.Contains(response.Body.String(), "may have executed")) {
				t.Fatalf("missing observation was falsely resolved: code=%d row=%+v", response.Code, row)
			}
		})
	}
}

func TestRecordedConnectorActionOriginalReceiptSurvivesReclaim(t *testing.T) {
	env := newActionAPIEnv(t, "json", "policy")
	env.setTool(func(w http.ResponseWriter, r *http.Request, id json.RawMessage) {
		if ok, err := env.store.BeginRunAttempt(env.ctx, "run", 3); err != nil || !ok {
			t.Errorf("replace durable attempt: accepted=%v error=%v", ok, err)
		}
		_, _ = env.queue.Ack(env.ctx, "run", 2)
		if err := env.queue.Enqueue(env.ctx, &transport.RunAssignment{RunID: "run", ThreadID: "thread", GraphID: "agent", TenantID: "acme", RunnerKind: "python-langgraph", Generation: 3}); err != nil {
			t.Error(err)
		}
		if _, err := env.queue.Dequeue(env.ctx, "python-langgraph", time.Second); err != nil {
			t.Error(err)
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(id)+`,"result":{"content":[]}}`)
	})
	response := env.call(t)
	row, err := env.store.GetConnectorAction(env.ctx, response.Header().Get(headerConnectorActionID))
	if err != nil || response.Code != http.StatusOK || row.Generation != 2 || row.State != models.ConnectorActionResponseObserved || env.effects.Load() != 1 {
		t.Fatalf("late response attributed to replacement: code=%d row=%+v error=%v", response.Code, row, err)
	}
}

func TestRecordedConnectorActionFinalWriteSurvivesCallerCancellation(t *testing.T) {
	env := newActionAPIEnv(t, "json", "policy")
	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	env.ctx = ctx
	env.store.finishHook = func(finishCtx context.Context) {
		cancel()
		deadline, bounded := finishCtx.Deadline()
		if finishCtx.Err() != nil || !bounded || time.Until(deadline) > 5*time.Second {
			t.Error("final evidence write inherited cancellation or an unbounded lifetime")
		}
	}
	response := env.call(t)
	row, err := env.store.GetConnectorAction(tenant.WithContext(context.Background(), "acme"), response.Header().Get(headerConnectorActionID))
	if err != nil || row.State != models.ConnectorActionResponseObserved || response.Code != http.StatusOK {
		t.Fatalf("caller cancellation erased evidence: code=%d row=%+v error=%v", response.Code, row, err)
	}
}

func TestManagedApprovalCannotAuthorizeARecreatedRun(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "legacy_proxy"
		if enabled {
			name = "recorded_proxy"
		}
		t.Run(name, func(t *testing.T) {
			env := newActionAPIEnv(t, "json", "approved")
			if !enabled {
				env.s.SetPolicyEngine(policy.New(policy.Config{
					ForceEnable: true, DefaultEffect: policy.EffectAllow, AuditRequired: true, Auditor: policy.NewStoreAuditor(env.store),
					MandatoryHITL: []policy.MandatoryHITLRule{{ID: "review", TenantID: "acme", Connector: "fixture", Tools: []string{"write"}}},
				}))
			}
			approvalID := env.approve(t)
			old, err := env.store.GetRun(env.ctx, "run")
			if err != nil {
				t.Fatal(err)
			}
			if err := env.store.DeleteRun(env.ctx, "run"); err != nil {
				t.Fatal(err)
			}
			if err := env.store.CreateRun(env.ctx, &models.Run{RunID: old.RunID, ThreadID: old.ThreadID, AgentID: old.AgentID, Status: models.RunStatusPending, Metadata: old.Metadata, CreatedAt: old.CreatedAt, UpdatedAt: old.UpdatedAt}); err != nil {
				t.Fatal(err)
			}
			if accepted, err := env.store.BeginRunAttempt(env.ctx, "run", 2); err != nil || !accepted {
				t.Fatalf("start replacement attempt: accepted=%v error=%v", accepted, err)
			}
			r := httptest.NewRequest(http.MethodPost, "/internal/connectors/fixture/mcp", strings.NewReader(recordedToolCall)).WithContext(env.ctx)
			r.SetPathValue("name", "fixture")
			attachConnectorSession(t, env.s, env.binding, "fixture", r)
			w := httptest.NewRecorder()
			env.s.handleProxyMCPRequest(w, r)
			pending, err := env.store.GetPendingAction(env.ctx, approvalID)
			if err != nil || pending.Status != models.PendingStatusApproved || env.effects.Load() != 0 || !strings.Contains(w.Body.String(), `"effect":"pending"`) {
				t.Fatalf("old approval crossed run incarnation: effects=%d pending=%+v code=%d body=%s error=%v", env.effects.Load(), pending, w.Code, w.Body.String(), err)
			}
			if enabled {
				row, err := env.store.GetConnectorAction(env.ctx, w.Header().Get(headerConnectorActionID))
				if err != nil || row.State != models.ConnectorActionApprovalRequired || row.PendingActionID == approvalID {
					t.Fatalf("replacement reused old approval evidence: row=%+v error=%v", row, err)
				}
			}
		})
	}
}

func TestUnboundManagedApprovalsRequireFreshReview(t *testing.T) {
	for _, status := range []string{models.PendingStatusPending, models.PendingStatusApproved} {
		t.Run(status, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy-approval.db")
			env := newActionAPIEnvAtPath(t, path, "json", "approved")
			approvalID := env.approve(t)
			legacy, err := env.store.GetPendingAction(env.ctx, approvalID)
			if err != nil {
				t.Fatal(err)
			}
			// Simulate an existing approval written before incarnation binding.
			// Migration must not silently attach that old authority to this run.
			legacy.RunIncarnationID = ""
			legacy.Status = status
			connection, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = connection.Close() })
			if _, err := connection.ExecContext(env.ctx, `UPDATE pending_actions SET run_incarnation_id='',status=?,pending_key=? WHERE id=?`, status, models.PendingActionKey(legacy), approvalID); err != nil {
				t.Fatal(err)
			}
			if status == models.PendingStatusPending {
				r := httptest.NewRequest(http.MethodPost, "/admin-api/pending-actions/"+approvalID+"/approve", nil)
				r.SetPathValue("id", approvalID)
				w := httptest.NewRecorder()
				env.s.handleAdminApprovePendingAction(w, r)
				if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "fresh approval") {
					t.Fatalf("unbound approval accepted for review: code=%d body=%s", w.Code, w.Body.String())
				}
			} else {
				response := env.call(t)
				if env.effects.Load() != 0 || !strings.Contains(response.Body.String(), `"effect":"pending"`) {
					t.Fatalf("unbound approval authorized execution: effects=%d body=%s", env.effects.Load(), response.Body.String())
				}
			}
			unchanged, err := env.store.GetPendingAction(env.ctx, approvalID)
			if err != nil || unchanged.Status != status || unchanged.RunIncarnationID != "" {
				t.Fatalf("legacy evidence was rewritten or consumed: row=%+v error=%v", unchanged, err)
			}
		})
	}
}

func TestIndependentDecideApprovalBindingDoesNotRequireRunIncarnation(t *testing.T) {
	expires := time.Now().Add(time.Minute)
	action := &models.PendingAction{ID: pendingActionIDPrefix + "fixture", ArgsDigest: "digest", ExpiresAt: &expires}
	if !currentPendingBinding(action) {
		t.Fatal("independent decide approval was forced to register a managed run")
	}
	action.ExpiresAt = nil
	if currentPendingBinding(action) {
		t.Fatal("unbound managed approval was considered current")
	}
}
