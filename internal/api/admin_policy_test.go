package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	pgstore "github.com/getrunkite/runkite/internal/state/postgres"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestAdminPolicyGrants_UnsupportedStoreReturns501(t *testing.T) {
	env := newTestEnv(t)
	resp, err := http.Get(env.srv.URL + "/admin-api/policy-grants")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("want 501, got %d", resp.StatusCode)
	}
}

func TestAdminPolicyGrants_CRUDAndHotReload(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping policy grant Admin API tests")
	}
	ctx := context.Background()
	store, err := pgstore.New(ctx, dsn)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		_ = store.DeletePolicyGrant(context.Background(), "crud-test-grant")
	})
	_ = store.DeletePolicyGrant(ctx, "crud-test-grant")

	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "baseline", TenantID: "acme", AgentID: "sales", Connector: "sf",
		}},
	})
	queue := inprocess.NewQueue()
	broker := inprocess.NewBroker()
	apiServer := api.NewServer(store, queue, broker, inprocess.NewCancelBus())
	apiServer.SetPolicyEngine(eng)
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(srv.Close)

	// Before overlay: beta/ops/gh denied
	in := policy.PolicyInput{
		Stage: policy.StageConnectorSession, TenantID: "beta", AgentID: "ops", Connector: "gh",
	}
	if dec := eng.Decide(ctx, in); dec.Effect != policy.EffectDeny {
		t.Fatalf("precondition: want deny, got %q", dec.Effect)
	}

	body := `{"id":"crud-test-grant","tenant_id":"beta","agent_id":"ops","connector":"gh"}`
	resp, err := http.Post(srv.URL+"/admin-api/policy-grants", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: %d %s", resp.StatusCode, b)
	}
	if dec := eng.Decide(ctx, in); dec.Effect != policy.EffectAllow {
		t.Fatalf("after create: want allow without restart, got %q", dec.Effect)
	}

	listResp, err := http.Get(srv.URL + "/admin-api/policy-grants?tenant_id=beta")
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	var listed []models.PolicyGrant
	json.NewDecoder(listResp.Body).Decode(&listed)
	if len(listed) != 1 || listed[0].ID != "crud-test-grant" {
		t.Fatalf("list = %+v", listed)
	}

	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/admin-api/policy-grants/crud-test-grant", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status %d", delResp.StatusCode)
	}
	if dec := eng.Decide(ctx, in); dec.Effect != policy.EffectDeny {
		t.Fatalf("after delete: want deny, got %q", dec.Effect)
	}
}

func TestAdminPolicyGrants_DuplicateKeyReturns409(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping policy grant Admin API tests")
	}
	ctx := context.Background()
	store, err := pgstore.New(ctx, dsn)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	for _, id := range []string{"dup-a", "dup-b"} {
		_ = store.DeletePolicyGrant(ctx, id)
		t.Cleanup(func() { _ = store.DeletePolicyGrant(context.Background(), id) })
	}

	eng := policy.New(policy.Config{ForceEnable: true})
	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetPolicyEngine(eng)
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(srv.Close)

	body1 := `{"id":"dup-a","tenant_id":"acme","agent_id":"sales","connector":"sf"}`
	resp1, err := http.Post(srv.URL+"/admin-api/policy-grants", "application/json", strings.NewReader(body1))
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first create: %d", resp1.StatusCode)
	}

	body2 := `{"id":"dup-b","tenant_id":"acme","agent_id":"sales","connector":"sf"}`
	resp2, err := http.Post(srv.URL+"/admin-api/policy-grants", "application/json", strings.NewReader(body2))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp2.Body)
		t.Fatalf("duplicate key: want 409, got %d %s", resp2.StatusCode, b)
	}
}

func TestAdminPolicyGrants_EmptyPolicySectionBootstraps(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping policy grant Admin API tests")
	}
	ctx := context.Background()
	store, err := pgstore.New(ctx, dsn)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	_ = store.DeletePolicyGrant(ctx, "bootstrap-only")
	t.Cleanup(func() { _ = store.DeletePolicyGrant(context.Background(), "bootstrap-only") })

	// ForceEnable mirrors initPolicy with "policy": {}.
	eng := policy.New(policy.Config{ForceEnable: true})
	if !eng.Enabled() {
		t.Fatal("ForceEnable engine must report Enabled")
	}
	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetPolicyEngine(eng)
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(srv.Close)

	body := `{"id":"bootstrap-only","tenant_id":"acme","agent_id":"sales","connector":"sf"}`
	resp, err := http.Post(srv.URL+"/admin-api/policy-grants", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("bootstrap create: want 201, got %d %s", resp.StatusCode, b)
	}
	in := policy.PolicyInput{
		Stage: policy.StageConnectorSession, TenantID: "acme", AgentID: "sales", Connector: "sf",
	}
	if dec := eng.Decide(ctx, in); dec.Effect != policy.EffectAllow {
		t.Fatalf("after bootstrap grant: want allow, got %q", dec.Effect)
	}
}
