package policy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/policy"
)

func TestDecide_CacheKeyIncludesArgsDigest(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		bodies = append(bodies, string(raw))
		var env struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(raw, &env)
		w.Header().Set("Content-Type", "application/json")
		digest, _ := env.Data["args_digest"].(string)
		if digest == "high" {
			_, _ = w.Write([]byte(`{"effect":"deny","reason_code":"too_high"}`))
			return
		}
		_, _ = w.Write([]byte(`{"effect":"allow"}`))
	}))
	t.Cleanup(srv.Close)

	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "acme", AgentID: "payroll", Connector: "bank",
		}},
		Webhook:  &policy.WebhookConfig{URL: srv.URL},
		CacheTTL: time.Hour,
	})
	base := policy.PolicyInput{
		Stage: policy.StageToolCall, TenantID: "acme", AgentID: "payroll",
		Connector: "bank", Tool: "transfer", Principal: "alice",
	}
	low := base
	low.ArgsDigest = "low"
	low.ArgsMeta = map[string]any{"amount": float64(50)}
	high := base
	high.ArgsDigest = "high"
	high.ArgsMeta = map[string]any{"amount": float64(250)}

	if dec := eng.Decide(context.Background(), low); dec.Effect != policy.EffectAllow {
		t.Fatalf("low: %+v", dec)
	}
	if dec := eng.Decide(context.Background(), high); dec.Effect != policy.EffectDeny {
		t.Fatalf("high must not reuse low cache: %+v", dec)
	}
	n := len(bodies)
	_ = eng.Decide(context.Background(), low)
	if len(bodies) != n {
		t.Fatalf("low should be cached; webhook bodies %d → %d", n, len(bodies))
	}
	if n < 2 {
		t.Fatalf("want webhook for both digests, got %d", n)
	}
	joined := strings.Join(bodies, "\n")
	if !strings.Contains(joined, `"args_digest":"low"`) || !strings.Contains(joined, `"args_digest":"high"`) {
		t.Fatalf("webhook bodies missing digests: %s", joined)
	}
	if !strings.Contains(joined, `"amount":50`) {
		t.Fatalf("webhook bodies missing capped args: %s", joined)
	}
}

func TestDecide_CacheBoundedByLRU(t *testing.T) {
	eng := policy.New(policy.Config{
		CacheTTL: time.Hour,
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "acme", AgentID: "payroll", Connector: "bank",
		}},
	})
	base := policy.PolicyInput{
		Stage: policy.StageToolCall, TenantID: "acme", AgentID: "payroll",
		Connector: "bank", Tool: "transfer",
	}
	const extra = 500
	for i := 0; i < 10_000+extra; i++ {
		in := base
		in.ArgsDigest = fmt.Sprintf("%d", i)
		if dec := eng.Decide(context.Background(), in); dec.Effect != policy.EffectAllow {
			t.Fatalf("i=%d %+v", i, dec)
		}
	}
	if n := eng.CacheLen(); n > 10_000 {
		t.Fatalf("CacheLen=%d want ≤ 10000", n)
	}
	if n := eng.CacheLen(); n < 1 {
		t.Fatalf("CacheLen=%d want occupancy after flood", n)
	}
}
