package api_test

import (
	"testing"

	"github.com/getrunkite/runkite/internal/api"
)

func TestCreateRun_AdmissionConcurrentCap(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "capped-agent")
	registerAgent(t, env, "other-agent")
	env.apiServer.SetAdmissionLimits(&api.AdmissionLimits{AgentConcurrent: 1})

	resp, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "capped-agent"})
	expectStatus(t, resp, 200)

	// First run stays pending (no runner finishes it) — second hits the cap.
	resp2, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "capped-agent"})
	expectStatus(t, resp2, 429)
	if ra := resp2.Header.Get("Retry-After"); ra == "" {
		t.Error("expected Retry-After on concurrent 429")
	}

	// Other agent has its own concurrent budget.
	resp3, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "other-agent"})
	expectStatus(t, resp3, 200)
}

func TestCreateRun_AdmissionDailyCap(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "daily-agent")
	env.apiServer.SetAdmissionLimits(&api.AdmissionLimits{AgentDaily: 1})

	resp, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "daily-agent"})
	expectStatus(t, resp, 200)

	resp2, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "daily-agent"})
	expectStatus(t, resp2, 429)
	if ra := resp2.Header.Get("Retry-After"); ra == "" {
		t.Error("expected Retry-After on daily 429")
	}
}

func TestCreateRun_AdmissionTenantConcurrentCap(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "a1")
	registerAgent(t, env, "a2")
	env.apiServer.SetAdmissionLimits(&api.AdmissionLimits{TenantConcurrent: 1})

	resp, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "a1"})
	expectStatus(t, resp, 200)

	// Different agent, same tenant — still blocked by tenant concurrent.
	resp2, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "a2"})
	expectStatus(t, resp2, 429)
}

func TestCreateRun_NoAdmissionLimitsIsUnlimited(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "free-agent")
	for i := 0; i < 3; i++ {
		resp, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "free-agent"})
		expectStatus(t, resp, 200)
	}
}
