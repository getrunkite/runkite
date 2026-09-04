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

// TestUsage_UnpricedAlertsOnMissingPricebookRow proves a terminal run with
// real tokens but no matching pricebook entry (and no reported cost_usd)
// surfaces a usage_unpriced audit event instead of silently showing $0 —
// the failure mode that makes an under-maintained pricebook look like the
// run was free.
func TestUsage_UnpricedAlertsOnMissingPricebookRow(t *testing.T) {
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
	if err := store.CreateThread(acme, &models.Thread{ThreadID: "tu2", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(acme, &models.Run{
		RunID: "run-unpriced", ThreadID: "tu2", AgentID: "echo", Status: models.RunStatusRunning,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	broker := inprocess.NewBroker()
	s := api.NewServer(store, inprocess.NewQueue(), broker, inprocess.NewCancelBus())
	// Pricebook has rates, but not for the model this run reports — the
	// realistic "admin swapped models, forgot to update the pricebook" case.
	s.SetFinOps(&finops.Config{
		Pricebook: finops.Pricebook{"gpt-4o-mini": {InputPer1k: 0.00015, OutputPer1k: 0.0006}},
	})

	out := []byte(`{"messages":[],"usage":{"prompt_tokens":1000,"completion_tokens":500,"model":"some-new-model-v9"}}`)
	if err := broker.Publish(acme, "run-unpriced", &transport.RunEvent{
		EventID: "run-unpriced_evt_1", Seq: 1, Method: "values",
		Namespace: []string{}, Data: json.RawMessage(out),
		Ts: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	s.StatusCallback()("run-unpriced", "success", "")

	// Usage is still recorded (tokens matter even at $0)...
	tin, tout, usd, err := store.SumUsage(ctx, "acme", "echo", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if tin != 1000 || tout != 500 || usd != 0 {
		t.Fatalf("ingested usage = %d/%d/%v want 1000/500/0", tin, tout, usd)
	}

	// ...and the gap is surfaced via /admin-api/usage/alerts, not silent.
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/admin-api/usage/alerts?tenant_id=acme")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("alerts: %d %s", resp.StatusCode, b)
	}
	var events []*models.AuditEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.ReasonCode == "usage_unpriced" && e.RunID == "run-unpriced" {
			found = true
			if e.Attrs["model"] != "some-new-model-v9" {
				t.Fatalf("usage_unpriced attrs missing model: %#v", e.Attrs)
			}
		}
	}
	if !found {
		t.Fatalf("want a usage_unpriced alert for run-unpriced, got %#v", events)
	}
}

// TestUsage_UnmeteredAlertsWhenRunnerFoundNoUsageAtAll proves a terminal run
// whose runner set usage.unmetered (a real reply, but zero token/cost data
// extractable in any shape it recognizes — the runner-side signal for "a
// brand-new/unrecognized provider or framework integration") surfaces a
// usage_unmetered audit event instead of looking identical to an agent that
// made no LLM call at all. This is a strictly worse failure mode than
// usage_unpriced: there, tokens are at least visible and only the dollar
// figure is wrong; here, nothing is visible unless this alert fires.
func TestUsage_UnmeteredAlertsWhenRunnerFoundNoUsageAtAll(t *testing.T) {
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
	if err := store.CreateThread(acme, &models.Thread{ThreadID: "tu-unmetered", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(acme, &models.Run{
		RunID: "run-unmetered", ThreadID: "tu-unmetered", AgentID: "echo", Status: models.RunStatusRunning,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	broker := inprocess.NewBroker()
	s := api.NewServer(store, inprocess.NewQueue(), broker, inprocess.NewCancelBus())
	s.SetFinOps(&finops.Config{
		Pricebook: finops.Pricebook{"gpt-4o-mini": {InputPer1k: 0.00015, OutputPer1k: 0.0006}},
	})

	// No prompt_tokens/completion_tokens/cost_usd at all -- just the
	// runner's explicit unmetered marker, exactly what a future/
	// unrecognized provider integration would emit.
	out := []byte(`{"messages":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"unmetered":true,"model":"some-brand-new-provider-v1"}}`)
	if err := broker.Publish(acme, "run-unmetered", &transport.RunEvent{
		EventID: "run-unmetered_evt_1", Seq: 1, Method: "values",
		Namespace: []string{}, Data: json.RawMessage(out),
		Ts: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	s.StatusCallback()("run-unmetered", "success", "")

	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/admin-api/usage/alerts?tenant_id=acme")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("alerts: %d %s", resp.StatusCode, b)
	}
	var events []*models.AuditEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.ReasonCode == "usage_unmetered" && e.RunID == "run-unmetered" {
			found = true
			if e.Attrs["model"] != "some-brand-new-provider-v1" {
				t.Fatalf("usage_unmetered attrs missing model: %#v", e.Attrs)
			}
		}
	}
	if !found {
		t.Fatalf("want a usage_unmetered alert for run-unmetered, got %#v", events)
	}
}

// TestUsage_NoUnmeteredAlertForOrdinaryZeroUsageRun proves a run with no
// usage.unmetered marker and genuinely zero tokens/cost (an agent that made
// no LLM call at all, or an adapter that has not wired up FinOps) does not
// spuriously raise usage_unmetered — only the explicit marker does.
func TestUsage_NoUnmeteredAlertForOrdinaryZeroUsageRun(t *testing.T) {
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
	if err := store.CreateThread(acme, &models.Thread{ThreadID: "tu-nousage", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(acme, &models.Run{
		RunID: "run-nousage", ThreadID: "tu-nousage", AgentID: "echo", Status: models.RunStatusRunning,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	broker := inprocess.NewBroker()
	s := api.NewServer(store, inprocess.NewQueue(), broker, inprocess.NewCancelBus())
	s.SetFinOps(&finops.Config{
		Pricebook: finops.Pricebook{"gpt-4o-mini": {InputPer1k: 0.00015, OutputPer1k: 0.0006}},
	})

	// No usage key at all -- a deterministic agent that never called an LLM.
	out := []byte(`{"messages":[]}`)
	if err := broker.Publish(acme, "run-nousage", &transport.RunEvent{
		EventID: "run-nousage_evt_1", Seq: 1, Method: "values",
		Namespace: []string{}, Data: json.RawMessage(out),
		Ts: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	s.StatusCallback()("run-nousage", "success", "")

	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/admin-api/usage/alerts?tenant_id=acme")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []*models.AuditEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.ReasonCode == "usage_unmetered" {
			t.Fatalf("ordinary zero-usage run must not raise usage_unmetered, got %#v", e)
		}
	}
}

// TestUsage_NoUnpricedAlertWhenPricebookEmpty: tokens-only metering (no
// pricebook configured) must not spam usage_unpriced on every run.
func TestUsage_NoUnpricedAlertWhenPricebookEmpty(t *testing.T) {
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
	if err := store.CreateThread(acme, &models.Thread{ThreadID: "tu-empty-pb", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(acme, &models.Run{
		RunID: "run-empty-pb", ThreadID: "tu-empty-pb", AgentID: "echo", Status: models.RunStatusRunning,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	broker := inprocess.NewBroker()
	s := api.NewServer(store, inprocess.NewQueue(), broker, inprocess.NewCancelBus())
	s.SetFinOps(&finops.Config{Pricebook: finops.Pricebook{}})

	out := []byte(`{"messages":[],"usage":{"prompt_tokens":1000,"completion_tokens":500,"model":"gpt-4o-mini"}}`)
	if err := broker.Publish(acme, "run-empty-pb", &transport.RunEvent{
		EventID: "run-empty-pb_evt_1", Seq: 1, Method: "values",
		Namespace: []string{}, Data: json.RawMessage(out),
		Ts: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	s.StatusCallback()("run-empty-pb", "success", "")

	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/admin-api/usage/alerts?tenant_id=acme")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []*models.AuditEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.ReasonCode == "usage_unpriced" {
			t.Fatalf("empty pricebook must not emit usage_unpriced, got %#v", e)
		}
	}
}

// TestUsage_NoUnpricedAlertWhenZeroRateRow: a present pricebook row with
// $0 rates is intentional free tier, not a missing-model gap.
func TestUsage_NoUnpricedAlertWhenZeroRateRow(t *testing.T) {
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
	if err := store.CreateThread(acme, &models.Thread{ThreadID: "tu-zero", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(acme, &models.Run{
		RunID: "run-zero-rate", ThreadID: "tu-zero", AgentID: "echo", Status: models.RunStatusRunning,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	broker := inprocess.NewBroker()
	s := api.NewServer(store, inprocess.NewQueue(), broker, inprocess.NewCancelBus())
	s.SetFinOps(&finops.Config{
		Pricebook: finops.Pricebook{"free-model": {InputPer1k: 0, OutputPer1k: 0}},
	})

	out := []byte(`{"messages":[],"usage":{"prompt_tokens":1000,"completion_tokens":500,"model":"free-model"}}`)
	if err := broker.Publish(acme, "run-zero-rate", &transport.RunEvent{
		EventID: "run-zero-rate_evt_1", Seq: 1, Method: "values",
		Namespace: []string{}, Data: json.RawMessage(out),
		Ts: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	s.StatusCallback()("run-zero-rate", "success", "")

	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/admin-api/usage/alerts?tenant_id=acme")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []*models.AuditEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.ReasonCode == "usage_unpriced" {
			t.Fatalf("$0-rate pricebook row must not emit usage_unpriced, got %#v", e)
		}
	}
}

// TestUsage_NoUnpricedAlertWhenPricebookMatches is the negative case: a
// normal, correctly-priced run must not trip the unpriced alert.
func TestUsage_NoUnpricedAlertWhenPricebookMatches(t *testing.T) {
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
	if err := store.CreateThread(acme, &models.Thread{ThreadID: "tu3", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(acme, &models.Run{
		RunID: "run-priced", ThreadID: "tu3", AgentID: "echo", Status: models.RunStatusRunning,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	broker := inprocess.NewBroker()
	s := api.NewServer(store, inprocess.NewQueue(), broker, inprocess.NewCancelBus())
	s.SetFinOps(&finops.Config{
		Pricebook: finops.Pricebook{"gpt-4o-mini": {InputPer1k: 0.00015, OutputPer1k: 0.0006}},
	})

	out := []byte(`{"messages":[],"usage":{"prompt_tokens":1000,"completion_tokens":500,"model":"gpt-4o-mini"}}`)
	if err := broker.Publish(acme, "run-priced", &transport.RunEvent{
		EventID: "run-priced_evt_1", Seq: 1, Method: "values",
		Namespace: []string{}, Data: json.RawMessage(out),
		Ts: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	s.StatusCallback()("run-priced", "success", "")

	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/admin-api/usage/alerts?tenant_id=acme")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []*models.AuditEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.ReasonCode == "usage_unpriced" {
			t.Fatalf("did not expect usage_unpriced for a correctly-priced run, got %#v", e)
		}
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
