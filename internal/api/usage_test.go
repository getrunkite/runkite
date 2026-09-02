package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/finops"
	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestAdmission_BudgetHardDeny(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	acme := tenant.WithContext(ctx, "acme")
	if err := store.UpsertAgent(acme, &models.Agent{
		AgentID: "echo", Name: "Echo", Capabilities: map[string]interface{}{},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.CreateThread(acme, &models.Thread{ThreadID: "tp", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(acme, &models.Run{
		RunID: "prior", ThreadID: "tp", AgentID: "echo", Status: models.RunStatusSuccess,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetFinOps(&finops.Config{
		Budgets: finops.Budgets{
			Agents: map[string]finops.BudgetCap{
				"acme/echo": {MaxRunsPerDay: 1},
			},
		},
	})
	d := hooks.NewDispatcher()
	apiServer.SetHookDispatcher(d)
	apiServer.RegisterAdmissionGate(d)

	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"op": {Permissions: []string{"write"}, TenantID: "acme"},
	})
	h := auth.Middleware(p, nil, nil, apiServer.Handler())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	body := []byte(`{"assistant_id":"echo","input":{}}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/threads/tb/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer op")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 403 hard budget, got %d body=%s", resp.StatusCode, b)
	}

	events, err := store.SearchAuditEvents(tenant.SystemContext(ctx), &models.AuditSearchRequest{
		Decision: "deny", Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.ReasonCode == "budget_exceeded" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("want budget_exceeded audit; got %#v", events)
	}
}

func TestAdmission_BudgetSoftAllow(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	acme := tenant.WithContext(ctx, "acme")
	if err := store.UpsertAgent(acme, &models.Agent{
		AgentID: "echo", Name: "Echo", Capabilities: map[string]interface{}{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteUsageEvent(ctx, &models.UsageEvent{
		ID: "u-soft", TS: time.Now().UTC(), TenantID: "acme", RunID: "old", AgentID: "echo",
		USDEstimate: 5, Source: models.UsageSourceTerminalOutput,
	}); err != nil {
		t.Fatal(err)
	}

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetFinOps(&finops.Config{
		Budgets: finops.Budgets{
			Agents: map[string]finops.BudgetCap{
				"acme/echo": {MaxUSDPerDay: 1, Soft: true},
			},
		},
	})
	d := hooks.NewDispatcher()
	apiServer.SetHookDispatcher(d)
	apiServer.RegisterAdmissionGate(d)

	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"op": {Permissions: []string{"write"}, TenantID: "acme"},
	})
	h := auth.Middleware(p, nil, nil, apiServer.Handler())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	body := []byte(`{"assistant_id":"echo","input":{}}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/threads/ts/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer op")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("soft budget should allow, got %d body=%s", resp.StatusCode, b)
	}

	events, err := store.SearchAuditEvents(tenant.SystemContext(ctx), &models.AuditSearchRequest{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.ReasonCode == "budget_soft" && ev.Decision == "allow" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("want budget_soft allow audit; got %#v", events)
	}
}

func TestUsage_TerminalIngest(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	acme := tenant.WithContext(ctx, "acme")
	if err := store.UpsertAgent(acme, &models.Agent{
		AgentID: "echo", Name: "Echo", Capabilities: map[string]interface{}{},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.CreateThread(acme, &models.Thread{ThreadID: "tu", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(acme, &models.Run{
		RunID: "run-usage", ThreadID: "tu", AgentID: "echo", Status: models.RunStatusRunning,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	broker := inprocess.NewBroker()
	s := api.NewServer(store, inprocess.NewQueue(), broker, inprocess.NewCancelBus())
	s.SetFinOps(&finops.Config{
		Pricebook: finops.Pricebook{"gpt-4o-mini": {InputPer1k: 0.00015, OutputPer1k: 0.0006}},
	})

	out := []byte(`{"messages":[],"usage":{"prompt_tokens":1000,"completion_tokens":500,"cost_usd":0.05,"model":"gpt-4o-mini"}}`)
	if err := broker.Publish(acme, "run-usage", &transport.RunEvent{
		EventID: "run-usage_evt_1", Seq: 1, Method: "values",
		Namespace: []string{}, Data: json.RawMessage(out),
		Ts: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	s.StatusCallback()("run-usage", "success", "")

	tin, tout, usd, err := store.SumUsage(ctx, "acme", "echo", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if tin != 1000 || tout != 500 || usd != 0.05 {
		t.Fatalf("ingested usage = %d/%d/%v want 1000/500/0.05", tin, tout, usd)
	}

	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/admin-api/usage/summary?tenant_id=acme")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("summary: %d %s", resp.StatusCode, b)
	}
	var rows []models.UsageSummaryRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) < 1 || rows[0].TokensIn != 1000 {
		t.Fatalf("want summary with tokens_in=1000, got %#v", rows)
	}
}

func TestUsage_AdminNonSQL501(t *testing.T) {
	srv := httptest.NewServer(api.NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus()).Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/admin-api/usage/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("want 501, got %d", resp.StatusCode)
	}
}

// Break-glass skips policy Decide only; hard finops budgets still deny.
func TestBreakGlass_DoesNotBypassHardBudget(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	acme := tenant.WithContext(ctx, "acme")
	if err := store.UpsertAgent(acme, &models.Agent{
		AgentID: "echo", Name: "Echo", Capabilities: map[string]interface{}{},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.CreateThread(acme, &models.Thread{ThreadID: "tbgb", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(acme, &models.Run{
		RunID: "prior-bg", ThreadID: "tbgb", AgentID: "echo", Status: models.RunStatusSuccess,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateBreakGlassWindow(ctx, &models.BreakGlassWindow{
		ID: "bg-budget", TenantID: "acme", Reason: "sev1",
		StartsAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	denyPDP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"effect":"deny","reason":"pdp says no"}`))
	}))
	t.Cleanup(denyPDP.Close)

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetFinOps(&finops.Config{
		Budgets: finops.Budgets{
			Agents: map[string]finops.BudgetCap{
				"acme/echo": {MaxRunsPerDay: 1},
			},
		},
	})
	apiServer.SetPolicyEngine(policy.New(policy.Config{
		Webhook: &policy.WebhookConfig{URL: denyPDP.URL},
	}))
	d := hooks.NewDispatcher()
	apiServer.SetHookDispatcher(d)
	apiServer.RegisterAdmissionGate(d)

	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"op": {Permissions: []string{"write"}, TenantID: "acme"},
	})
	h := auth.Middleware(p, nil, nil, apiServer.Handler())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	body := []byte(`{"assistant_id":"echo","input":{}}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/threads/tbgb2/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer op")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("break-glass must not bypass hard budget: want 403, got %d body=%s", resp.StatusCode, b)
	}

	events, err := store.SearchAuditEvents(tenant.SystemContext(ctx), &models.AuditSearchRequest{
		Decision: "deny", Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.ReasonCode == "budget_exceeded" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("want budget_exceeded under active break-glass; got %#v", events)
	}
}
