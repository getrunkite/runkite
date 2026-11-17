package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

type checkpointIncarnationInflight struct {
	assignment atomic.Pointer[transport.RunAssignment]
}

func (i *checkpointIncarnationInflight) LookupInflight(context.Context, string) (*transport.RunAssignment, error) {
	return i.assignment.Load(), nil
}

func TestOpaqueCheckpointWriteRejectsRecreatedRunAfterAuthorization(t *testing.T) {
	store, queue, broker, cancel := completionFixture(t)
	plane := api.NewServer(store, queue, broker, cancel)
	ctx := tenant.WithContext(t.Context(), "default")
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: "checkpoint-thread", Status: models.ThreadStatusBusy}); err != nil {
		t.Fatal(err)
	}
	if err := seedCheckpointRun(ctx, store, "checkpoint-run", "checkpoint-thread", 1); err != nil {
		t.Fatal(err)
	}
	lookup := &checkpointIncarnationInflight{}
	setAssignment := func() string {
		run, err := store.GetRun(ctx, "checkpoint-run")
		if err != nil {
			t.Fatal(err)
		}
		lookup.assignment.Store(&transport.RunAssignment{RunID: run.RunID, RunIncarnationID: run.IncarnationID, ThreadID: run.ThreadID, TenantID: "default", GraphID: run.AgentID, Generation: 1})
		return run.IncarnationID
	}
	oldIncarnation := setAssignment()
	handler := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: lookup}, plane.Handler())
	body := &checkpointDelayedBody{entered: make(chan struct{}), release: make(chan struct{}), body: bytes.NewBufferString("stale")}
	var once sync.Once
	unblock := func() { once.Do(func() { close(body.release) }) }
	defer unblock()
	request := httptest.NewRequest(http.MethodPut, "/internal/checkpoints/checkpoint-thread/adapter-state", body)
	request.Header.Set(auth.HeaderRunID, "checkpoint-run")
	request.Header.Set(auth.HeaderGeneration, "1")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); handler.ServeHTTP(response, request) }()
	select {
	case <-body.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old request did not reach body read")
	}
	if result, err := store.CompleteRun(ctx, &state.RunCompletion{TrustedSystem: true, RunID: "checkpoint-run", Generation: 1, Status: models.RunStatusSuccess}); err != nil || !result.Applied {
		t.Fatal(result, err)
	}
	if err := store.DeleteRun(ctx, "checkpoint-run"); err != nil {
		t.Fatal(err)
	}
	if err := seedCheckpointRun(ctx, store, "checkpoint-run", "checkpoint-thread", 1); err != nil {
		t.Fatal(err)
	}
	if current := setAssignment(); current == "" || current == oldIncarnation {
		t.Fatal("replacement did not get a fresh durable incarnation")
	}
	current := httptest.NewRequest(http.MethodPut, request.URL.String(), bytes.NewBufferString("current"))
	current.Header.Set(auth.HeaderRunID, "checkpoint-run")
	current.Header.Set(auth.HeaderGeneration, "1")
	currentResponse := httptest.NewRecorder()
	handler.ServeHTTP(currentResponse, current)
	if currentResponse.Code != http.StatusNoContent {
		t.Fatalf("current PUT: %d %s", currentResponse.Code, currentResponse.Body)
	}
	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old PUT did not finish")
	}
	blob, err := store.GetOpaqueCheckpoint(ctx, "checkpoint-thread", "adapter-state")
	if response.Code != http.StatusForbidden || err != nil || string(blob.Data) != "current" {
		t.Fatalf("recreated run accepted old authority: status=%d blob=%+v error=%v", response.Code, blob, err)
	}
	var denial map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &denial); err != nil || denial["reason_code"] != "run_incarnation_mismatch" {
		t.Fatalf("wrong stale-run denial: %s error=%v", response.Body, err)
	}
}

func TestOpaqueCheckpointDeleteRejectsRecreatedRunAfterAuthorization(t *testing.T) {
	store, queue, broker, cancel := completionFixture(t)
	plane := api.NewServer(store, queue, broker, cancel)
	ctx := tenant.WithContext(t.Context(), "default")
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: "checkpoint-thread", Status: models.ThreadStatusBusy}); err != nil {
		t.Fatal(err)
	}
	if err := seedCheckpointRun(ctx, store, "checkpoint-run", "checkpoint-thread", 1); err != nil {
		t.Fatal(err)
	}
	old, err := store.GetRun(ctx, "checkpoint-run")
	if err != nil {
		t.Fatal(err)
	}
	lookup := &checkpointIncarnationInflight{}
	lookup.assignment.Store(&transport.RunAssignment{RunID: old.RunID, RunIncarnationID: old.IncarnationID, ThreadID: old.ThreadID, TenantID: "default", GraphID: old.AgentID, Generation: 1})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		plane.Handler().ServeHTTP(w, r)
	})
	handler := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: lookup}, next)
	request := httptest.NewRequest(http.MethodDelete, "/internal/checkpoints/checkpoint-thread/adapter-state", nil)
	request.Header.Set(auth.HeaderRunID, "checkpoint-run")
	request.Header.Set(auth.HeaderGeneration, "1")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); handler.ServeHTTP(response, request) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old DELETE did not pass authorization")
	}
	if result, err := store.CompleteRun(ctx, &state.RunCompletion{TrustedSystem: true, RunID: old.RunID, Generation: 1, Status: models.RunStatusSuccess}); err != nil || !result.Applied {
		t.Fatal(result, err)
	}
	if err := store.DeleteRun(ctx, old.RunID); err != nil {
		t.Fatal(err)
	}
	if err := seedCheckpointRun(ctx, store, old.RunID, old.ThreadID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutOpaqueCheckpoint(ctx, old.ThreadID, "adapter-state", []byte("current"), "test", nil); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old DELETE did not finish")
	}
	var denial map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &denial); err != nil || response.Code != http.StatusForbidden || denial["reason_code"] != "run_incarnation_mismatch" {
		t.Fatalf("recreated run accepted old DELETE: %d %s error=%v", response.Code, response.Body, err)
	}
	if blob, err := store.GetOpaqueCheckpoint(ctx, old.ThreadID, "adapter-state"); err != nil || string(blob.Data) != "current" {
		t.Fatalf("replacement checkpoint changed: blob=%+v error=%v", blob, err)
	}
}

func TestOpaqueCheckpointMutationsRequireCapturedRecordIdentity(t *testing.T) {
	store, queue, broker, cancel := completionFixture(t)
	plane := api.NewServer(store, queue, broker, cancel)
	ctx := tenant.WithContext(t.Context(), "default")
	if accepted, err := store.BeginRunAttempt(ctx, "run", 1); err != nil || !accepted {
		t.Fatal(accepted, err)
	}
	if _, err := store.PutOpaqueCheckpoint(ctx, "thread", "adapter-state", []byte("saved"), "test", nil); err != nil {
		t.Fatal(err)
	}
	ctx = auth.WithRunBinding(ctx, &auth.RunBinding{RunID: "run", ThreadID: "thread", Generation: 1, TenantID: "default"})
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		response := httptest.NewRecorder()
		plane.Handler().ServeHTTP(response, httptest.NewRequest(method, "/internal/checkpoints/thread/adapter-state", bytes.NewBufferString("unbound")).WithContext(ctx))
		var denial map[string]string
		if err := json.Unmarshal(response.Body.Bytes(), &denial); err != nil || response.Code != http.StatusForbidden || denial["reason_code"] != auth.ReasonRunBindingRequired {
			t.Fatalf("missing record identity accepted: %d %s error=%v", response.Code, response.Body, err)
		}
	}
	if blob, err := store.GetOpaqueCheckpoint(ctx, "thread", "adapter-state"); err != nil || string(blob.Data) != "saved" {
		t.Fatalf("unbound mutation changed checkpoint: blob=%+v error=%v", blob, err)
	}
}
