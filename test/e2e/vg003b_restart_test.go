package e2e_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestVG003b_ResumeSurvivesRunnerRestart is the proof of checkpoint dual
// mode (master plan): interrupt a run, kill the runner process entirely
// (no graceful shutdown -- simulates a crash, zero shared memory with the
// next process), start a brand new runner process, then verify the
// checkpoint persisted somewhere external to the runner's own memory.
//
// The deterministic, non-flaky assertion is a direct Postgres query for
// LangGraph's own checkpoint row (proves AsyncPostgresSaver actually wrote
// state that survives the process boundary -- this is what checkpoint
// dual mode is actually about, and it's been manually verified multiple
// times against a live stack with a real kill+restart).
//
// After the checkpoint assertion, this test also proves the full HTTP
// resume path against the fresh runner. Job-loss after a crashed runner's
// zombie GetJob is mitigated by: (1) gRPC keepalive, (2) inflight
// Ack/Nack + ReclaimStale on the queue, (3) a short post-kill wait in
// restartRunner so the e2e path is deterministic, and (4) capping each
// individual BRPOP call's blocking duration in redis.Queue.Dequeue (see
// dequeueBlockCap's doc comment) -- this test used to flake ~50-70% of
// the time on a genuine Redis race that (4) closes: a zombie's
// long-lived single BRPOP call could atomically pop a freshly-enqueued
// resume job to deliver to a connection that was being torn down at the
// same instant, losing the job client-side even though Redis had
// already removed it from the list server-side. Root-caused live via
// targeted instrumentation -- see redis.go's dequeueBlockCap comment for the fix.
func TestVG003b_ResumeSurvivesRunnerRestart(t *testing.T) {
	resp := postJSON(t, "/threads", map[string]interface{}{})
	var thread map[string]interface{}
	decodeJSON(t, resp, &thread)
	threadID := thread["thread_id"].(string)

	// Run 1: interrupt, on the ORIGINAL runner process.
	resp = postJSON(t, "/threads/"+threadID+"/runs/stream", map[string]interface{}{
		"agent_id": "approval_agent",
		"input":    map[string]interface{}{"messages": []map[string]string{{"role": "user", "content": "send the email"}}, "approved": false},
	})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	events := parseSSE(t, string(body))
	if !containsAll(eventTypes(events), []string{"lifecycle", "input.requested"}) {
		t.Fatalf("expected an interrupt before restarting the runner, got %v", eventTypes(events))
	}

	// Deterministic proof: LangGraph's AsyncPostgresSaver must have written
	// a real checkpoint row for this thread, independent of whether any
	// runner is currently alive to serve it.
	requireCheckpointInPostgres(t, threadID)

	// Thread must be free before the resume attempt.
	var threadState map[string]interface{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		getJSON(t, "/threads/"+threadID, &threadState)
		if s, _ := threadState["status"].(string); s != "busy" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	// Kill the runner entirely and start a completely fresh process. The
	// control plane itself is untouched -- only the runner "crashes".
	restartRunner(t, configPath)

	// Run 2: resume against the FRESH runner process. A couple of retries
	// cover reclaim timing; a real client recovering from a runner crash
	// would retry too.
	fastClient := &http.Client{Timeout: 10 * time.Second}
	var result map[string]interface{}
	var lastErr string
	resumed := false
	for attempt := 0; attempt < 5; attempt++ {
		b, _ := json.Marshal(map[string]interface{}{
			"agent_id": "approval_agent",
			"command":  map[string]interface{}{"resume": true},
		})
		resp, err := fastClient.Post(baseURL+"/threads/"+threadID+"/runs/wait", "application/json", strings.NewReader(string(b)))
		if err == nil {
			decodeJSON(t, resp, &result)
			if run, _ := result["run"].(map[string]interface{}); run != nil && run["status"] == "success" {
				resumed = true
				break
			}
			raw, _ := json.Marshal(result)
			lastErr = "run did not reach success: " + string(raw)
		} else {
			lastErr = err.Error()
		}
		time.Sleep(1 * time.Second)
	}

	if !resumed {
		var cpLog string
		if serveLogRef != nil {
			cpLog = serveLogRef.String()
		}
		t.Fatalf("resume after runner restart failed (%s); checkpoint was present in Postgres\n--- control plane log ---\n%s", lastErr, cpLog)
	}

	values, _ := result["values"].(map[string]interface{})
	msgs, _ := values["messages"].([]interface{})
	if len(msgs) == 0 {
		t.Fatalf("expected messages after resume, got %v", values)
	}
	last, _ := msgs[len(msgs)-1].(map[string]interface{})
	content, _ := last["content"].(string)
	if !strings.Contains(content, "sent") && !strings.Contains(content, "successfully") {
		t.Fatalf("expected the resumed run to complete the original action, got: %q", content)
	}
}

// requireCheckpointInPostgres queries LangGraph's own checkpoints table
// directly (bypassing the control plane and the runner entirely) to prove
// the direct-mode AsyncPostgresSaver actually persisted state for this
// thread. This is the deterministic proof of checkpoint dual mode.
func requireCheckpointInPostgres(t *testing.T, threadID string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), os.Getenv("POSTGRES_DSN"))
	if err != nil {
		t.Fatalf("connect to postgres for checkpoint verification: %v", err)
	}
	defer conn.Close(context.Background())

	var count int
	err = conn.QueryRow(context.Background(),
		"SELECT count(*) FROM checkpoints WHERE thread_id = $1", threadID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("query checkpoints table: %v", err)
	}
	if count == 0 {
		t.Fatalf("expected at least one row in LangGraph's checkpoints table for thread %s -- "+
			"checkpoint dual mode (direct-mode AsyncPostgresSaver) did not persist anything", threadID)
	}
	t.Logf("confirmed %d checkpoint row(s) in Postgres for thread %s (survives runner restart)", count, threadID)
}
