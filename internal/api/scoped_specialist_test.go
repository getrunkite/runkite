package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

func specialistRequest(h http.Handler, method, path, key, reference, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	if reference != "" {
		r.Header.Set("Runkite-Expected-Run-Reference", reference)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestScopedSpecialistObservationIsolationAndReference(t *testing.T) {
	f := newMCPRetryFixture(t)
	f.seedRun(t, context.Background(), "retained", "thread", "mcp-agent", models.RunStatusSuccess, `{"message":"input"}`, `{"answer":"private"}`)
	f.seedRun(t, context.Background(), "encoded/run", "encoded-thread", "mcp-agent", models.RunStatusSuccess, `{}`, `{"answer":"encoded"}`)
	f.seedRun(t, context.Background(), "foreign-agent", "other-thread", "other-agent", models.RunStatusSuccess, `{}`, `{"answer":"foreign"}`)
	f.seedRun(t, context.Background(), "whitespace-agent", "whitespace-thread", " mcp-agent ", models.RunStatusSuccess, `{}`, `{"answer":"foreign-whitespace"}`)
	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"run":          {Name: "caller", Permissions: []string{"agents:mcp-agent:run"}},
		"observe":      {Name: "caller", Permissions: []string{"agents:mcp-agent:observe"}},
		"other-tenant": {Name: "caller", TenantID: "other", Permissions: []string{"agents:mcp-agent:observe"}},
		"broad":        {Name: "caller", Permissions: []string{"read"}},
	})
	h := auth.Middleware(p, nil, nil, f.plane.Handler())
	for _, tc := range []struct {
		key, path, reference string
		status               int
	}{
		{"run", "/runs/retained", "", 403}, {"observe", "/runs/foreign-agent", "", 403},
		{"observe", "/runs/whitespace-agent", "", 403},
		{"other-tenant", "/runs/retained", "", 404}, {"observe", "/runs/retained/wait", "", 428},
		{"observe", "/runs/retained", "wrong", 412}, {"broad", "/runs/retained", "wrong", 412},
		{"broad", "/runs/retained/wait", "", 200},
	} {
		w := specialistRequest(h, "GET", tc.path, tc.key, tc.reference, "")
		if w.Code != tc.status {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
	}
	encoded := specialistRequest(h, "GET", "/runs/encoded%2Frun", "observe", "", "")
	if encoded.Code != 200 || !strings.Contains(encoded.Body.String(), "encoded") {
		t.Fatal(encoded.Code, encoded.Body.String())
	}
	first := specialistRequest(h, "GET", "/runs/retained", "observe", "", "")
	ref := first.Header().Get("Runkite-Run-Reference")
	if first.Code != 200 || ref == "" {
		t.Fatal(first.Code, first.Body.String())
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			w := specialistRequest(h, "GET", "/runs/retained/wait", "observe", ref, "")
			if w.Code != 200 || !strings.Contains(w.Body.String(), "private") {
				t.Errorf("observation: %d %s", w.Code, w.Body.String())
			}
		})
	}
	wg.Wait()
	if f.queue.enqueued.Load() != 0 {
		t.Fatal("observation dispatched")
	}
	original, _ := f.store.GetRun(context.Background(), "retained")
	if err := f.store.DeleteRun(context.Background(), "retained"); err != nil {
		t.Fatal(err)
	}
	missing := specialistRequest(h, "GET", "/runs/retained/wait", "observe", ref, "")
	if missing.Code != 404 {
		t.Fatal(missing.Code)
	}
	replacement := *original
	replacement.Status = models.RunStatusSuccess
	replacement.Output = json.RawMessage(`{"answer":"replacement"}`)
	if err := f.store.CreateRun(context.Background(), &replacement); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"observe", "broad"} {
		for _, suffix := range []string{"", "/wait"} {
			w := specialistRequest(h, "GET", "/runs/retained"+suffix, key, ref, "")
			if w.Code != 412 || strings.Contains(w.Body.String(), "replacement") {
				t.Fatal(key, suffix, w.Code, w.Body.String())
			}
		}
	}
	if f.queue.enqueued.Load() != 0 {
		t.Fatal("missing/replaced observation dispatched")
	}
}

func TestScopedSpecialistMCPDiscoveryAndAuthoritySnapshot(t *testing.T) {
	f := newMCPRetryFixture(t)
	provider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"run":       {Name: "caller", Permissions: []string{"agents:mcp-agent:run", "agents:mcp-agent:observe"}},
		"reordered": {Name: "caller", Permissions: []string{"agents:mcp-agent:observe", "agents:mcp-agent:run", "agents:mcp-agent:run"}},
		"changed":   {Name: "caller", Permissions: []string{"agents:other-agent:run"}},
	})
	h := auth.Middleware(provider, nil, nil, f.plane.Handler())
	call := func(key, session, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			r.Header.Set("Mcp-Session-Id", session)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	init := call("run", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`)
	session := init.Header().Get("Mcp-Session-Id")
	if init.Code != 200 || session == "" {
		t.Fatal(init.Code, init.Body.String())
	}
	call("run", session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	list := call("reordered", session, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"name":"mcp-agent"`) || strings.Contains(list.Body.String(), `"name":"other-agent"`) {
		t.Fatal(list.Code, list.Body.String())
	}
	denied := call("run", session, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"other-agent","arguments":{"message":"deny"}}}`)
	if denied.Code != 200 || !strings.Contains(denied.Body.String(), "error") || f.queue.enqueued.Load() != 0 {
		t.Fatal(denied.Code, denied.Body.String())
	}
	changed := call("changed", session, `{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}`)
	if changed.Code != 403 {
		t.Fatal(changed.Code, changed.Body.String())
	}
}

func TestScopedSpecialistAuthorityIncludesIdentityProvenance(t *testing.T) {
	ctx := tenant.WithContext(context.Background(), "tenant")
	a := &auth.AuthResult{Identity: "caller", Permissions: []string{"agents:a:run"}}
	baseline := mcpAuthorityKey(auth.WithContext(ctx, a))
	unverified := *a
	auth.MarkIdentityUnverified(&unverified)
	if baseline == mcpAuthorityKey(auth.WithContext(ctx, &unverified)) {
		t.Fatal("verification distinction lost")
	}
	withIssuer := *a
	withIssuer.Provenance = &models.IdentityProvenance{Issuer: "https://issuer-a", Subject: "caller", Audience: "runkite", ExpiresAt: time.Now().Add(time.Hour)}
	issuerKey := mcpAuthorityKey(auth.WithContext(ctx, &withIssuer))
	otherIssuer := withIssuer
	provenance := *withIssuer.Provenance
	provenance.Issuer = "https://issuer-b"
	otherIssuer.Provenance = &provenance
	if issuerKey == mcpAuthorityKey(auth.WithContext(ctx, &otherIssuer)) {
		t.Fatal("issuer distinction lost")
	}
	if mcpAuthorityKey(auth.WithContext(ctx, &auth.AuthResult{Identity: "caller"})) != mcpAuthorityKey(auth.WithContext(ctx, &auth.AuthResult{Identity: "caller", Permissions: []string{}})) {
		t.Fatal("nil and empty permissions differ")
	}
	changed := *a
	changed.Extra = map[string]interface{}{"department": "other"}
	if baseline == mcpAuthorityKey(auth.WithContext(ctx, &changed)) {
		t.Fatal("claim distinction lost")
	}
}

func TestRunObservationFencesReplacementDuringWaitAndCompletion(t *testing.T) {
	f := newMCPRetryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	f.seedRun(t, ctx, "waiting", "thread", "mcp-agent", models.RunStatusRunning, `{}`, `{}`)
	original, _ := f.store.GetRun(ctx, "waiting")
	if err := f.store.DeleteRun(ctx, "waiting"); err != nil {
		t.Fatal(err)
	}
	replacement := *original
	replacement.Status = models.RunStatusRunning
	if err := f.store.CreateRun(ctx, &replacement); err != nil {
		t.Fatal(err)
	}
	_, err := f.plane.waitForRunObservation(ctx, original)
	var conflict *state.ErrConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("replacement wait: %v", err)
	}
	// A replacement after the API's last read must also be rejected under the
	// completion transaction lock, even when generation and all public IDs match.
	result, err := f.store.CompleteRun(ctx, &state.RunCompletion{TrustedSystem: true, ExpectedIncarnationID: original.IncarnationID, RunID: original.RunID, Status: models.RunStatusSuccess, Values: map[string]interface{}{"leak": "old"}})
	if !errors.As(err, &conflict) || result.Applied {
		t.Fatalf("stale completion: %+v %v", result, err)
	}
	current, _ := f.store.GetRun(ctx, original.RunID)
	if current.Status != models.RunStatusRunning || len(current.Output) != 0 {
		t.Fatalf("replacement mutated: %+v", current)
	}
}

type observationCompletionRaceStore struct {
	state.Store
	replace func()
}

func (s *observationCompletionRaceStore) CompleteRun(ctx context.Context, c *state.RunCompletion) (state.CompletionResult, error) {
	s.replace()
	return s.Store.CompleteRun(ctx, c)
}

func TestRunObservationCompletionRaceCannotSettleReplacement(t *testing.T) {
	f := newMCPRetryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	f.seedRun(t, ctx, "completion-race", "thread", "mcp-agent", models.RunStatusRunning, `{}`, `{}`)
	original, _ := f.store.GetRun(ctx, "completion-race")
	f.plane.store = &observationCompletionRaceStore{Store: f.store, replace: func() {
		if err := f.store.DeleteRun(ctx, original.RunID); err != nil {
			t.Fatal(err)
		}
		replacement := *original
		if err := f.store.CreateRun(ctx, &replacement); err != nil {
			t.Fatal(err)
		}
	}}
	if err := f.broker.Publish(ctx, original.RunID, &transport.RunEvent{EventID: "stale-values", Seq: 1, Method: "values", Data: json.RawMessage(`{"answer":"old"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := f.broker.Publish(ctx, original.RunID, &transport.RunEvent{EventID: "stale-end", Seq: 2, Method: "end", Data: json.RawMessage(`{"status":"success"}`)}); err != nil {
		t.Fatal(err)
	}
	result, err := f.plane.waitForRunObservation(ctx, original)
	if err == nil || result != nil {
		t.Fatalf("replacement became observed result: %+v %v", result, err)
	}
	replacement, _ := f.store.GetRun(ctx, original.RunID)
	if replacement.IncarnationID == original.IncarnationID || replacement.Status != models.RunStatusRunning || len(replacement.Output) != 0 {
		t.Fatalf("replacement mutated: %+v", replacement)
	}
	if f.queue.enqueued.Load() != 0 {
		t.Fatal("observation dispatched")
	}
}

func TestScopedSpecialistBroadAndLocalCompatibility(t *testing.T) {
	f := newMCPRetryFixture(t)
	f.seedRun(t, context.Background(), "compatibility", "thread", "mcp-agent", models.RunStatusSuccess, `{}`, `{"answer":"durable"}`)
	for _, permission := range []string{"read", "write", "admin", "local"} {
		h := f.plane.Handler()
		if permission != "local" {
			p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{"key": {Name: "caller", Permissions: []string{permission}}})
			h = auth.Middleware(p, nil, nil, h)
		}
		for _, suffix := range []string{"", "/wait"} {
			w := specialistRequest(h, "GET", "/runs/compatibility"+suffix, "key", "", "")
			if w.Code != 200 || !strings.Contains(w.Body.String(), "durable") {
				t.Fatalf("%s%s: %d %s", permission, suffix, w.Code, w.Body.String())
			}
		}
	}
}
