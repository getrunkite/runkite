package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/transport"
)

// TestHandleLivez_AlwaysReadyRegardlessOfDependencyHealth proves /livez
// stays cheap and unconditional -- it must never fail just because a
// downstream store/queue is down, since that's exactly what would turn
// a transient dependency outage into an orchestrator restart-crash-loop
// (see handleLivez's own doc comment).
func TestHandleLivez_AlwaysReadyRegardlessOfDependencyHealth(t *testing.T) {
	s, _ := newLifecycleServer(t, failPingQueue{err: errors.New("queue down")})
	s.store = failPingStore{err: errors.New("store down")}

	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected /livez to always return 200, got %d", rec.Code)
	}
}

// TestHandleReadyz_AllDependenciesHealthy_Returns200 is the happy path:
// a real sqlite store and in-process queue/broker/cancelbus (from
// newLifecycleServer) are all reachable, so /readyz must say so.
func TestHandleReadyz_AllDependenciesHealthy_Returns200(t *testing.T) {
	s, _ := newLifecycleServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected /readyz to return 200 when all dependencies are healthy, got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "ready" {
		t.Errorf("expected status=ready, got %v", body["status"])
	}
	checks, _ := body["checks"].(map[string]interface{})
	if checks["store"] != "ok" || checks["queue"] != "ok" {
		t.Errorf("expected store and queue checks both ok, got %v", checks)
	}
}

// TestHandleReadyz_StoreDown_Returns503 proves a single failing
// dependency is enough to flip the whole probe to not-ready, and that
// the failure is attributed to the right check rather than lumped
// together -- an operator debugging a real outage needs to know WHICH
// backend is unreachable, not just that something is.
func TestHandleReadyz_StoreDown_Returns503(t *testing.T) {
	s, _ := newLifecycleServer(t, nil)
	s.store = failPingStore{err: errors.New("connection refused")}

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when store is down, got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &body)
	checks, _ := body["checks"].(map[string]interface{})
	if checks["store"] == "ok" {
		t.Error("expected store check to report the failure, not ok")
	}
	if checks["queue"] != "ok" {
		t.Errorf("expected queue check to still report ok independently, got %v", checks["queue"])
	}
}

// TestHandleReadyz_QueueDown_Returns503 mirrors the store case for the
// queue side of the same dependency-attribution contract.
func TestHandleReadyz_QueueDown_Returns503(t *testing.T) {
	s, _ := newLifecycleServer(t, failPingQueue{err: errors.New("no route to host")})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when queue is down, got %d, body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleReadyz_OptionalBrokerPingIsOnlyCheckedWhenImplemented proves
// the pinger type-assertion in handleReadyz doesn't panic or misbehave
// against the in-process broker/cancelbus, which deliberately don't
// implement Ping at all (see pinger's own doc comment) -- readiness
// must still succeed on that combination, not silently skip a check it
// meant to run.
func TestHandleReadyz_OptionalBrokerPingIsOnlyCheckedWhenImplemented(t *testing.T) {
	s, _ := newLifecycleServer(t, nil) // in-process broker/cancelbus: no Ping method

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 against in-process broker/cancelbus (no Ping method), got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &body)
	checks, _ := body["checks"].(map[string]interface{})
	if _, present := checks["event_broker"]; present {
		t.Errorf("expected no event_broker check for a broker without Ping, got %v", checks)
	}
}

// failPingStore wraps a real store but forces Ping to fail, to test
// /readyz's failure path without needing an actual broken database.
type failPingStore struct {
	state.Store
	err error
}

func (f failPingStore) Ping(ctx context.Context) error { return f.err }

// failPingQueue wraps the transport.JobQueue interface minimally,
// forcing Ping to fail -- separate from lifecycle_regression_test.go's
// failQueue (which fails Enqueue/Dequeue, not Ping, for a different
// test's purposes).
type failPingQueue struct {
	transport.JobQueue
	err error
}

func (f failPingQueue) Ping(ctx context.Context) error { return f.err }
