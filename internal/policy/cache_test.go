package policy_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/policy"
)

// Regression: cache must not share decisions across principals. A BYO
// webhook PDP can allow Alice / deny Bob for the same (tenant, agent,
// connector, tool); omitting Principal from the cache key would leak
// Alice's allow to Bob until TTL.
func TestDecide_CacheKeyIncludesPrincipal(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		payload := string(raw)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(payload, `"principal":"alice"`) ||
			strings.Contains(payload, `"identity":"alice"`) {
			_, _ = w.Write([]byte(`{"effect":"allow"}`))
			return
		}
		_, _ = w.Write([]byte(`{"effect":"deny","reason_code":"policy_denied","reason":"bob denied"}`))
	}))
	t.Cleanup(srv.Close)

	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "sf",
		}},
		Webhook:  &policy.WebhookConfig{URL: srv.URL},
		CacheTTL: time.Hour,
	})

	base := policy.PolicyInput{
		Stage: policy.StageToolCall, TenantID: "acme", AgentID: "sales",
		Connector: "sf", Tool: "query",
	}
	alice := base
	alice.Principal = "alice"
	bob := base
	bob.Principal = "bob"

	decA := eng.Decide(context.Background(), alice)
	if decA.Effect != policy.EffectAllow {
		t.Fatalf("alice: effect=%q want allow", decA.Effect)
	}
	decB := eng.Decide(context.Background(), bob)
	if decB.Effect != policy.EffectDeny {
		t.Fatalf("bob: effect=%q want deny (must not reuse alice's cached allow)", decB.Effect)
	}

	before := hits.Load()
	decA2 := eng.Decide(context.Background(), alice)
	if decA2.Effect != policy.EffectAllow {
		t.Fatalf("alice cached: effect=%q", decA2.Effect)
	}
	if hits.Load() != before {
		t.Fatalf("alice second call should be cached; webhook hits grew %d → %d", before, hits.Load())
	}
	if hits.Load() < 2 {
		t.Fatalf("webhook hits=%d want at least 2 (alice + bob)", hits.Load())
	}
}
