package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/auth"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// TestMCPSessionOwner_SweepEvictsOnlyStaleEntries proves the session
// ownership map's own cleanup sweep evicts entries untouched since
// before mcpSessionTTL, and leaves recently-touched ones alone --
// otherwise this map (kept purely to close the cross-tenant session
// hijack this file's own middleware doc comment describes) would grow
// without bound for the lifetime of a long-running control plane.
func TestMCPSessionOwner_SweepEvictsOnlyStaleEntries(t *testing.T) {
	o := &mcpSessionOwner{sessions: make(map[string]mcpSessionEntry)}
	now := time.Now()

	o.sessions["stale"] = mcpSessionEntry{caller: "tenant-a|user1", lastSeen: now.Add(-mcpSessionTTL - time.Minute)}
	o.sessions["fresh"] = mcpSessionEntry{caller: "tenant-a|user1", lastSeen: now.Add(-time.Minute)}
	o.sessions["boundary"] = mcpSessionEntry{caller: "tenant-a|user1", lastSeen: now.Add(-mcpSessionTTL + time.Second)}

	o.sweepOnce(now)

	if _, ok := o.sessions["stale"]; ok {
		t.Error("expected the stale entry (older than TTL) to be evicted")
	}
	if _, ok := o.sessions["fresh"]; !ok {
		t.Error("expected the fresh entry to survive the sweep")
	}
	if _, ok := o.sessions["boundary"]; !ok {
		t.Error("expected the just-inside-TTL entry to survive the sweep")
	}
}

// TestMCPSessionOwner_MiddlewareTracksNewSessionFromResponseHeader
// proves the middleware records ownership from the RESPONSE header the
// SDK sets when it assigns a brand-new session ID -- there's no way to
// know a session's ID before that first response, so this is the only
// point the middleware CAN learn it.
func TestMCPSessionOwner_MiddlewareTracksNewSessionFromResponseHeader(t *testing.T) {
	o := newMCPSessionOwner()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(mcpSessionIDHeader, "new-session-id")
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req = req.WithContext(auth.WithContext(tenant.WithContext(req.Context(), "tenant-a"), &auth.AuthResult{Identity: "user1"}))
	rec := httptest.NewRecorder()
	o.middleware(next).ServeHTTP(rec, req)

	o.mu.Lock()
	entry, ok := o.sessions["new-session-id"]
	o.mu.Unlock()
	if !ok {
		t.Fatal("expected the new session ID to be recorded after the response set it")
	}
	if entry.caller != "tenant-a|user1" {
		t.Fatalf("expected caller=tenant-a|user1, got %q", entry.caller)
	}
}

// TestMCPSessionOwner_MiddlewareAllowsUnknownSessionThrough proves an
// Mcp-Session-Id the middleware has never seen (e.g. this process just
// restarted, or the SDK's own routing will reject it anyway) is passed
// through rather than rejected outright -- this middleware's job is
// catching a MISMATCH against a KNOWN owner, not acting as its own
// session-existence check (the SDK's own "session not found" already
// covers that).
func TestMCPSessionOwner_MiddlewareAllowsUnknownSessionThrough(t *testing.T) {
	o := newMCPSessionOwner()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set(mcpSessionIDHeader, "never-seen-before")
	req = req.WithContext(auth.WithContext(tenant.WithContext(req.Context(), "tenant-a"), &auth.AuthResult{Identity: "user1"}))
	rec := httptest.NewRecorder()
	o.middleware(next).ServeHTTP(rec, req)

	if !called {
		t.Fatal("expected an unrecognized session ID to be passed through to the wrapped handler")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}
