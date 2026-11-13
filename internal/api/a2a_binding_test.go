package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

// Functional delegation fixtures execute the same binding middleware as serve.
// Hand-created root rows need a dispatched assignment; children already have one
// waiting in the queue and are dequeued before they can delegate in turn.
func activateA2AParent(t *testing.T, env *testEnv, runID string, user *transport.UserContext) *transport.RunAssignment {
	t.Helper()
	ctx := context.Background()
	if current, err := env.queue.LookupInflight(ctx, runID); err != nil {
		t.Fatal(err)
	} else if current != nil {
		return current
	}
	parent, err := env.store.GetRun(tenant.SystemContext(ctx), runID)
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if assignment == nil {
		assignment = &transport.RunAssignment{RunID: runID, ThreadID: parent.ThreadID, GraphID: parent.AgentID, TenantID: parent.TenantID, Generation: 1, RunnerKind: "python-langgraph", User: user}
		if err := env.queue.Enqueue(ctx, assignment); err != nil {
			t.Fatal(err)
		}
		assignment, err = env.queue.Dequeue(ctx, "python-langgraph", time.Second)
		if err != nil || assignment == nil {
			t.Fatalf("activate parent: %v", err)
		}
	}
	if assignment.RunID != runID {
		t.Fatalf("expected parent %s next in queue, got %s", runID, assignment.RunID)
	}
	return assignment
}

func postA2AJSON(t *testing.T, env *testEnv, body map[string]interface{}) (*http.Response, error) {
	t.Helper()
	parentID, _ := body["parent_run_id"].(string)
	generation := int64(1)
	if _, err := env.store.GetRun(tenant.SystemContext(context.Background()), parentID); parentID != "" && err == nil {
		generation = activateA2AParent(t, env, parentID, nil).Generation
	}
	return postA2AWithHeaders(t, env, body, parentID, strconv.FormatInt(generation, 10))
}

func postA2AWithHeaders(t *testing.T, env *testEnv, body map[string]interface{}, runID, generation string) (*http.Response, error) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/a2a/runs", bytes.NewReader(raw))
	if runID != "" {
		req.Header.Set(auth.HeaderRunID, runID)
	}
	if generation != "" {
		req.Header.Set(auth.HeaderGeneration, generation)
	}
	recorder := httptest.NewRecorder()
	auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: env.queue}, env.apiServer.Handler()).ServeHTTP(recorder, req)
	return recorder.Result(), nil
}

func TestA2A_ParentAuthorityCannotBeReplaced(t *testing.T) {
	for _, tc := range []struct {
		name       string
		runID      string
		generation string
		parentID   string
		delegation interface{}
		inactive   bool
		want       int
	}{
		{name: "missing binding", generation: "5", want: http.StatusUnauthorized},
		{name: "foreign parent", runID: "parent", generation: "5", parentID: "foreign-parent", want: http.StatusForbidden},
		{name: "inactive parent", runID: "parent", generation: "5", inactive: true, want: http.StatusForbidden},
		{name: "stale generation", runID: "parent", generation: "4", want: http.StatusForbidden},
		{name: "legacy zero generation", runID: "parent", generation: "0", want: http.StatusForbidden},
		{name: "identity replacement", runID: "parent", generation: "5", delegation: map[string]interface{}{"identity": "bob"}, want: http.StatusForbidden},
		{name: "child not permitted after attenuation", runID: "parent", generation: "5", delegation: map[string]interface{}{"permissions": []string{"read"}}, want: http.StatusForbidden},
		{name: "null delegation", runID: "parent", generation: "5", delegation: json.RawMessage("null"), want: http.StatusForbidden},
		{name: "authentication replacement", runID: "parent", generation: "5", delegation: map[string]interface{}{"is_authenticated": false}, want: http.StatusForbidden},
		{name: "permission elevation", runID: "parent", generation: "5", delegation: map[string]interface{}{"permissions": []string{"write"}}, want: http.StatusForbidden},
		{name: "empty permissions", runID: "parent", generation: "5", delegation: map[string]interface{}{"permissions": []string{}}, want: http.StatusForbidden},
		{name: "null permissions", runID: "parent", generation: "5", delegation: map[string]interface{}{"permissions": nil}, want: http.StatusForbidden},
		{name: "malformed permissions", runID: "parent", generation: "5", delegation: map[string]interface{}{"permissions": []interface{}{"agents:child:run", 7}}, want: http.StatusForbidden},
		{name: "forged flat claim", runID: "parent", generation: "5", delegation: map[string]interface{}{"email": "other@example.com"}, want: http.StatusForbidden},
		{name: "forged nested claim", runID: "parent", generation: "5", delegation: map[string]interface{}{"extra": map[string]interface{}{"tenant_id": "foreign"}}, want: http.StatusForbidden},
		{name: "unrestricted permission echo", runID: "parent", generation: "5", delegation: map[string]interface{}{"identity": "alice", "permissions": []string{}}, want: http.StatusOK},
		{name: "inherit trusted identity", runID: "parent", generation: "5", want: http.StatusOK},
		{name: "frozen principal mismatch", runID: "parent", generation: "5", want: http.StatusForbidden},
		{name: "frozen permissions mismatch", runID: "parent", generation: "5", want: http.StatusForbidden},
		{name: "terminal parent lease retained", runID: "parent", generation: "5", want: http.StatusForbidden},
		{name: "narrow permissions", runID: "parent", generation: "5", delegation: map[string]interface{}{"identity": "alice", "permissions": []string{"agents:child:run"}, "email": "alice@example.com"}, want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			dispatcher := hooks.NewDispatcher()
			env.apiServer.SetHookDispatcher(dispatcher)
			env.apiServer.RegisterAdmissionGate(dispatcher)
			t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })
			ctx := tenant.WithContext(context.Background(), "tenant-a")
			upsertA2AAgent(t, env, ctx, "parent-agent")
			upsertA2AAgent(t, env, ctx, "child")
			now := time.Now().UTC()
			if err := env.store.CreateThread(ctx, &models.Thread{ThreadID: "parent-thread", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			user := &transport.UserContext{Identity: "alice", IsAuthenticated: true, Permissions: []string{"read", "agents:child:run"}, Extra: map[string]interface{}{"email": "alice@example.com"}}
			if tc.name == "unrestricted permission echo" {
				user.Permissions = nil
			}
			manifest := models.RunManifest{SchemaVersion: 1, TenantID: "tenant-a", AgentID: "parent-agent", Principal: &models.RunManifestPrincipal{Identity: user.Identity, Permissions: user.Permissions}}
			if tc.name == "frozen principal mismatch" {
				manifest.Principal.Identity = "other"
			}
			if tc.name == "frozen permissions mismatch" {
				manifest.Principal.Permissions = []string{"read"}
			}
			if err := env.store.CreateRun(ctx, &models.Run{RunID: "parent", ThreadID: "parent-thread", AgentID: "parent-agent", Status: models.RunStatusRunning, Metadata: map[string]interface{}{"run_manifest": manifest}, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if tc.name == "terminal parent lease retained" {
				if err := env.store.UpdateRunStatus(ctx, "parent", models.RunStatusSuccess, nil, ""); err != nil {
					t.Fatal(err)
				}
			}
			assignment := &transport.RunAssignment{RunID: "parent", ThreadID: "parent-thread", GraphID: "parent-agent", RunnerKind: "python-langgraph", TenantID: "tenant-a", Generation: 5, User: user}
			if err := env.queue.Enqueue(ctx, assignment); err != nil {
				t.Fatal(err)
			}
			if _, err := env.queue.Dequeue(ctx, assignment.RunnerKind, time.Second); err != nil {
				t.Fatal(err)
			}
			if tc.inactive {
				if _, err := env.queue.Ack(ctx, "parent", 5); err != nil {
					t.Fatal(err)
				}
			}
			parentID := tc.parentID
			if parentID == "" {
				parentID = "parent"
			}
			body := map[string]interface{}{"agent_id": "child", "parent_run_id": parentID}
			if tc.delegation != nil {
				body["on_behalf_of"] = tc.delegation
			}
			resp, _ := postA2AWithHeaders(t, env, body, tc.runID, tc.generation)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, tc.want, readBody(t, resp))
			}
			children, err := env.store.SearchRuns(ctx, &models.RunSearchRequest{ParentRunID: "parent", Limit: 20})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want != http.StatusOK {
				if len(children) != 0 {
					t.Fatalf("rejected request created %d children", len(children))
				}
				if queued, _ := env.queue.Dequeue(ctx, "python-langgraph", time.Millisecond); queued != nil {
					t.Fatal("rejected request queued a child")
				}
				return
			}
			if len(children) != 1 || children[0].TenantID != "tenant-a" {
				t.Fatalf("children=%+v", children)
			}
			child, err := env.queue.Dequeue(ctx, "python-langgraph", time.Second)
			if err != nil || child == nil || child.User == nil {
				t.Fatalf("child assignment: %+v %v", child, err)
			}
			if child.User.Identity != "alice" || child.User.Extra["email"] != "alice@example.com" {
				t.Fatalf("lost trusted claims: %+v", child.User)
			}
			wantPermissions := user.Permissions
			if tc.name == "narrow permissions" {
				wantPermissions = []string{"agents:child:run"}
			}
			if !reflect.DeepEqual(child.User.Permissions, wantPermissions) && (len(child.User.Permissions) != 0 || len(wantPermissions) != 0) {
				t.Fatalf("permissions=%v want=%v", child.User.Permissions, wantPermissions)
			}
		})
	}
}

// The callback runs on the first body read, after middleware resolved binding.
type a2aBeforeRead struct {
	io.Reader
	before func()
}

func (r *a2aBeforeRead) Read(p []byte) (int, error) {
	if r.before != nil {
		before := r.before
		r.before = nil
		before()
	}
	return r.Reader.Read(p)
}

func startA2AParent(t *testing.T, env *testEnv) *transport.RunAssignment {
	t.Helper()
	upsertA2AAgent(t, env, context.Background(), "parent-agent")
	upsertA2AAgent(t, env, context.Background(), "child")
	resp, err := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "parent-agent"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create parent: %d %s", resp.StatusCode, readBody(t, resp))
	}
	parent, err := env.queue.Dequeue(context.Background(), "python-langgraph", time.Second)
	if err != nil || parent == nil {
		t.Fatalf("dispatch parent: %+v %v", parent, err)
	}
	return parent
}

func changeA2AParent(t *testing.T, env *testEnv, parent *transport.RunAssignment, mode string) {
	t.Helper()
	ctx := context.Background()
	if mode == "cancel" {
		resp, err := postJSON(env.srv.URL+"/runs/"+parent.RunID+"/cancel", map[string]interface{}{})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("cancel: %d %s", resp.StatusCode, readBody(t, resp))
		}
		return
	}
	if count, _, err := env.queue.ReclaimStale(ctx, 0, 0); err != nil || count != 1 {
		t.Fatalf("reclaim count=%d err=%v", count, err)
	}
	if replacement, err := env.queue.Dequeue(ctx, "python-langgraph", time.Second); err != nil || replacement == nil || replacement.Generation != parent.Generation+1 {
		t.Fatalf("replacement=%+v err=%v", replacement, err)
	}
}

func TestA2A_RechecksParentAfterSlowBody(t *testing.T) {
	for _, mode := range []string{"reclaim", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			env := newTestEnv(t)
			parent := startA2AParent(t, env)
			raw, _ := json.Marshal(map[string]interface{}{"agent_id": "child", "parent_run_id": parent.RunID})
			body := &a2aBeforeRead{Reader: bytes.NewReader(raw), before: func() { changeA2AParent(t, env, parent, mode) }}
			req := httptest.NewRequest(http.MethodPost, "/internal/a2a/runs", body)
			req.Header.Set(auth.HeaderRunID, parent.RunID)
			req.Header.Set(auth.HeaderGeneration, strconv.FormatInt(parent.Generation, 10))
			recorder := httptest.NewRecorder()
			auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: env.queue}, env.apiServer.Handler()).ServeHTTP(recorder, req)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			children, err := env.store.SearchRuns(context.Background(), &models.RunSearchRequest{ParentRunID: parent.RunID, Limit: 10})
			if err != nil || len(children) != 0 {
				t.Fatalf("children=%+v err=%v", children, err)
			}
		})
	}
}

type a2aPausedGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *a2aPausedGate) Decide(ctx context.Context, event hooks.Event) hooks.Decision {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.release:
		return hooks.Decision{Allow: true}
	case <-ctx.Done():
		return hooks.Decision{Allow: false}
	}
}

func TestA2A_RechecksParentAfterSlowPreflight(t *testing.T) {
	for _, mode := range []string{"reclaim", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			env := newTestEnv(t)
			parent := startA2AParent(t, env)
			gate := &a2aPausedGate{entered: make(chan struct{}), release: make(chan struct{})}
			var releaseOnce sync.Once
			releaseGate := func() { releaseOnce.Do(func() { close(gate.release) }) }
			defer releaseGate()
			dispatcher := hooks.NewDispatcher()
			dispatcher.RegisterGate(gate)
			env.apiServer.SetHookDispatcher(dispatcher)
			t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })
			done := make(chan *http.Response, 1)
			go func() {
				resp, _ := postA2AWithHeaders(t, env, map[string]interface{}{"agent_id": "child", "thread_id": "child-not-created", "parent_run_id": parent.RunID}, parent.RunID, strconv.FormatInt(parent.Generation, 10))
				done <- resp
			}()
			select {
			case <-gate.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("child did not reach preflight")
			}
			changeA2AParent(t, env, parent, mode)
			releaseGate()
			select {
			case resp := <-done:
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
				}
			case <-time.After(2 * time.Second):
				t.Fatal("child request did not finish")
			}
			children, err := env.store.SearchRuns(context.Background(), &models.RunSearchRequest{ParentRunID: parent.RunID, Limit: 10})
			if err != nil || len(children) != 0 {
				t.Fatalf("children=%+v err=%v", children, err)
			}
			if _, err := env.store.GetThread(context.Background(), "child-not-created"); err == nil {
				t.Fatal("invalidated delegation created a thread")
			}
			if length, err := env.queue.Len(context.Background()); err != nil || length != 0 {
				t.Fatalf("queued children=%d err=%v", length, err)
			}
		})
	}
}

func TestA2A_InvalidatedDuringPrewarmNeverDispatchesChild(t *testing.T) {
	for _, mode := range []string{"reclaim", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			env := newTestEnv(t)
			parent := startA2AParent(t, env)
			entered, release := make(chan struct{}), make(chan struct{})
			var enteredOnce, releaseOnce sync.Once
			releasePrewarm := func() { releaseOnce.Do(func() { close(release) }) }
			defer releasePrewarm()
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"a2a-prewarm-fixture-token","expires_in":3600}`))
			}))
			defer provider.Close()
			defer releasePrewarm()
			env.apiServer.SetConnectorRegistry(connector.NewRegistry(map[string]connector.ConnectorConfig{"slow": {Auth: connector.AuthConfig{Type: "oauth2_client_credentials", TokenURL: provider.URL, ClientID: "fixture", ClientSecret: "fixture"}}}))
			if err := env.store.UpsertAgent(context.Background(), &models.Agent{AgentID: "child", Name: "child", Metadata: map[string]interface{}{"connector_needs": []string{"slow"}}}); err != nil {
				t.Fatal(err)
			}
			done := make(chan *http.Response, 1)
			go func() {
				resp, _ := postA2AWithHeaders(t, env, map[string]interface{}{"agent_id": "child", "thread_id": "child-created", "parent_run_id": parent.RunID}, parent.RunID, strconv.FormatInt(parent.Generation, 10))
				done <- resp
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("child did not reach connector prewarm")
			}
			children, err := env.store.SearchRuns(context.Background(), &models.RunSearchRequest{ParentRunID: parent.RunID, Limit: 10})
			if err != nil || len(children) != 1 {
				t.Fatalf("child not committed before prewarm: %+v err=%v", children, err)
			}
			changeA2AParent(t, env, parent, mode)
			releasePrewarm()
			select {
			case resp := <-done:
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
				}
			case <-time.After(2 * time.Second):
				t.Fatal("child request did not finish")
			}
			child, err := env.store.GetRun(context.Background(), children[0].RunID)
			if err != nil || child.Status != models.RunStatusInterrupted {
				t.Fatalf("child=%+v err=%v", child, err)
			}
			thread, err := env.store.GetThread(context.Background(), "child-created")
			if err != nil || thread.Status == models.ThreadStatusBusy {
				t.Fatalf("thread=%+v err=%v", thread, err)
			}
			if length, err := env.queue.Len(context.Background()); err != nil || length != 0 {
				t.Fatalf("queued children=%d err=%v", length, err)
			}
		})
	}
}

func TestA2A_ConnectorPrewarmUsesChildAuthorityAndAuditIdentity(t *testing.T) {
	for _, tc := range []struct {
		name         string
		grantAgent   string
		wantRequests int64
		wantDecision string
	}{
		{"parent granted child denied", "parent-agent", 0, policy.EffectDeny},
		{"parent denied child granted", "child", 1, policy.EffectAllow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			ctx := tenant.WithContext(context.Background(), "tenant-a")
			upsertA2AAgent(t, env, ctx, "parent-agent")
			if err := env.store.UpsertAgent(ctx, &models.Agent{AgentID: "child", Name: "child", Metadata: map[string]interface{}{"connector_needs": []string{"provider"}}}); err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int64
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"a2a-prewarm-authority-fixture","expires_in":3600}`))
			}))
			defer provider.Close()
			env.apiServer.SetConnectorRegistry(connector.NewRegistry(map[string]connector.ConnectorConfig{"provider": {Auth: connector.AuthConfig{Type: "oauth2_client_credentials", TokenURL: provider.URL, ClientID: "fixture", ClientSecret: "fixture"}}}))
			parentReq := httptest.NewRequest(http.MethodPost, "/runs", bytes.NewBufferString(`{"agent_id":"parent-agent"}`))
			parentReq = parentReq.WithContext(auth.WithContext(ctx, &auth.AuthResult{Identity: "alice", Permissions: []string{"write"}, TenantID: "tenant-a"}))
			parentResponse := httptest.NewRecorder()
			env.apiServer.Handler().ServeHTTP(parentResponse, parentReq)
			if parentResponse.Code != http.StatusOK {
				t.Fatalf("parent status=%d body=%s", parentResponse.Code, parentResponse.Body.String())
			}
			parent, err := env.queue.Dequeue(ctx, "python-langgraph", time.Second)
			if err != nil || parent == nil {
				t.Fatalf("parent=%+v err=%v", parent, err)
			}
			env.apiServer.SetPolicyEngine(policy.New(policy.Config{Grants: []policy.Grant{{ID: "selected-agent", TenantID: "tenant-a", AgentID: tc.grantAgent, Connector: "provider"}}, Auditor: policy.NewStoreAuditor(env.store)}))
			response, _ := postA2AWithHeaders(t, env, map[string]interface{}{"agent_id": "child", "parent_run_id": parent.RunID}, parent.RunID, strconv.FormatInt(parent.Generation, 10))
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("child status=%d body=%s", response.StatusCode, readBody(t, response))
			}
			var child models.Run
			if err := json.NewDecoder(response.Body).Decode(&child); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != tc.wantRequests {
				t.Fatalf("provider requests=%d want=%d", requests.Load(), tc.wantRequests)
			}
			events, err := env.store.SearchAuditEvents(ctx, &models.AuditSearchRequest{Action: policy.StageConnectorSession, Connector: "provider", Limit: 10})
			if err != nil || len(events) != 1 {
				t.Fatalf("audit events=%+v err=%v", events, err)
			}
			event := events[0]
			if event.AgentID != "child" || event.RunID != child.RunID || event.RunID == parent.RunID || event.Generation != 0 || event.TenantID != "tenant-a" || event.Actor != "alice" || event.Decision != tc.wantDecision {
				t.Fatalf("incorrect prewarm audit identity: %+v", event)
			}
		})
	}
}
