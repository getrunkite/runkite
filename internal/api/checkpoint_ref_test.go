package api_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCreateRun_CheckpointRefRejected closes a real audit finding:
// checkpoint_ref used to flow all the way from this request down to the
// runner and get silently dropped there, so a client asking to resume
// from a specific past checkpoint got a normal 200 and a run that
// quietly resumed from the thread's LATEST checkpoint instead --
// wrong-answer-with-no-error, worse than an outright missing feature.
// Now it's a loud, immediate 400 instead.
func TestCreateRun_CheckpointRefRejected(t *testing.T) {
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

	// checkpoint_ref is *string (see models.RunCreate) -- must send an
	// actual string here, not an object. Sending the wrong JSON shape
	// would still 400, but from readJSON's generic "invalid request
	// body" decode failure, not from ErrCheckpointRefUnsupported --
	// a false-positive pass that isn't actually exercising the
	// rejection this test exists to prove. The message assertion below
	// (checking for wording unique to ErrCheckpointRefUnsupported, not
	// just any 400) is what catches that class of mistake.
	runResp, err := postJSON(env.srv.URL+"/threads/"+threadID+"/runs", map[string]interface{}{
		"agent_id":       "checkpoint_ref_agent",
		"input":          map[string]interface{}{},
		"checkpoint_ref": "some-past-checkpoint-id",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	expectStatus(t, runResp, 400)

	var body map[string]interface{}
	json.NewDecoder(runResp.Body).Decode(&body)
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "checkpoint_ref") {
		t.Fatalf("expected the ErrCheckpointRefUnsupported message specifically (mentioning checkpoint_ref), got body %+v -- a generic 400 (e.g. a JSON decode error from sending the wrong shape) would also pass a bare status-code check, so this must assert on the actual message", body)
	}
}

// TestCreateRun_NoCheckpointRefStillSucceeds is the control: the
// overwhelmingly common case (no checkpoint_ref at all) must be
// completely unaffected by the rejection above.
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
