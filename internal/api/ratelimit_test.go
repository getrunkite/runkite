package api_test

import (
	"testing"

	"github.com/getrunkite/runkite/internal/ratelimit"
)

// TestCreateRun_PerAgentRateLimit proves createRun (the single choke point
// every run-creation path -- REST, WebSocket, streaming commands -- goes
// through) enforces a configured per-agent limit, and that a different
// agent's runs are unaffected by another agent's exhausted bucket.
func TestCreateRun_PerAgentRateLimit(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "limited-agent")
	registerAgent(t, env, "other-agent")
	env.apiServer.SetRateLimiter(ratelimit.New(&ratelimit.Config{
		PerAgent: &ratelimit.Rule{RPS: 0.001, Burst: 1},
	}))

	resp, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "limited-agent"})
	expectStatus(t, resp, 200)

	// Second run for the SAME agent exceeds the burst-of-1 limit.
	resp2, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "limited-agent"})
	expectStatus(t, resp2, 429)
	if ra := resp2.Header.Get("Retry-After"); ra == "" {
		t.Error("expected Retry-After header on 429 response")
	}

	// A DIFFERENT agent has its own independent bucket and is unaffected.
	resp3, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "other-agent"})
	expectStatus(t, resp3, 200)
}

// TestCreateRun_NoRateLimiterIsUnlimited proves the default (no
// SetRateLimiter call at all, matching production with no rate_limit
// config) never blocks anything.
func TestCreateRun_NoRateLimiterIsUnlimited(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "unlimited-agent")
	for i := 0; i < 5; i++ {
		resp, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "unlimited-agent"})
		expectStatus(t, resp, 200)
	}
}
