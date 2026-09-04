package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestAdminTry_CreateThreadScopedToTenant(t *testing.T) {
	store, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/admin-api/threads?tenant_id=acme", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	tid, _ := body["thread_id"].(string)
	if tid == "" {
		t.Fatalf("no thread_id: %#v", body)
	}
	// Admin get surfaces tenant_id
	g, err := http.Get(srv.URL + "/admin-api/threads/" + tid)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Body.Close()
	var view map[string]any
	if err := json.NewDecoder(g.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view["tenant_id"] != "acme" {
		t.Fatalf("expected tenant acme, got %#v", view["tenant_id"])
	}
}
