package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/finops"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestAdminFinOpsOverlay_PutReloadDelete(t *testing.T) {
	store, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(t.Context()); err != nil {
		t.Fatal(err)
	}

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetFinOps(&finops.Config{
		Pricebook: finops.Pricebook{"file-model": {InputPer1k: 0.001, OutputPer1k: 0.002}},
		Budgets: finops.Budgets{
			Tenants: map[string]finops.BudgetCap{"default": {MaxUSDPerDay: 5}},
		},
	})
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/admin-api/finops")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET status %d body %s", resp.StatusCode, body)
	}
	var view map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view["has_overlay"] != false {
		t.Fatalf("expected no overlay: %#v", view)
	}

	bad := `{"pricebook":{"x":{"input_per_1k":999,"output_per_1k":1}}}`
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/admin-api/finops", bytes.NewReader([]byte(bad)))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for absurd rate, got %d %s", resp.StatusCode, body)
	}

	good := `{"pricebook":{"live-model":{"input_per_1k":0.0001,"output_per_1k":0.0004}},"budgets":{"tenants":{"default":{"max_usd_per_day":42}}}}`
	req, _ = http.NewRequest(http.MethodPut, srv.URL+"/admin-api/finops", bytes.NewReader([]byte(good)))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status %d body %s", resp.StatusCode, body)
	}
	eff := apiServer.FinOps()
	if eff == nil || eff.Pricebook["live-model"].InputPer1k != 0.0001 {
		t.Fatalf("effective missing live-model: %#v", eff)
	}
	if eff.Pricebook["file-model"].InputPer1k != 0.001 {
		t.Fatalf("file baseline lost: %#v", eff.Pricebook)
	}
	if eff.Budgets.Tenants["default"].MaxUSDPerDay != 42 {
		t.Fatalf("budget overlay not applied: %#v", eff.Budgets)
	}

	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/admin-api/finops", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status %d", resp.StatusCode)
	}
	eff = apiServer.FinOps()
	if _, ok := eff.Pricebook["live-model"]; ok {
		t.Fatalf("overlay price should be gone: %#v", eff.Pricebook)
	}
	if eff.Budgets.Tenants["default"].MaxUSDPerDay != 5 {
		t.Fatalf("file budget not restored: %#v", eff.Budgets)
	}
}

func TestAdminFinOpsOverlay_NonSQLStoreReturns501(t *testing.T) {
	srv := httptest.NewServer(api.NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus()).Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/admin-api/finops")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 501, got %d: %s", resp.StatusCode, body)
	}
}
