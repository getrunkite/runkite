package api_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestCreateRun_CheckpointRefForwarded closes the audit finding that
// checkpoint_ref used to be accepted by the API but silently dropped by
// runners (wrong-answer-with-no-error). The control plane now forwards a
// non-empty value on RunAssignment so LangGraph runners can set
// configurable.checkpoint_id and resume from that past checkpoint.
func TestCreateRun_CheckpointRefForwarded(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "checkpoint_ref_agent")

	resp, err := postJSON(env.srv.URL+"/threads", map[string]interface{}{})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	var thread map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&thread)
	resp.Body.Close()
	threadID := thread["thread_id"].(string)

	const wantRef = "some-past-checkpoint-id"
	runResp, err := postJSON(env.srv.URL+"/threads/"+threadID+"/runs", map[string]interface{}{
		"agent_id":       "checkpoint_ref_agent",
		"input":          map[string]interface{}{},
		"checkpoint_ref": wantRef,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	expectStatus(t, runResp, 200)
	runResp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil {
		t.Fatalf("dequeue assignment: %v", err)
	}
	if assignment.CheckpointRef == nil {
		t.Fatal("expected RunAssignment.CheckpointRef to be set, got nil")
	}
	if *assignment.CheckpointRef != wantRef {
		t.Fatalf("CheckpointRef = %q, want %q", *assignment.CheckpointRef, wantRef)
	}
}

// TestCreateRun_EmptyCheckpointRefTreatedAsAbsent: "" / whitespace must
// not be forwarded (same as omitting the field).
func TestCreateRun_EmptyCheckpointRefTreatedAsAbsent(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "empty_checkpoint_ref_agent")

	resp, err := postJSON(env.srv.URL+"/threads", map[string]interface{}{})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	var thread map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&thread)
	resp.Body.Close()
	threadID := thread["thread_id"].(string)

	runResp, err := postJSON(env.srv.URL+"/threads/"+threadID+"/runs", map[string]interface{}{
		"agent_id":       "empty_checkpoint_ref_agent",
		"input":          map[string]interface{}{},
		"checkpoint_ref": "   ",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	expectStatus(t, runResp, 200)
	runResp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil {
		t.Fatalf("dequeue assignment: %v", err)
	}
	if assignment.CheckpointRef != nil {
		t.Fatalf("expected nil CheckpointRef for whitespace-only value, got %q", *assignment.CheckpointRef)
	}
}

// TestCreateRun_NoCheckpointRefStillSucceeds is the control: the
// overwhelmingly common case (no checkpoint_ref at all) must still work.
func TestCreateRun_NoCheckpointRefStillSucceeds(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "no_checkpoint_ref_agent")

	resp, err := postJSON(env.srv.URL+"/threads", map[string]interface{}{})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	var thread map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&thread)
	resp.Body.Close()
	threadID := thread["thread_id"].(string)

	runResp, err := postJSON(env.srv.URL+"/threads/"+threadID+"/runs", map[string]interface{}{
		"agent_id": "no_checkpoint_ref_agent",
		"input":    map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	expectStatus(t, runResp, 200)
}
