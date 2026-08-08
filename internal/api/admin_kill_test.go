package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/models"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestKillSwitch_DrainCancelsBeyondPageSize(t *testing.T) {
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
	if err := store.CreateThread(acme, &models.Thread{
		ThreadID: "t-kill", Status: models.ThreadStatusBusy, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	const n = 250
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		run := &models.Run{
			RunID:     fmt.Sprintf("run-kill-%03d", i),
			ThreadID:  "t-kill",
			AgentID:   "echo",
			Status:    models.RunStatusPending,
			CreatedAt: now,
			UpdatedAt: now,
			Metadata:  map[string]interface{}{},
		}
		if err := store.CreateRun(acme, run); err != nil {
			t.Fatalf("CreateRun %d: %v", i, err)
		}
	}

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(srv.Close)

	body := `{"tenant_id":"acme","agent_id":"echo","reason":"incident"}`
	resp, err := http.Post(srv.URL+"/admin-api/kill-switches", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create kill: %d %s", resp.StatusCode, raw)
	}
	var out struct {
		Cancelled int `json:"cancelled"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Cancelled != n {
		t.Fatalf("cancelled=%d want %d (page-size bug or Interrupted double-count)", out.Cancelled, n)
	}

	pending := models.RunStatusPending
	still, err := store.SearchRuns(acme, &models.RunSearchRequest{
		Status: &pending, AgentID: "echo", Limit: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(still) != 0 {
		t.Fatalf("expected 0 pending after drain, got %d", len(still))
	}
	running := models.RunStatusRunning
	stillRun, err := store.SearchRuns(acme, &models.RunSearchRequest{
		Status: &running, AgentID: "echo", Limit: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stillRun) != 0 {
		t.Fatalf("expected 0 running after drain, got %d", len(stillRun))
	}

	// Interrupted is the cancel outcome — should be n, not 2n.
	interrupted := models.RunStatusInterrupted
	done, err := store.SearchRuns(acme, &models.RunSearchRequest{
		Status: &interrupted, AgentID: "echo", Limit: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != n {
		t.Fatalf("interrupted count=%d want %d", len(done), n)
	}
}

func TestKillSwitch_PauseOnlyDoesNotCancel(t *testing.T) {
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
	now := time.Now().UTC()
	if err := store.CreateThread(acme, &models.Thread{
		ThreadID: "t-pause", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(acme, &models.Run{
		RunID: "r-pause", ThreadID: "t-pause", AgentID: "echo",
		Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now, Metadata: map[string]interface{}{},
	}); err != nil {
		t.Fatal(err)
	}

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(srv.Close)

	body := `{"tenant_id":"acme","pause_only":true,"reason":"maintenance"}`
	resp, err := http.Post(srv.URL+"/admin-api/kill-switches", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, raw)
	}
	var out struct {
		Cancelled int `json:"cancelled"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.Cancelled != 0 {
		t.Fatalf("pause_only cancelled=%d want 0", out.Cancelled)
	}
	run, err := store.GetRun(acme, "r-pause")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != models.RunStatusPending {
		t.Fatalf("pause_only must leave run pending, got %s", run.Status)
	}
}
