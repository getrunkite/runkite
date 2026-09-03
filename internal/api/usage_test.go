package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestAdmission_BudgetAlertHookOnHardDeny(t *testing.T) {
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
	if err := store.CreateThread(acme, &models.Thread{ThreadID: "ta", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(acme, &models.Run{
		RunID: "prior-alert", ThreadID: "ta", AgentID: "echo", Status: models.RunStatusSuccess,
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
	sink := &testSink{}
	d.Register(sink)
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
		t.Fatalf("want 403, got %d %s", resp.StatusCode, b)
	}
	evs := waitForHookCount(t, sink, 1)
	found := false
	for _, e := range evs {
		if e.Type == hooks.BudgetAlert {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("want budget_alert hook, got %#v", evs)
	}
}

func TestUsage_ExportCSV(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteUsageEvent(ctx, &models.UsageEvent{
		ID: "u-exp", TS: time.Now().UTC(), TenantID: "acme", RunID: "r1", AgentID: "echo",
		TokensIn: 10, TokensOut: 5, USDEstimate: 0.01, Source: models.UsageSourceTerminalOutput,
	}); err != nil {
		t.Fatal(err)
	}
	s := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/admin-api/usage/export?tenant_id=acme&format=csv")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("export: %d %s", resp.StatusCode, b)
	}
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if !bytes.Contains(b, []byte("day,tenant_id,agent_id")) && !bytes.Contains(b, []byte("day,tenant_id,agent_id,tokens_in")) {
		// header may use tokens_in naming from admin_usage
		if !strings.Contains(body, "tenant_id") || !strings.Contains(body, "usd") {
			t.Fatalf("unexpected CSV header/body: %s", body)
		}
	}
	if !strings.Contains(body, "acme") {
		t.Fatalf("CSV missing row: %s", body)
	}
}

func TestAdmission_UsageHoldBlocksSecondCreate(t *testing.T) {
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

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetFinOps(&finops.Config{
		Budgets: finops.Budgets{
			Agents: map[string]finops.BudgetCap{
				"acme/echo": {MaxUSDPerDay: 1},
			},
		},
		Reservation: finops.ReservationConfig{USDPerRun: 1},
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

	mk := func(thread string) *http.Response {
		body := []byte(`{"assistant_id":"echo","input":{}}`)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/threads/"+thread+"/runs", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer op")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	r1 := mk("th1")
	defer r1.Body.Close()
	if r1.StatusCode < 200 || r1.StatusCode >= 300 {
		b, _ := io.ReadAll(r1.Body)
		t.Fatalf("first create should allow, got %d %s", r1.StatusCode, b)
	}
	r2 := mk("th2")
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(r2.Body)
		t.Fatalf("second create should hard-deny on open hold, got %d %s", r2.StatusCode, b)
	}
}

func TestFinOps_RoutingRewritesNearSoftPct(t *testing.T) {
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
	for _, id := range []string{"pricey", "cheap"} {
		if err := store.UpsertAgent(acme, &models.Agent{
			AgentID: id, Name: id, Capabilities: map[string]interface{}{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.WriteUsageEvent(ctx, &models.UsageEvent{
		ID: "u-route", TS: time.Now().UTC(), TenantID: "acme", RunID: "old", AgentID: "pricey",
		USDEstimate: 0.85, Source: models.UsageSourceTerminalOutput,
	}); err != nil {
		t.Fatal(err)
	}

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetFinOps(&finops.Config{
		Budgets: finops.Budgets{
			Agents: map[string]finops.BudgetCap{
				"acme/pricey": {MaxUSDPerDay: 1},
			},
		},
		Alerts: finops.AlertsConfig{SoftPct: 80},
		Routing: finops.RoutingConfig{
			Enabled: true,
			SoftPct: 80,
			Aliases: map[string][]string{"pricey": {"cheap"}},
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

	body := []byte(`{"assistant_id":"pricey","input":{}}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/threads/tr/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer op")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create should allow after rewrite, got %d %s", resp.StatusCode, b)
	}
	var run models.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	if run.AgentID != "cheap" {
		t.Fatalf("want routed agent cheap, got %q", run.AgentID)
	}
}

// Reservation holds must not inflate the run dimension: CountRunsSince
// already includes in-flight creates, so adding open-hold count would
// ~2× max_runs_per_day and deny prematurely.
func TestAdmission_ReservationDoesNotDoubleCountRuns(t *testing.T) {
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

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetFinOps(&finops.Config{
		Budgets: finops.Budgets{
			Agents: map[string]finops.BudgetCap{
				"acme/echo": {MaxRunsPerDay: 2},
			},
		},
		Reservation: finops.ReservationConfig{USDPerRun: 0.01},
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

	mk := func(thread string) int {
		body := []byte(`{"assistant_id":"echo","input":{}}`)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/threads/"+thread+"/runs", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer op")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if c := mk("tr1"); c < 200 || c >= 300 {
		t.Fatalf("1st create want 2xx, got %d", c)
	}
	if c := mk("tr2"); c < 200 || c >= 300 {
		t.Fatalf("2nd create want 2xx (cap=2); hold must not consume a run slot, got %d", c)
	}
	if c := mk("tr3"); c != http.StatusForbidden {
		t.Fatalf("3rd create want 403 at max_runs=2, got %d", c)
	}
}

func TestAdmission_CancelInflightOnHardBreach(t *testing.T) {
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
	if err := store.CreateThread(acme, &models.Thread{
		ThreadID: "t-inflight", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(acme, &models.Run{
		RunID: "run-inflight", ThreadID: "t-inflight", AgentID: "echo",
		Status: models.RunStatusRunning, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteUsageEvent(ctx, &models.UsageEvent{
		ID: "u-cap", TS: now, TenantID: "acme", RunID: "spent", AgentID: "echo",
		USDEstimate: 1.0, Source: models.UsageSourceTerminalOutput,
	}); err != nil {
		t.Fatal(err)
	}

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetFinOps(&finops.Config{
		Budgets: finops.Budgets{
			Agents: map[string]finops.BudgetCap{
				"acme/echo": {MaxUSDPerDay: 1},
			},
		},
		OnHardBreach: finops.OnHardBreachCancelInflight,
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
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/threads/t-new/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer op")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 403 hard deny, got %d %s", resp.StatusCode, b)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		run, err := store.GetRun(acme, "run-inflight")
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == models.RunStatusInterrupted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("inflight run not cancelled; status=%s", run.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAdmission_ApproachAlertDedupedPerDay(t *testing.T) {
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
	if err := store.CreateThread(acme, &models.Thread{
		ThreadID: "t-approach", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 8 of 10 day-cap runs used → next creates sit in the default 80% approach band.
	for i := 0; i < 8; i++ {
		if err := store.CreateRun(acme, &models.Run{
			RunID: fmt.Sprintf("prior-%d", i), ThreadID: "t-approach", AgentID: "echo",
			Status: models.RunStatusSuccess, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetFinOps(&finops.Config{
		Budgets: finops.Budgets{
			Agents: map[string]finops.BudgetCap{
				"acme/echo": {MaxRunsPerDay: 10},
			},
		},
	})
	d := hooks.NewDispatcher()
	apiServer.SetHookDispatcher(d)
	apiServer.RegisterAdmissionGate(d)

	authP := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"op": {Permissions: []string{"write"}, TenantID: "acme"},
	})
	h := auth.Middleware(authP, nil, nil, apiServer.Handler())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	mk := func(thread string) int {
		body := []byte(`{"assistant_id":"echo","input":{}}`)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/threads/"+thread+"/runs", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer op")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if c := mk("t-a1"); c < 200 || c >= 300 {
		t.Fatalf("1st approach create want 2xx, got %d", c)
	}
	if c := mk("t-a2"); c < 200 || c >= 300 {
		t.Fatalf("2nd approach create want 2xx, got %d", c)
	}

	events, err := store.SearchAuditEvents(tenant.SystemContext(ctx), &models.AuditSearchRequest{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range events {
		if ev.ReasonCode == policy.ReasonBudgetAlert {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want 1 approach audit for the UTC day, got %d (%#v)", n, events)
	}
}
