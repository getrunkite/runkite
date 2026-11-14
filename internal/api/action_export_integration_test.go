package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/getrunkite/runkite/internal/actionexport"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func TestDurableActionDeliveryRetryNeverRepeatsTool(t *testing.T) {
	env := newActionAPIEnv(t, "json", "policy")
	t.Setenv("RUNKITE_TEST_ACTION_EXPORT_KEY", "synthetic-delivery-signing-key")
	var mu sync.Mutex
	seen := map[string]int{}
	fail := true
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload state.ActionDeliveryPayload
		if json.Unmarshal(raw, &payload) != nil || payload.Schema != "runkite.action-event.v1" || payload.DeliveryID == "" || payload.DeliveryID != r.Header.Get("X-Runkite-Delivery-Id") {
			t.Error("delivery identity missing")
		}
		mac := hmac.New(sha256.New, []byte("synthetic-delivery-signing-key"))
		_, _ = mac.Write(raw)
		if r.Header.Get("X-Runkite-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
			t.Error("delivery signature invalid")
		}
		if strings.Contains(string(raw), "synthetic-sensitive") {
			t.Error("evidence delivery exposed fixture payload")
		}
		mu.Lock()
		seen[payload.DeliveryID]++
		bad := fail
		mu.Unlock()
		if bad {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(204)
	}))
	defer endpoint.Close()
	cfg := actionexport.Config{Name: "security-test", URL: endpoint.URL, SecretRef: "env:RUNKITE_TEST_ACTION_EXPORT_KEY", AllowInsecureLoopback: true, MaxAttempts: 1}
	worker, err := actionexport.New(env.store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	env.s.SetActionExportSinkID(worker.SinkID())
	if response := env.call(t); response.Code != 200 || env.effects.Load() != 1 {
		t.Fatal("fixture tool did not run exactly once")
	}
	system := tenant.SystemContext(context.Background())
	attempted, err := worker.RunOnce(system)
	if !attempted || err == nil {
		t.Fatal("failed endpoint did not retain delivery failure")
	}
	ctx := tenant.ScopedContext(context.Background(), env.binding.TenantID)
	rows, err := env.store.SearchActionDeliveries(ctx, &state.ActionDeliverySearchRequest{TenantID: env.binding.TenantID, Status: state.ActionDeliveryDeadLetter})
	if err != nil || len(rows) != 1 {
		t.Fatal("failed delivery was not retained")
	}
	retryID := rows[0].ID
	if err := env.store.RetryActionDelivery(ctx, env.binding.TenantID, retryID); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	for range 20 {
		more, e := worker.RunOnce(system)
		if e != nil {
			t.Fatal(e)
		}
		if !more {
			break
		}
	}
	mu.Lock()
	received := seen[retryID]
	mu.Unlock()
	if received != 2 || env.effects.Load() != 1 {
		t.Fatal("evidence retry lost identity or repeated the protected tool")
	}
	remaining, err := env.store.SearchActionDeliveries(ctx, &state.ActionDeliverySearchRequest{TenantID: env.binding.TenantID, Status: state.ActionDeliveryPending})
	if err != nil || len(remaining) != 0 {
		t.Fatal("acknowledged events remained pending")
	}

	// Changing a URL under the same logical name cannot claim the original queue.
	cfg.URL = endpoint.URL + "/other"
	second, err := actionexport.New(env.store, cfg)
	if err != nil || second.SinkID() == worker.SinkID() {
		t.Fatal("changed destination reused queue identity")
	}
}
