package api_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/models"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestAdmission_AgentScopedAuthz(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"echo", "other"} {
		if err := store.UpsertAgent(ctx, &models.Agent{
			AgentID: id, Name: id, Capabilities: map[string]interface{}{},
		}); err != nil {
			t.Fatal(err)
		}
	}

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	d := hooks.NewDispatcher()
	apiServer.SetHookDispatcher(d)
	apiServer.RegisterAdmissionGate(d)

	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"key-a": {Permissions: []string{auth.AgentRunPermission("echo")}},
		"key-b": {Permissions: []string{auth.AgentRunPermission("other")}},
	})
	h := auth.Middleware(p, nil, nil, apiServer.Handler())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	body := []byte(`{"assistant_id":"echo","input":{"msg":"hi"}}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/threads/t1/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer key-a")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("key-a echo: want 2xx, got %d body=%s", resp.StatusCode, b)
	}

	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/threads/t2/runs", bytes.NewReader(body))
	req2.Header.Set("Authorization", "Bearer key-b")
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp2.Body)
		t.Fatalf("key-b echo: want 403, got %d body=%s", resp2.StatusCode, b)
	}
}

func TestAdmission_KillSwitchRefusesCreate(t *testing.T) {
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

	if err := store.UpsertKillSwitch(ctx, &models.KillSwitch{
		ID: "kill-acme-echo", TenantID: "acme", AgentID: "echo",
		PauseOnly: true, Reason: "incident",
	}); err != nil {
		t.Fatal(err)
	}

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	d := hooks.NewDispatcher()
	apiServer.SetHookDispatcher(d)
	apiServer.RegisterAdmissionGate(d)

	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"op": {Permissions: []string{"write"}, TenantID: "acme"},
	})
	h := auth.Middleware(p, nil, nil, apiServer.Handler())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	runBody := []byte(`{"assistant_id":"echo","input":{}}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/threads/tk/runs", bytes.NewReader(runBody))
	req.Header.Set("Authorization", "Bearer op")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 403 under kill, got %d body=%s", resp.StatusCode, b)
	}
}

func TestKillSwitch_CRUDAndFind(t *testing.T) {
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

	resp, err := http.Get(srv.URL + "/admin-api/kill-switches")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d", resp.StatusCode)
	}

	body := `{"tenant_id":"acme","reason":"drill"}`
	cresp, err := http.Post(srv.URL+"/admin-api/kill-switches", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer cresp.Body.Close()
	if cresp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(cresp.Body)
		t.Fatalf("create: %d %s", cresp.StatusCode, b)
	}

	k, err := store.FindActiveKill(ctx, "acme", "any-agent")
	if err != nil || k == nil {
		t.Fatalf("tenant kill should match any agent: %v %#v", err, k)
	}
	if k.AgentID != "" {
		t.Fatalf("want tenant-wide empty agent_id, got %q", k.AgentID)
	}

	dreq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/admin-api/kill-switches/"+k.ID, nil)
	dresp, err := http.DefaultClient.Do(dreq)
	if err != nil {
		t.Fatal(err)
	}
	defer dresp.Body.Close()
	if dresp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", dresp.StatusCode)
	}
	k2, err := store.FindActiveKill(ctx, "acme", "any-agent")
	if err != nil || k2 != nil {
		t.Fatalf("after delete want nil, got %#v err=%v", k2, err)
	}
}

func TestAdmission_NonSQLKillReturns501(t *testing.T) {
	srv := httptest.NewServer(api.NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus()).Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/admin-api/kill-switches")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("want 501, got %d", resp.StatusCode)
	}
}
