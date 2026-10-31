package policy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestPolicyWebhookDoesNotFollowRedirectedAllow(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = w.Write([]byte(`{"allow":true}`)) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(307)
	}))
	defer origin.Close()
	decision := NewWebhook(WebhookConfig{URL: origin.URL, Secret: "synthetic"}).Decide(context.Background(), PolicyInput{}, true)
	if decision.Effect != EffectDeny || hits.Load() != 0 {
		t.Fatalf("redirected policy accepted: %+v hits=%d", decision, hits.Load())
	}
}

func TestPolicyWebhookRejectsUnknownEffect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"effect":"unexpected"}`)) }))
	defer server.Close()
	webhook := NewWebhook(WebhookConfig{URL: server.URL})
	if got := webhook.Decide(context.Background(), PolicyInput{}, true); got.Effect != EffectDeny {
		t.Fatalf("invalid effect accepted: %+v", got)
	}
	if got := webhook.Decide(context.Background(), PolicyInput{}, false); got.Effect != EffectAllow {
		t.Fatal("explicit fail-open behavior changed")
	}
}
