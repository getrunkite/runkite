package connector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestGetSession_CircuitBreakerTripsAfterRepeatedFailures proves the
// breaker is really wired into GetSession's client_credentials path: after
// FailureThreshold consecutive failed token fetches, further calls fail
// fast with ErrCircuitOpen instead of hitting the (still-broken) endpoint
// again.
func TestGetSession_CircuitBreakerTripsAfterRepeatedFailures(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewRegistry(map[string]ConnectorConfig{
		"flaky": {
			Auth: AuthConfig{
				Type: "oauth2_client_credentials", TokenURL: srv.URL,
				ClientID: "id", ClientSecret: "secret",
			},
			CircuitBreaker: &CircuitBreakerConfig{FailureThreshold: 2, CooldownSeconds: 30},
		},
	})

	// First 2 calls hit the real endpoint and fail normally.
	for i := 0; i < 2; i++ {
		if _, err := r.GetSession(context.Background(), "flaky", nil); err == nil {
			t.Fatalf("call %d: expected error from broken endpoint", i)
		}
	}
	if r.BreakerState("flaky") != "open" {
		t.Fatalf("expected breaker to be open after threshold failures, got %s", r.BreakerState("flaky"))
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly 2 real calls to the endpoint, got %d", got)
	}

	// Third call must fail FAST via the breaker, not hit the endpoint again.
	_, err := r.GetSession(context.Background(), "flaky", nil)
	if err == nil {
		t.Fatal("expected an error once the breaker is open")
	}
	var circuitOpen *ErrCircuitOpen
	if !errors.As(err, &circuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %T: %v", err, err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected NO additional call to the endpoint once breaker is open, got %d total calls", got)
	}
}

// TestGetSession_CachedTokenBypassesOpenBreaker proves a previously cached
// (still-valid) token keeps working even after the breaker trips on a
// later refresh attempt -- the breaker only guards the network call, not
// the fast cache-hit path.
func TestGetSession_CachedTokenBypassesOpenBreaker(t *testing.T) {
	good := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if good {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"tok1","expires_in":7200}`))
			return
		}
		http.Error(w, "broken now", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewRegistry(map[string]ConnectorConfig{
		"svc": {
			Auth: AuthConfig{Type: "oauth2_client_credentials", TokenURL: srv.URL, ClientID: "id", ClientSecret: "secret"},
		},
	})

	sess, err := r.GetSession(context.Background(), "svc", nil)
	if err != nil || sess.Credentials["access_token"] != "tok1" {
		t.Fatalf("expected successful first fetch, got sess=%v err=%v", sess, err)
	}

	// Endpoint breaks, but since the cached token is still valid (7200s
	// expiry, 60s buffer), GetSession must keep returning it without ever
	// touching the (now broken) endpoint or the breaker.
	good = false
	sess2, err := r.GetSession(context.Background(), "svc", nil)
	if err != nil || sess2.Credentials["access_token"] != "tok1" {
		t.Fatalf("expected cached token despite broken endpoint, got sess=%v err=%v", sess2, err)
	}
}
