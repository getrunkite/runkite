package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
)

func TestWebhookCacheBindsCookieAndCustomIdentityHeaders(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var body struct {
			Headers map[string]string `json:"headers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"allow": true, "user": map[string]any{"identity": body.Headers["Cookie"] + body.Headers["X-User"]}})
	}))
	t.Cleanup(up.Close)
	p := auth.NewWebhookProvider(auth.WebhookConfig{URL: up.URL, CacheTTLSeconds: 60})
	for _, identity := range []string{"alice", "bob", "alice"} {
		r := httptest.NewRequest("GET", "/threads", nil)
		r.Header.Set("Cookie", "session="+identity)
		r.Header.Set("X-User", identity)
		result, err := p.Authenticate(r)
		if err != nil || result.Identity != "session="+identity+identity {
			t.Fatalf("cache mixed identities: %+v %v", result, err)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("cache hits=%d want two distinct request contexts", hits.Load())
	}
}

func TestWebhookRequiresBoundedSuccessfulResponseFromConfiguredOrigin(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		_, _ = w.Write([]byte(`{"allow":true}`))
	}))
	t.Cleanup(target.Close)
	for _, mode := range []string{"status", "redirect", "trailing", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "status":
					w.WriteHeader(503)
				case "redirect":
					w.Header().Set("Location", target.URL)
					w.WriteHeader(307)
				case "trailing":
					_, _ = w.Write([]byte(`{"allow":true} {}`))
					return
				case "oversized":
					_, _ = w.Write([]byte(strings.Repeat(" ", 1<<20)))
				}
				_, _ = w.Write([]byte(`{"allow":true}`))
			}))
			defer up.Close()
			p := auth.NewWebhookProvider(auth.WebhookConfig{URL: up.URL})
			if result, err := p.Authenticate(httptest.NewRequest("GET", "/threads", nil)); err == nil || result != nil {
				t.Fatalf("accepted unsafe auth result: %+v %v", result, err)
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("auth context sent to redirect target")
	}
}

func TestCancelledWebhookRequestDoesNotPoisonAuthCache(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"allow":true,"user":{"identity":"valid"}}`))
	}))
	defer up.Close()
	p := auth.NewWebhookProvider(auth.WebhookConfig{URL: up.URL, CacheTTLSeconds: 60})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("GET", "/threads", nil)
	if _, err := p.Authenticate(r.WithContext(ctx)); err == nil {
		t.Fatal("cancelled request authenticated")
	}
	if result, err := p.Authenticate(r); err != nil || result.Identity != "valid" {
		t.Fatalf("cancelled request cached a shared denial: %+v %v", result, err)
	}
}
