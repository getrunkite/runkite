package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

type checkpointFenceInflight struct{ generation atomic.Int64 }

func (i *checkpointFenceInflight) LookupInflight(context.Context, string) (*transport.RunAssignment, error) {
	return &transport.RunAssignment{RunID: "checkpoint-run", ThreadID: "checkpoint-thread", GraphID: "agent", TenantID: "default", Generation: i.generation.Load()}, nil
}

type checkpointDelayedBody struct {
	entered chan struct{}
	release chan struct{}
	body    io.Reader
	first   bool
}

func (b *checkpointDelayedBody) Read(p []byte) (int, error) {
	if !b.first {
		b.first = true
		close(b.entered)
		<-b.release
	}
	return b.body.Read(p)
}
func (*checkpointDelayedBody) Close() error { return nil }

func TestOpaqueCheckpointWriteFenceAfterAuthorization(t *testing.T) {
	for _, mode := range []string{"unconditional", "cas", "create-only", "terminal"} {
		t.Run(mode, func(t *testing.T) {
			env := newTestEnv(t)
			ctx := tenant.WithContext(context.Background(), "default")
			if err := env.store.CreateThread(ctx, &models.Thread{ThreadID: "checkpoint-thread", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}}); err != nil {
				t.Fatal(err)
			}
			if err := seedCheckpointRun(ctx, env.store, "checkpoint-run", "checkpoint-thread", 1); err != nil {
				t.Fatal(err)
			}
			if mode != "create-only" {
				if _, err := env.store.PutOpaqueCheckpoint(ctx, "checkpoint-thread", "adapter-state", []byte("initial"), "test", nil); err != nil {
					t.Fatal(err)
				}
			}
			lookup := &checkpointFenceInflight{}
			lookup.generation.Store(1)
			handler := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: lookup}, env.apiServer.Handler())
			body := &checkpointDelayedBody{entered: make(chan struct{}), release: make(chan struct{}), body: bytes.NewBufferString("stale")}
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(body.release) }) }
			defer unblock()
			stale := httptest.NewRequest(http.MethodPut, "/internal/checkpoints/checkpoint-thread/adapter-state", body)
			stale.Header.Set(auth.HeaderRunID, "checkpoint-run")
			stale.Header.Set(auth.HeaderGeneration, "1")
			if mode == "cas" {
				stale.Header.Set("If-Match", `"1"`)
			}
			if mode == "create-only" {
				stale.Header.Set("If-None-Match", "*")
			}
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); handler.ServeHTTP(response, stale) }()
			select {
			case <-body.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("old request did not reach body read")
			}
			wantData, wantReason := "current", auth.ReasonRunGenerationMismatch
			if mode == "terminal" {
				wantData, wantReason = "initial", auth.ReasonRunNotInflight
				if _, err := env.store.CompleteRun(ctx, &state.RunCompletion{RunID: "checkpoint-run", Generation: 1, Status: "interrupted"}); err != nil {
					t.Fatal(err)
				}
			} else {
				if ok, err := env.store.BeginRunAttempt(ctx, "checkpoint-run", 2); err != nil || !ok {
					t.Fatal(ok, err)
				}
				lookup.generation.Store(2)
				current := httptest.NewRequest(http.MethodPut, stale.URL.String(), bytes.NewBufferString("current"))
				current.Header.Set(auth.HeaderRunID, "checkpoint-run")
				current.Header.Set(auth.HeaderGeneration, "2")
				currentResponse := httptest.NewRecorder()
				handler.ServeHTTP(currentResponse, current)
				if currentResponse.Code != http.StatusNoContent {
					t.Fatalf("current PUT: %d %s", currentResponse.Code, currentResponse.Body)
				}
			}
			unblock()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("stale PUT did not finish")
			}
			var denial map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &denial); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusForbidden || denial["reason_code"] != wantReason {
				t.Fatalf("stale PUT: %d %s", response.Code, response.Body)
			}
			blob, err := env.store.GetOpaqueCheckpoint(ctx, "checkpoint-thread", "adapter-state")
			if err != nil || string(blob.Data) != wantData {
				t.Fatalf("history changed: blob=%+v err=%v", blob, err)
			}
		})
	}
}

func TestOpaqueCheckpointDeleteFenceAfterAuthorization(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "reclaimed", true: "terminal"}[terminal], func(t *testing.T) {
			env := newTestEnv(t)
			ctx := tenant.WithContext(context.Background(), "default")
			if err := env.store.CreateThread(ctx, &models.Thread{ThreadID: "checkpoint-thread", Status: models.ThreadStatusBusy}); err != nil {
				t.Fatal(err)
			}
			if err := seedCheckpointRun(ctx, env.store, "checkpoint-run", "checkpoint-thread", 1); err != nil {
				t.Fatal(err)
			}
			if _, err := env.store.PutOpaqueCheckpoint(ctx, "checkpoint-thread", "adapter-state", []byte("saved"), "test", nil); err != nil {
				t.Fatal(err)
			}
			lookup := &checkpointFenceInflight{}
			lookup.generation.Store(1)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				env.apiServer.Handler().ServeHTTP(w, r)
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
				t.Fatal("DELETE did not pass middleware")
			}
			wantReason := auth.ReasonRunGenerationMismatch
			if terminal {
				wantReason = auth.ReasonRunNotInflight
				if result, err := env.store.CompleteRun(ctx, &state.RunCompletion{RunID: "checkpoint-run", Generation: 1, Status: "interrupted"}); err != nil || !result.Applied {
					t.Fatal(result, err)
				}
			} else if ok, err := env.store.BeginRunAttempt(ctx, "checkpoint-run", 2); err != nil || !ok {
				t.Fatal(ok, err)
			}
			unblock()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("DELETE did not finish")
			}
			var denial map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &denial); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusForbidden || denial["reason_code"] != wantReason {
				t.Fatalf("stale DELETE: %d %s", response.Code, response.Body)
			}
			if blob, err := env.store.GetOpaqueCheckpoint(ctx, "checkpoint-thread", "adapter-state"); err != nil || string(blob.Data) != "saved" {
				t.Fatal(blob, err)
			}
		})
	}
}
