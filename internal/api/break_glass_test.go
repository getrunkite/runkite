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
	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestBreakGlass_CRUDAnd501(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(srv.Close)

	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	body := `{"tenant_id":"acme","agent_id":"sales","reason":"incident drill","expires_at":"` + expires + `"}`
	cresp, err := http.Post(srv.URL+"/admin-api/break-glass", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer cresp.Body.Close()
	if cresp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(cresp.Body)
		t.Fatalf("create: %d %s", cresp.StatusCode, b)
	}
	var win models.BreakGlassWindow
	if err := json.NewDecoder(cresp.Body).Decode(&win); err != nil {
		t.Fatal(err)
	}
	if win.ID == "" || win.TenantID != "acme" || win.AgentID != "sales" {
		t.Fatalf("created = %#v", win)
	}

	found, err := store.FindActiveBreakGlass(ctx, "acme", "sales")
	if err != nil || found == nil || found.ID != win.ID {
		t.Fatalf("FindActive: %#v err=%v", found, err)
	}

	list, err := http.Get(srv.URL + "/admin-api/break-glass?tenant_id=acme")
	if err != nil {
		t.Fatal(err)
	}
	defer list.Body.Close()
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list: %d", list.StatusCode)
	}

	dreq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/admin-api/break-glass/"+win.ID, nil)
	dresp, err := http.DefaultClient.Do(dreq)
	if err != nil {
		t.Fatal(err)
	}
	defer dresp.Body.Close()
	if dresp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", dresp.StatusCode)
	}
	after, err := store.FindActiveBreakGlass(ctx, "acme", "sales")
	if err != nil || after != nil {
		t.Fatalf("after delete want nil, got %#v err=%v", after, err)
	}

	// Mint + revoke leave admin audit rows.
	audits, err := store.SearchAuditEvents(tenant.SystemContext(ctx), &models.AuditSearchRequest{
		TenantID: "acme", Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	var mint, revoke bool
	for _, ev := range audits {
		if ev.Action == "break_glass.mint" {
			mint = true
		}
		if ev.Action == "break_glass.revoke" {
			revoke = true
		}
	}
	if !mint || !revoke {
		t.Fatalf("want mint+revoke audit, mint=%v revoke=%v events=%d", mint, revoke, len(audits))
	}

	nosql := httptest.NewServer(api.NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus()).Handler())
	t.Cleanup(nosql.Close)
	r501, err := http.Get(nosql.URL + "/admin-api/break-glass")
	if err != nil {
		t.Fatal(err)
	}
	defer r501.Body.Close()
	if r501.StatusCode != http.StatusNotImplemented {
		t.Fatalf("want 501, got %d", r501.StatusCode)
	}
}

func TestBreakGlass_BypassesRunCreatePolicyWebhook(t *testing.T) {
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

	denyPDP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"effect":"deny","reason":"pdp says no"}`))
	}))
	t.Cleanup(denyPDP.Close)

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	d := hooks.NewDispatcher()
	apiServer.SetHookDispatcher(d)
	apiServer.RegisterAdmissionGate(d)
	apiServer.SetPolicyEngine(policy.New(policy.Config{
		Webhook: &policy.WebhookConfig{URL: denyPDP.URL},
	}))

	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"op": {Permissions: []string{"write"}, TenantID: "acme"},
	})
	h := auth.Middleware(p, nil, nil, apiServer.Handler())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	runBody := []byte(`{"assistant_id":"echo","input":{}}`)
	mk := func() *http.Request {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/threads/tbg/runs", bytes.NewReader(runBody))
		req.Header.Set("Authorization", "Bearer op")
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	resp, err := http.DefaultClient.Do(mk())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("policy deny want 403, got %d %s", resp.StatusCode, b)
	}

	if err := store.CreateBreakGlassWindow(ctx, &models.BreakGlassWindow{
		ID: "bg-run", TenantID: "acme", Reason: "sev1",
		StartsAt: time.Now().UTC().Add(-time.Minute), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	resp2, err := http.DefaultClient.Do(mk())
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode < 200 || resp2.StatusCode >= 300 {
		b, _ := io.ReadAll(resp2.Body)
		t.Fatalf("break-glass want 2xx, got %d %s", resp2.StatusCode, b)
	}
}

func TestBreakGlass_DoesNotBypassKillOrAuthz(t *testing.T) {
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
	if err := store.CreateBreakGlassWindow(ctx, &models.BreakGlassWindow{
		ID: "bg-k", TenantID: "acme", AgentID: "echo", Reason: "sev1",
		StartsAt: time.Now().UTC().Add(-time.Minute), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertKillSwitch(ctx, &models.KillSwitch{
		ID: "kill-e", TenantID: "acme", AgentID: "echo", PauseOnly: true, Reason: "incident",
	}); err != nil {
		t.Fatal(err)
	}

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	d := hooks.NewDispatcher()
	apiServer.SetHookDispatcher(d)
	apiServer.RegisterAdmissionGate(d)
	// Policy deny would be bypassed; kill must still win.
	apiServer.SetPolicyEngine(policy.New(policy.Config{ForceEnable: true}))

	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"op":     {Permissions: []string{"write"}, TenantID: "acme"},
		"scoped": {Permissions: []string{auth.AgentRunPermission("other")}, TenantID: "acme"},
	})
	h := auth.Middleware(p, nil, nil, apiServer.Handler())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	runBody := []byte(`{"assistant_id":"echo","input":{}}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/threads/tk2/runs", bytes.NewReader(runBody))
	req.Header.Set("Authorization", "Bearer op")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("kill should still 403, got %d %s", resp.StatusCode, b)
	}

	// Clear kill — authz still wins over break-glass.
	if err := store.DeleteKillSwitch(ctx, "kill-e"); err != nil {
		t.Fatal(err)
	}
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/threads/tk3/runs", bytes.NewReader(runBody))
	req2.Header.Set("Authorization", "Bearer scoped")
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp2.Body)
		t.Fatalf("authz should still 403, got %d %s", resp2.StatusCode, b)
	}
}

func TestBreakGlass_RejectsOver24h(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(srv.Close)

	expires := time.Now().UTC().Add(25 * time.Hour).Format(time.RFC3339)
	body := `{"tenant_id":"acme","reason":"too long","expires_at":"` + expires + `"}`
	resp, err := http.Post(srv.URL+"/admin-api/break-glass", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 400 for >24h, got %d %s", resp.StatusCode, b)
	}
}
