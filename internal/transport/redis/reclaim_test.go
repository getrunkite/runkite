package redistransport_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
	redistransport "github.com/getrunkite/runkite/internal/transport/redis"
)

// These mirror internal/transport/inprocess/reclaim_test.go so both queue
// backends have equivalent coverage for the crash-recovery mechanism
// (in-flight tracking + Ack/Nack + ReclaimStale) -- this is what prevents a
// crashed runner's zombie GetJob long-poll from permanently losing a job it
// stole from the queue but can never deliver.

func TestRedisReclaimStale_ReenqueuesUnackedJob(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	q := redistransport.NewQueue(rdb)
	ctx := context.Background()

	job := &transport.RunAssignment{
		RunID:      "run-stale",
		ThreadID:   "t1",
		GraphID:    "echo",
		RunnerKind: "test-runner",
	}
	if err := q.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}

	got, err := q.Dequeue(ctx, "test-runner", time.Second)
	if err != nil || got == nil {
		t.Fatalf("dequeue: got=%v err=%v", got, err)
	}

	// Not Ack'd -- reclaim with a zero maxAge should pick it up immediately.
	n, _, err := q.ReclaimStale(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d, want 1", n)
	}

	again, err := q.Dequeue(ctx, "test-runner", time.Second)
	if err != nil || again == nil {
		t.Fatalf("expected reclaimed job, got=%v err=%v", again, err)
	}
	if again.RunID != "run-stale" {
		t.Fatalf("got run %s, want run-stale", again.RunID)
	}
	// Fencing: a reclaimed job's generation must be bumped so the
	// ORIGINAL runner's late Heartbeat/ReportStatus (if its blip was
	// transient and it finishes anyway) presents a stale value and
	// gets rejected instead of clobbering this new attempt.
	if again.Generation != 1 {
		t.Fatalf("generation after one reclaim = %d, want 1 (started at 0, ReclaimStale increments once)", again.Generation)
	}
}

func TestRedisAck_PreventsReclaim(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	q := redistransport.NewQueue(rdb)
	ctx := context.Background()

	job := &transport.RunAssignment{
		RunID:      "run-acked",
		ThreadID:   "t1",
		GraphID:    "echo",
		RunnerKind: "test-runner",
	}
	_ = q.Enqueue(ctx, job)
	got, _ := q.Dequeue(ctx, "test-runner", time.Second)
	if got == nil {
		t.Fatal("expected job")
	}
	if accepted, err := q.Ack(ctx, got.RunID, 0); err != nil || !accepted {
		t.Fatalf("Ack: accepted=%v err=%v", accepted, err)
	}

	n, _, err := q.ReclaimStale(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reclaimed %d after Ack, want 0", n)
	}
}

func TestRedisNack_ReenqueuesImmediately(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	q := redistransport.NewQueue(rdb)
	ctx := context.Background()

	job := &transport.RunAssignment{
		RunID:      "run-nacked",
		ThreadID:   "t1",
		GraphID:    "echo",
		RunnerKind: "test-runner",
	}
	_ = q.Enqueue(ctx, job)
	got, _ := q.Dequeue(ctx, "test-runner", time.Second)
	if got == nil {
		t.Fatal("expected job")
	}

	if err := q.Nack(ctx, got.RunID); err != nil {
		t.Fatal(err)
	}

	// Nack should make the job available again without waiting for reclaim.
	again, err := q.Dequeue(ctx, "test-runner", time.Second)
	if err != nil || again == nil {
		t.Fatalf("expected job back after Nack, got=%v err=%v", again, err)
	}
	if again.RunID != "run-nacked" {
		t.Fatalf("got run %s, want run-nacked", again.RunID)
	}
}

// TestRedisReclaimStale_SkipsCanceledJob guards a correctness property that
// falls out of ReclaimStale reusing Enqueue's own cancel check: a job
// canceled while it's in flight (runner has it, hasn't Ack'd yet) must not
// come back to life via reclaim after the cancellation.
func TestRedisReclaimStale_SkipsCanceledJob(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	q := redistransport.NewQueue(rdb)
	ctx := context.Background()

	job := &transport.RunAssignment{
		RunID:      "run-cancel-inflight",
		ThreadID:   "t1",
		GraphID:    "echo",
		RunnerKind: "test-runner",
	}
	_ = q.Enqueue(ctx, job)
	got, _ := q.Dequeue(ctx, "test-runner", time.Second)
	if got == nil {
		t.Fatal("expected job")
	}

	// Run gets canceled while in flight (e.g. client canceled, runner died).
	if err := q.Cancel(ctx, got.RunID); err != nil {
		t.Fatal(err)
	}

	n, _, err := q.ReclaimStale(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reclaimed %d for a canceled in-flight job, want 0 (canceled jobs must not resurrect)", n)
	}

	// Confirm it's truly gone, not just delayed.
	again, err := q.Dequeue(ctx, "test-runner", 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Fatalf("expected no job after canceling an in-flight run, got %v", again)
	}
}

// TestRedisReclaimStale_PreservesEmptyArrayFields is a regression test
// for a real bug found live: an earlier version of reclaimStaleScript
// bumped generation via a full cjson.decode-then-cjson.encode
// round-trip of the whole job payload. Redis's bundled lua-cjson has no
// way to tell an empty JSON array apart from an empty object once
// decoded to an empty Lua table (cjson.array_mt, the textbook fix,
// doesn't exist in Redis 7.4's bundled version, confirmed live) -- so
// re-encoding silently turned every empty-array field (e.g.
// ConnectorNeeds:[]) into {}, which Go's own json.Unmarshal then
// rejects, silently dropping the job instead of ever redelivering it.
// Fixed by never re-encoding the payload at all: this asserts a
// reclaimed job's own empty-array field survives byte-for-byte, not
// just that Dequeue returns *something*.
func TestRedisReclaimStale_PreservesEmptyArrayFields(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	q := redistransport.NewQueue(rdb)
	ctx := context.Background()

	job := &transport.RunAssignment{
		RunID:          "run-empty-array",
		ThreadID:       "t1",
		GraphID:        "echo",
		RunnerKind:     "test-runner",
		Generation:     1,
		ConnectorNeeds: []string{}, // the exact empty-array shape that triggered the bug
		StreamModes:    []string{"values"},
	}
	if err := q.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Dequeue(ctx, "test-runner", time.Second); err != nil {
		t.Fatal(err)
	}

	if n, _, err := q.ReclaimStale(ctx, 0, 0); err != nil || n != 1 {
		t.Fatalf("ReclaimStale: n=%d err=%v", n, err)
	}

	again, err := q.Dequeue(ctx, "test-runner", time.Second)
	if err != nil || again == nil {
		t.Fatalf("expected reclaimed job to still be dequeueable (not stuck on a corrupted payload), got=%v err=%v", again, err)
	}
	if again.ConnectorNeeds == nil || len(again.ConnectorNeeds) != 0 {
		t.Fatalf("ConnectorNeeds after reclaim = %#v, want a non-nil empty slice (not corrupted into an object Go can't unmarshal)", again.ConnectorNeeds)
	}
	if len(again.StreamModes) != 1 || again.StreamModes[0] != "values" {
		t.Fatalf("StreamModes after reclaim = %#v, want [\"values\"] unchanged", again.StreamModes)
	}
	if again.Generation != 2 {
		t.Fatalf("Generation after one reclaim = %d, want 2", again.Generation)
	}
}

// TestRedisReclaimStale_NestedGenerationInInputDoesNotConfuseTheBump is a
// regression test for a real, confirmed-live bug: an earlier version
// bumped the fencing generation by text-searching the job's own raw
// JSON for a "generation":N substring and replacing the first match.
// When the job's own Input happened to contain a field ALSO named
// "generation" (arbitrary user/agent content -- nothing stops this),
// that search found and mutated the WRONG occurrence, leaving the real
// top-level generation unbumped while silently corrupting the run's
// input. This run's Input deliberately contains its own "generation"
// key with a DIFFERENT value than the run's real (top-level) one, to
// catch exactly that confusion.
func TestRedisReclaimStale_NestedGenerationInInputDoesNotConfuseTheBump(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	q := redistransport.NewQueue(rdb)
	ctx := context.Background()

	job := &transport.RunAssignment{
		RunID:      "run-nested-generation",
		ThreadID:   "t1",
		GraphID:    "echo",
		RunnerKind: "test-runner",
		Generation: 1,
		// A field named "generation" nested inside the run's own input
		// -- ordinary, plausible agent content, not a crafted attack.
		Input: json.RawMessage(`{"generation":999,"messages":[{"role":"user","content":"hi"}]}`),
	}
	if err := q.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Dequeue(ctx, "test-runner", time.Second); err != nil {
		t.Fatal(err)
	}

	if n, _, err := q.ReclaimStale(ctx, 0, 0); err != nil || n != 1 {
		t.Fatalf("ReclaimStale: n=%d err=%v", n, err)
	}

	again, err := q.Dequeue(ctx, "test-runner", time.Second)
	if err != nil || again == nil {
		t.Fatalf("expected reclaimed job, got=%v err=%v", again, err)
	}
	if again.Generation != 2 {
		t.Fatalf("top-level Generation after one reclaim = %d, want 2 -- the nested \"generation\" in Input must not have been mistaken for the real field", again.Generation)
	}
	var input map[string]any
	if err := json.Unmarshal(again.Input, &input); err != nil {
		t.Fatalf("Input is no longer valid JSON after reclaim: %v", err)
	}
	if v, _ := input["generation"].(float64); v != 999 {
		t.Fatalf("nested input.generation = %v, want unchanged 999 -- reclaim must not touch user content", input["generation"])
	}

	// A stale runner presenting generation 1 (what it was actually
	// dispatched with) must be rejected now that the real generation
	// is 2 -- this is the actual fencing property the bug above would
	// have silently defeated (top-level generation staying at 1 would
	// have made this Ack wrongly succeed).
	if accepted, err := q.Ack(ctx, "run-nested-generation", 1); err != nil || accepted {
		t.Fatalf("stale generation-1 Ack: expected accepted=false, got accepted=%v err=%v", accepted, err)
	}
	if accepted, err := q.Ack(ctx, "run-nested-generation", 2); err != nil || !accepted {
		t.Fatalf("current generation-2 Ack: expected accepted=true, got accepted=%v err=%v", accepted, err)
	}
}

// TestRedisReclaimStale_RespectsMaxAge confirms jobs younger than maxAge
// are left alone -- ReclaimStale must not be a "reclaim everything" hammer.
func TestRedisReclaimStale_RespectsMaxAge(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	q := redistransport.NewQueue(rdb)
	ctx := context.Background()

	job := &transport.RunAssignment{
		RunID:      "run-fresh",
		ThreadID:   "t1",
		GraphID:    "echo",
		RunnerKind: "test-runner",
	}
	_ = q.Enqueue(ctx, job)
	got, _ := q.Dequeue(ctx, "test-runner", time.Second)
	if got == nil {
		t.Fatal("expected job")
	}

	// Job was just dequeued -- a generous maxAge must not reclaim it yet.
	n, _, err := q.ReclaimStale(ctx, time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reclaimed %d fresh in-flight jobs, want 0", n)
	}
}

func TestRedisReclaimStale_MaxRetriesPoisonPill(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	q := redistransport.NewQueue(rdb)
	ctx := context.Background()

	job := &transport.RunAssignment{
		RunID:      "run-pill",
		ThreadID:   "t1",
		GraphID:    "echo",
		RunnerKind: "test-runner",
		Generation: 1,
		TenantID:   "acme",
	}
	if err := q.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Dequeue(ctx, "test-runner", time.Second); err != nil {
		t.Fatal(err)
	}

	const maxRetries = 2
	n, dead, err := q.ReclaimStale(ctx, 0, maxRetries)
	if err != nil || n != 1 || len(dead) != 0 {
		t.Fatalf("first reclaim: n=%d dead=%d err=%v", n, len(dead), err)
	}
	if _, err := q.Dequeue(ctx, "test-runner", time.Second); err != nil {
		t.Fatal(err)
	}

	n, dead, err = q.ReclaimStale(ctx, 0, maxRetries)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(dead) != 1 {
		t.Fatalf("ceiling: n=%d dead=%d want n=0 dead=1", n, len(dead))
	}
	if dead[0].RunID != "run-pill" || dead[0].Generation != 3 {
		t.Fatalf("pill=%+v want run-pill gen 3", dead[0])
	}
	if dead[0].TenantID != "acme" || dead[0].AgentID != "echo" {
		t.Fatalf("pill labels=%+v", dead[0])
	}
	extra, _ := q.Dequeue(ctx, "test-runner", 200*time.Millisecond)
	if extra != nil {
		t.Fatalf("poisoned job reappeared: %+v", extra)
	}
}
