package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type staticGate struct {
	dec Decision
}

func (g staticGate) Decide(context.Context, Event) Decision { return g.dec }

type hangGate struct{}

func (hangGate) Decide(ctx context.Context, _ Event) Decision {
	<-ctx.Done()
	return Decision{Allow: true} // unreachable when ctx cancelled first
}

func TestCheckBeforeRun_NilAndEmptyAllow(t *testing.T) {
	var d *Dispatcher
	if err := d.CheckBeforeRun(context.Background(), Event{Type: BeforeRun}); err != nil {
		t.Fatalf("nil dispatcher: %v", err)
	}
	d = NewDispatcher()
	if err := d.CheckBeforeRun(context.Background(), Event{Type: BeforeRun}); err != nil {
		t.Fatalf("empty gates: %v", err)
	}
}

func TestCheckBeforeRun_Deny(t *testing.T) {
	d := NewDispatcher()
	d.RegisterGate(staticGate{dec: Decision{Allow: false, Reason: "blocked by policy"}})
	err := d.CheckBeforeRun(context.Background(), Event{Type: BeforeRun, RunID: "r1"})
	var denied *ErrDenied
	if !errors.As(err, &denied) {
		t.Fatalf("want ErrDenied, got %v", err)
	}
	if denied.Reason != "blocked by policy" {
		t.Errorf("reason = %q", denied.Reason)
	}
}

func TestCheckBeforeRun_AllMustAllow(t *testing.T) {
	d := NewDispatcher()
	d.RegisterGate(staticGate{dec: Decision{Allow: true}})
	d.RegisterGate(staticGate{dec: Decision{Allow: false, Reason: "second gate"}})
	err := d.CheckBeforeRun(context.Background(), Event{Type: BeforeRun})
	var denied *ErrDenied
	if !errors.As(err, &denied) || denied.Reason != "second gate" {
		t.Fatalf("got %v", err)
	}
}

func TestCheckBeforeRun_TimeoutFailClosed(t *testing.T) {
	d := NewDispatcher()
	d.RegisterGate(hangGate{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := d.CheckBeforeRun(ctx, Event{Type: BeforeRun})
	var denied *ErrDenied
	if !errors.As(err, &denied) {
		t.Fatalf("want ErrDenied on timeout, got %v", err)
	}
}

func TestWebhookGate_AllowAndDeny(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		var ev Event
		if err := json.Unmarshal(body, &ev); err != nil {
			t.Errorf("bad event: %v", err)
		}
		if ev.Type != BeforeRun {
			t.Errorf("type = %q", ev.Type)
		}
		if r.Header.Get("X-Runkite-Signature") == "" {
			t.Error("missing signature")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"allow": hits.Load() == 1, "reason": "nope"})
	}))
	defer srv.Close()

	gate := NewWebhookGate(WebhookGateConfig{URL: srv.URL, Secret: "s3cret", Timeout: time.Second})
	dec := gate.Decide(context.Background(), Event{Type: BeforeRun, RunID: "r1", AgentID: "echo"})
	if !dec.Allow {
		t.Fatalf("first call should allow, got %+v", dec)
	}
	dec = gate.Decide(context.Background(), Event{Type: BeforeRun, RunID: "r2"})
	if dec.Allow || dec.Reason != "nope" {
		t.Fatalf("second call should deny: %+v", dec)
	}
}

func TestWebhookGate_HTTPErrorFailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	defer srv.Close()

	gate := NewWebhookGate(WebhookGateConfig{URL: srv.URL, Timeout: time.Second})
	dec := gate.Decide(context.Background(), Event{Type: BeforeRun})
	if dec.Allow {
		t.Fatal("non-2xx must deny")
	}
}

func TestWebhookGate_MissingAllowFailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"reason":"forgot allow"}`))
	}))
	defer srv.Close()

	gate := NewWebhookGate(WebhookGateConfig{URL: srv.URL, Timeout: time.Second})
	dec := gate.Decide(context.Background(), Event{Type: BeforeRun})
	if dec.Allow {
		t.Fatal("missing allow must deny")
	}
}
