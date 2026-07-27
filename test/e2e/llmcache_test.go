package e2e_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestLLMCache_HitAgainstRealPostgres is a regression test for a real bug
// found via manual live testing: input/config are stored as Postgres JSONB,
// which reformats JSON on write (strips whitespace). Computing the cache
// key from run.Input/run.Config fetched back out of the DB (post-JSONB-
// round-trip) produced a DIFFERENT hash than the one computed from the
// original raw request bytes at lookup time, so a cache-configured agent
// NEVER actually hit against Postgres -- masked entirely by the
// SQLite-backed internal/api unit tests (SQLite stores input/config as
// plain TEXT, no reformatting, so the mismatch never showed up there).
// Only a real end-to-end run against real Postgres like this one exercises
// the actual bug's conditions.
func TestLLMCache_HitAgainstRealPostgres(t *testing.T) {
	// examples/all_agents/langgraph.json configures echo_agent with
	// llm_cache.ttl_seconds -- see bootstrapAgents wiring.
	// Unique content so a prior e2e run's cached entry (TTL 3600s in the
	// shared test Postgres) cannot make the "first" request look like a hit.
	input := map[string]interface{}{
		"messages": []map[string]string{{"role": "human", "content": "e2e cache test " + uuid.NewString()}},
	}

	resp1 := postJSON(t, "/runs/wait", map[string]interface{}{"agent_id": "echo_agent", "input": input})
	var result1 struct {
		Run struct {
			Metadata map[string]interface{} `json:"metadata"`
		} `json:"run"`
	}
	decodeJSON(t, resp1, &result1)
	if result1.Run.Metadata["cache_hit"] == true {
		t.Fatal("expected the first request to be a real miss (nothing cached yet)")
	}

	// Give StatusCallback (triggered by the runner's ReportStatus RPC,
	// which arrives asynchronously after the run's terminal broker event)
	// a moment to actually persist the cache entry.
	deadline := time.Now().Add(5 * time.Second)
	var hit bool
	for time.Now().Before(deadline) {
		resp2 := postJSON(t, "/runs/wait", map[string]interface{}{"agent_id": "echo_agent", "input": input})
		var result2 struct {
			Run struct {
				Metadata map[string]interface{} `json:"metadata"`
			} `json:"run"`
			Values map[string]interface{} `json:"values"`
		}
		decodeJSON(t, resp2, &result2)
		if result2.Run.Metadata["cache_hit"] == true {
			hit = true
			if result2.Values["messages"] == nil {
				t.Fatalf("cache hit response missing cached values: %+v", result2.Values)
			}
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !hit {
		t.Fatal("expected a cache hit against real Postgres within 5s of the first run completing")
	}
}
