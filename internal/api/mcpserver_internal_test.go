package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/tenant"
)

// TestMCPSessionOwner_SweepEvictsOnlyStaleEntries proves the session
// ownership map's own cleanup sweep evicts entries untouched since
// before mcpSessionTTL, and leaves recently-touched ones alone --
// otherwise this map (kept purely to close the cross-tenant session
// hijack this file's own middleware doc comment describes) would grow
// without bound for the lifetime of a long-running control plane.
func TestMCPSessionOwner_SweepEvictsOnlyStaleEntries(t *testing.T) {
	now := time.Now()
	o := &mcpSessionOwner{sessions: make(map[string]mcpSessionEntry), ttl: mcpSessionTTL}
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
	o := newMCPSessionOwner(mcpSessionTTL, mcpSessionSweepInterval)
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
	o := newMCPSessionOwner(mcpSessionTTL, mcpSessionSweepInterval)
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

// TestMCPSessionTimeouts_ProductionConstantsOrderedCorrectly is a
// regression test for a real gap a review found: mcpSDKSessionTimeout
// must stay strictly less than mcpSessionTTL, with a full sweep
// interval of margin -- not just "some positive value" -- see
// mcpSDKSessionTimeout's own doc comment for the mechanism this
// protects. A future edit to any of the three constants that violates
// this ordering reopens the hijack window they exist to close; this
// catches that at test time instead of only in a live 30-minute-later
// probe.
func TestMCPSessionTimeouts_ProductionConstantsOrderedCorrectly(t *testing.T) {
	if mcpSDKSessionTimeout >= mcpSessionTTL {
		t.Fatalf("mcpSDKSessionTimeout (%s) must be less than mcpSessionTTL (%s) -- otherwise an idle "+
			"SDK session can outlive its own ownership record", mcpSDKSessionTimeout, mcpSessionTTL)
	}
	if mcpSDKSessionTimeout > mcpSessionTTL-mcpSessionSweepInterval {
		t.Fatalf("mcpSDKSessionTimeout (%s) leaves less than a full sweep interval (%s) of margin below "+
			"mcpSessionTTL (%s) -- an ownership record could be evicted before the SDK session it "+
			"corresponds to has actually closed", mcpSDKSessionTimeout, mcpSessionSweepInterval, mcpSessionTTL)
	}
}

// TestMCPSDKSessionTimeout_ClosesBeforeHijackWindowReopens is the
// behavioral counterpart to the constants check above: proves that
// once an ownership record is genuinely swept away (not just
// mathematically "should be"), the underlying SDK session is ALSO
// already closed, so a mismatched caller presenting that now-unowned
// session ID does NOT get a successful dispatch -- reproducing, on a
// compressed timescale, the exact live gap a review found (passing nil
// for StreamableHTTPOptions left SDK sessions immortal, so they
// outlived their 30-minute ownership record and reopened the hijack
// window this whole file's middleware exists to close).
func TestMCPSDKSessionTimeout_ClosesBeforeHijackWindowReopens(t *testing.T) {
	s, _ := newLifecycleServer(t, nil)

	// Mirrors the production formula (mcpSDKSessionTimeout =
	// mcpSessionTTL - mcpSessionSweepInterval) on a scale a test can
	// actually wait out.
	const (
		sweepInterval = 30 * time.Millisecond
		ownershipTTL  = 150 * time.Millisecond
		sdkTimeout    = ownershipTTL - sweepInterval
	)

	base := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		server, err := s.newMCPServer(r.Context())
		if err != nil {
			t.Errorf("newMCPServer: %v", err)
			return nil
		}
		return server
	}, &mcp.StreamableHTTPOptions{SessionTimeout: sdkTimeout})

	owner := newMCPSessionOwner(ownershipTTL, sweepInterval)
	// Auth normally happens in the outer auth.Middleware, not this
	// file's own code -- stand in for it here with a trivial
	// header-to-context translation, since this test is specifically
	// about the ownership/SDK-timeout interaction, not auth itself.
	withTestCaller := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			caller := r.Header.Get("X-Test-Caller")
			ctx := auth.WithContext(tenant.WithContext(r.Context(), caller), &auth.AuthResult{Identity: caller})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	srv := httptest.NewServer(withTestCaller(owner.middleware(base)))
	defer srv.Close()
	// httptest.Server.Close blocks until every outstanding connection's
	// handler returns -- the client's own standalone GET stream is
	// exactly one such connection, and without a working SessionTimeout
	// fix it never returns on its own, hanging Close() forever instead
	// of failing the assertion below promptly. Force it closed first so
	// a REGRESSION here surfaces as a normal test failure, not a wedged
	// test binary (confirmed by reproducing the hang with SessionTimeout
	// reverted to nil while writing this test).
	defer srv.CloseClientConnections()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	httpClient := &http.Client{Transport: &headerRoundTripperInternal{header: "X-Test-Caller", value: "tenant-a"}}
	transport := &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: httpClient}
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	sessionID := session.ID()
	if sessionID == "" {
		t.Fatal("expected a non-empty session ID")
	}

	// The SDK client keeps a standalone GET stream open after Connect
	// (for server-to-client push) -- confirmed by instrumenting this
	// test that ownership's lastSeen keeps refreshing until THAT stream
	// itself ends, which (with no further client activity) only
	// happens once the server's own sdkTimeout fires and closes it,
	// not at the original Connect time. So the wait here has to clear
	// sdkTimeout first (for that refresh to happen and settle), THEN
	// ownershipTTL/sweep on top of it -- not just the latter on their
	// own, or this test flakes by checking before the entry's real,
	// final lastSeen is even set.
	time.Sleep(sdkTimeout + ownershipTTL + 6*sweepInterval)

	// Confirm the ownership record is ACTUALLY gone -- otherwise this
	// test would trivially pass for the wrong reason (still rejected
	// at the ownership layer, the SDK session's own state never
	// actually exercised).
	owner.mu.Lock()
	_, stillOwned := owner.sessions[sessionID]
	owner.mu.Unlock()
	if stillOwned {
		t.Fatal("expected the ownership record to have been swept by now -- test's own wait is too short")
	}

	// A mismatched caller presenting the now-unowned session ID must
	// NOT get a successful dispatch under tenant-a's now-abandoned
	// session -- the SDK's own session must already be closed too.
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL, body)
	req.Header.Set("X-Test-Caller", "tenant-b")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(mcpSessionIDHeader, sessionID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post-expiry request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("expected the SDK's own session to already be closed by now (not a 200 dispatch under " +
			"the original tenant's abandoned session)")
	}
}

type headerRoundTripperInternal struct {
	header, value string
}

func (h *headerRoundTripperInternal) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set(h.header, h.value)
	return http.DefaultTransport.RoundTrip(req)
}
