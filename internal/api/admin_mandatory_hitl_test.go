package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestMandatoryHITL_AdminCRUDAndReload(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	eng := policy.New(policy.Config{
		ForceEnable: true,
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "gh",
		}},
	})
	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetPolicyEngine(eng)
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(srv.Close)

	in := policy.PolicyInput{
		Stage: policy.StageToolCall, TenantID: "acme", AgentID: "sales",
		Connector: "gh", Tool: "delete_repo",
	}
	if dec := eng.Decide(ctx, in); dec.Effect != policy.EffectAllow {
		t.Fatalf("precondition: %#v", dec)
	}

	body := `{"tenant_id":"acme","connector":"gh","tools":["delete_repo"]}`
	cresp, err := http.Post(srv.URL+"/admin-api/mandatory-hitl", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer cresp.Body.Close()
	if cresp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(cresp.Body)
		t.Fatalf("create: %d %s", cresp.StatusCode, b)
	}
	var created models.MandatoryHITLRule
	if err := json.NewDecoder(cresp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatal("missing id")
	}

	if dec := eng.Decide(ctx, in); dec.Effect != policy.EffectPending || dec.ReasonCode != policy.ReasonMandatoryHITL {
		t.Fatalf("after create reload: %#v", dec)
	}

	dreq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/admin-api/mandatory-hitl/"+created.ID, nil)
	dresp, err := http.DefaultClient.Do(dreq)
	if err != nil {
		t.Fatal(err)
	}
	defer dresp.Body.Close()
	if dresp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", dresp.StatusCode)
	}
	if dec := eng.Decide(ctx, in); dec.Effect != policy.EffectAllow {
		t.Fatalf("after delete: %#v", dec)
	}
}

func TestMandatoryHITL_Admin501WithoutSQL(t *testing.T) {
	eng := policy.New(policy.Config{ForceEnable: true})
	apiServer := api.NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetPolicyEngine(eng)
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/admin-api/mandatory-hitl")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("want 501, got %d", resp.StatusCode)
	}
}
