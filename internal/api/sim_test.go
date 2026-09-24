package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/finops"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

func postJSONHeader(url string, body interface{}, headers map[string]string) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return http.DefaultClient.Do(req)
}

func TestSimulation_HeaderSetsMetadataWithoutAuth(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "sim-agent")

	resp, err := postJSONHeader(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "sim-agent"}, map[string]string{
		auth.HeaderSimulation: "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp, 200)
	var run models.Run
	if err := json.Unmarshal(readBody(t, resp), &run); err != nil {
		t.Fatal(err)
	}
	if !models.RunIsSimulation(run.Metadata) {
		t.Fatalf("expected metadata.simulation=true, got %#v", run.Metadata)
	}
}

func TestSimulation_NoHeaderIsNotSimulation(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "sim-agent")

	resp, err := postJSON(env.srv.URL+"/runs", map[string]interface{}{
		"agent_id": "sim-agent",
		"metadata": map[string]interface{}{"simulation": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp, 200)
	var run models.Run
	if err := json.Unmarshal(readBody(t, resp), &run); err != nil {
		t.Fatal(err)
	}
	if models.RunIsSimulation(run.Metadata) {
		t.Fatalf("client metadata must not tag simulation, got %#v", run.Metadata)
	}
}

func TestSimulation_WriteOnlyKeyForbidden(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "sim-agent")
	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"write-key": {Name: "ci", Permissions: []string{"write"}},
		"admin-key": {Name: "ops", Permissions: []string{"admin"}},
	})
	h := auth.Middleware(p, nil, nil, env.apiServer.Handler())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := postJSONHeader(srv.URL+"/runs", map[string]interface{}{"agent_id": "sim-agent"}, map[string]string{
		"Authorization":       "Bearer write-key",
		auth.HeaderSimulation: "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	b := readBody(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d body=%s", resp.StatusCode, b)
	}
	if !bytes.Contains(b, []byte("simulation_requires_admin")) {
		t.Fatalf("want reason_code in body, got %s", b)
	}

	runs, err := env.store.SearchRuns(context.Background(), &models.RunSearchRequest{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("forbidden create must not insert a run, got %d", len(runs))
	}

	resp2, err := postJSONHeader(srv.URL+"/runs", map[string]interface{}{"agent_id": "sim-agent"}, map[string]string{
		"Authorization":       "Bearer admin-key",
		auth.HeaderSimulation: "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp2, 200)
}

func TestSimulation_SkipsHoldAndCount(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "sim-agent")
	env.apiServer.SetFinOps(&finops.Config{
		Reservation: finops.ReservationConfig{USDPerRun: 0.05, TokensPerRun: 1000},
	})

	since := time.Now().UTC().Add(-time.Minute)
	resp, err := postJSONHeader(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "sim-agent"}, map[string]string{
		auth.HeaderSimulation: "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp, 200)
	_ = readBody(t, resp)

	n, err := env.store.CountRunsSince(context.Background(), tenant.DefaultTenant, "sim-agent", since)
	if err != nil || n != 0 {
		t.Fatalf("CountRunsSince after sim = %d err=%v, want 0", n, err)
	}
	_, _, holds, err := env.store.SumUsageHolds(context.Background(), tenant.DefaultTenant, "sim-agent", since, time.Now().UTC().Add(time.Minute))
	if err != nil || holds != 0 {
		t.Fatalf("sim run must not place a usage hold, count=%d err=%v", holds, err)
	}

	resp2, err := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "sim-agent"})
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp2, 200)
	_ = readBody(t, resp2)
	n, err = env.store.CountRunsSince(context.Background(), tenant.DefaultTenant, "sim-agent", since)
	if err != nil || n != 1 {
		t.Fatalf("CountRunsSince after normal = %d err=%v, want 1", n, err)
	}
	_, _, holds, err = env.store.SumUsageHolds(context.Background(), tenant.DefaultTenant, "sim-agent", since, time.Now().UTC().Add(time.Minute))
	if err != nil || holds != 1 {
		t.Fatalf("normal run should hold, count=%d err=%v", holds, err)
	}
}

func TestSimulation_SkipsUsageIngest(t *testing.T) {
	env := newTestEnv(t)
	ctx := tenant.WithContext(context.Background(), tenant.DefaultTenant)
	if err := env.store.UpsertAgent(ctx, &models.Agent{AgentID: "echo", Name: "echo", Capabilities: map[string]interface{}{}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := env.store.CreateThread(ctx, &models.Thread{ThreadID: "tu", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.CreateRun(ctx, &models.Run{
		RunID: "run-sim-usage", ThreadID: "tu", AgentID: "echo", Status: models.RunStatusRunning,
		Metadata: map[string]interface{}{"simulation": true}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	env.apiServer.SetFinOps(&finops.Config{
		Pricebook: finops.Pricebook{"gpt-4o-mini": {InputPer1k: 0.00015, OutputPer1k: 0.0006}},
	})
	out := []byte(`{"messages":[],"usage":{"prompt_tokens":1000,"completion_tokens":500,"cost_usd":0.05,"model":"gpt-4o-mini"}}`)
	if err := env.broker.Publish(ctx, "run-sim-usage", &transport.RunEvent{
		EventID: "e1", Seq: 1, Method: "values",
		Namespace: []string{}, Data: json.RawMessage(out), Ts: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	env.apiServer.StatusCallback()("run-sim-usage", "success", "")

	tin, tout, usd, err := env.store.SumUsage(ctx, tenant.DefaultTenant, "echo", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if tin != 0 || tout != 0 || usd != 0 {
		t.Fatalf("sim ingest must be skipped, got %d/%d/%v", tin, tout, usd)
	}
}

func TestSimulation_AdmissionDailyExcludes(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "daily-agent")
	env.apiServer.SetAdmissionLimits(&api.AdmissionLimits{AgentDaily: 1})

	for i := 0; i < 3; i++ {
		resp, err := postJSONHeader(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "daily-agent"}, map[string]string{
			auth.HeaderSimulation: "true",
		})
		if err != nil {
			t.Fatal(err)
		}
		expectStatus(t, resp, 200)
		resp.Body.Close()
	}
	resp, err := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "daily-agent"})
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp, 200)
	resp.Body.Close()

	resp2, err := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "daily-agent"})
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp2, 429)
	resp2.Body.Close()
}

func TestSimulation_AdmissionConcurrentIncludes(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "conc-agent")
	env.apiServer.SetAdmissionLimits(&api.AdmissionLimits{AgentConcurrent: 1})

	resp, err := postJSONHeader(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "conc-agent"}, map[string]string{
		auth.HeaderSimulation: "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp, 200)
	resp.Body.Close()

	resp2, err := postJSONHeader(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "conc-agent"}, map[string]string{
		auth.HeaderSimulation: "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp2, 429)
	resp2.Body.Close()
}

func TestSimulation_A2AChildInheritsWithoutHeader(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	upsertA2AAgent(t, env, ctx, "parent_agent")
	upsertA2AAgent(t, env, ctx, "child_agent")

	resp, err := postJSONHeader(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "parent_agent"}, map[string]string{
		auth.HeaderSimulation: "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp, 200)
	var parent models.Run
	if err := json.Unmarshal(readBody(t, resp), &parent); err != nil {
		t.Fatal(err)
	}

	childResp, err := postJSON(env.srv.URL+"/internal/a2a/runs", map[string]interface{}{
		"agent_id":      "child_agent",
		"parent_run_id": parent.RunID,
		"input":         map[string]interface{}{"messages": []interface{}{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, childResp, 200)
	var child models.Run
	if err := json.Unmarshal(readBody(t, childResp), &child); err != nil {
		t.Fatal(err)
	}
	if !models.RunIsSimulation(child.Metadata) {
		t.Fatalf("A2A child of sim parent must inherit, got %#v", child.Metadata)
	}
}

func TestSimulation_SkipsLLMCache(t *testing.T) {
	env := newTestEnv(t)
	bootstrapCachedAgent(t, env, "cached-agent", 3600)
	ctx := context.Background()
	input := map[string]interface{}{"messages": []map[string]string{{"role": "user", "content": "what is 2+2?"}}}

	resp, err := postJSON(env.srv.URL+"/threads/cache-sim-1/runs", map[string]interface{}{
		"agent_id": "cached-agent", "input": input,
	})
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp, 200)
	var run1 models.Run
	if err := json.Unmarshal(readBody(t, resp), &run1); err != nil {
		t.Fatal(err)
	}
	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil || assignment == nil {
		t.Fatalf("expected enqueue: %v", err)
	}
	env.broker.Publish(ctx, run1.RunID, &transport.RunEvent{
		EventID: "e1", Seq: 1, Method: "values",
		Namespace: []string{}, Data: json.RawMessage(`{"messages":[{"role":"ai","content":"4"}]}`), Ts: time.Now().UnixMilli(),
	})
	env.apiServer.StatusCallback()(run1.RunID, "success", "")

	resp2, err := postJSONHeader(env.srv.URL+"/threads/cache-sim-2/runs", map[string]interface{}{
		"agent_id": "cached-agent", "input": input,
	}, map[string]string{auth.HeaderSimulation: "true"})
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp2, 200)
	var run2 models.Run
	if err := json.Unmarshal(readBody(t, resp2), &run2); err != nil {
		t.Fatal(err)
	}
	if run2.Metadata["cache_hit"] == true {
		t.Fatal("simulation create must not take the LLM cache hit")
	}
	if !models.RunIsSimulation(run2.Metadata) {
		t.Fatalf("expected sim tag, got %#v", run2.Metadata)
	}
}
