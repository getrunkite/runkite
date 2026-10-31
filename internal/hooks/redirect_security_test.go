package hooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHookGateAndSinkDoNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = w.Write([]byte(`{"allow":true}`)) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(307)
	}))
	defer origin.Close()
	gate := NewWebhookGate(WebhookGateConfig{URL: origin.URL})
	if decision := gate.Decide(context.Background(), Event{}); decision.Allow {
		t.Fatal("redirected preflight allowed")
	}
	sink := NewWebhookSink(WebhookConfig{URL: origin.URL}, nil)
	if err := sink.deliver(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("redirected webhook accepted")
	}
	if hits.Load() != 0 {
		t.Fatal("hook payload sent to another origin")
	}
}
