package e2e_test

import "testing"

// TestCronScheduler_BootstrapsFromConfig proves the cron scheduler
// actually registers schedules from langgraph.json against the real,
// unmodified binary and a real database -- not just the config parser in
// isolation (see internal/config's own unit test for that).
//
// This deliberately uses a schedule that fires once a year (see
// examples/all_agents/langgraph.json's "yearly-echo" entry) so the test
// only proves registration, not an actual dispatch -- the full live fire +
// multi-instance-safe claim behavior was verified manually against two
// real control-plane replicas sharing one Postgres + Redis (see
// cmd/cron_test.go for the pure scheduling-decision unit tests and
// internal/api/cron_test.go for DispatchScheduledRun's dispatch-path
// tests; a 60s-plus real-time wait for a natural fire isn't worth paying
// in every CI run).
func TestCronScheduler_BootstrapsFromConfig(t *testing.T) {
	var schedules []map[string]interface{}
	getJSON(t, "/internal/cron", &schedules)

	var found map[string]interface{}
	for _, s := range schedules {
		if s["name"] == "yearly-echo" {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatalf("expected yearly-echo cron schedule to be registered, got %+v", schedules)
	}
	if found["agent_id"] != "echo_agent" {
		t.Errorf("expected agent_id echo_agent, got %v", found["agent_id"])
	}
	if found["expression"] != "0 3 1 1 *" {
		t.Errorf("expected expression '0 3 1 1 *', got %v", found["expression"])
	}
	if found["enabled"] != true {
		t.Errorf("expected enabled=true by default, got %v", found["enabled"])
	}
}
