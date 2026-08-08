package policy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/policy"
)

func TestDecide_PendingNotCached(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"effect":"pending","reason":"wait"}`))
	}))
	t.Cleanup(srv.Close)

	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "sf",
		}},
		Webhook:  &policy.WebhookConfig{URL: srv.URL},
		CacheTTL: time.Hour,
	})
	in := policy.PolicyInput{
		Stage: policy.StageToolCall, TenantID: "acme", AgentID: "sales",
		Connector: "sf", Tool: "delete_repo",
	}
	for i := 0; i < 3; i++ {
		dec := eng.Decide(context.Background(), in)
		if dec.Effect != policy.EffectPending {
			t.Fatalf("call %d: effect=%q want pending", i, dec.Effect)
		}
		if dec.ReasonCode != policy.ReasonPolicyPending {
			t.Fatalf("call %d: reason_code=%q", i, dec.ReasonCode)
		}
	}
	if hits.Load() != 3 {
		t.Fatalf("webhook hits=%d want 3 (pending must not be cached)", hits.Load())
	}
}
