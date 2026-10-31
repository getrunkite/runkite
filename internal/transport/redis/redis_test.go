package redistransport_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/conformance"
	redistransport "github.com/getrunkite/runkite/internal/transport/redis"
)

func getRedisClient(t *testing.T) *goredis.Client {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set — skipping Redis conformance tests")
	}
	opts, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse REDIS_URL: %v", err)
	}
	rdb := goredis.NewClient(opts)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	return rdb
}

func TestRedisJobQueue(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()

	conformance.RunJobQueueSuite(t, func() transport.JobQueue {
		redistransport.FlushAll(context.Background(), rdb)
		return redistransport.NewQueue(rdb)
	})
}

func TestRedisEventBroker(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()

	conformance.RunEventBrokerSuite(t, func() transport.EventBroker {
		redistransport.FlushAll(context.Background(), rdb)
		return redistransport.NewBroker(rdb)
	})
}

func TestRedisCancelBroker(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()

	conformance.RunCancelBrokerSuite(t, func() transport.CancelBroker {
		redistransport.FlushAll(context.Background(), rdb)
		return redistransport.NewCancelBus(rdb)
	})
}

// TestRedisBroker_StreamExpiresAfterTerminalEvent is the regression for a
// real keyspace-bloat bug found via Redis commandstats under concurrent
// load: XAdd never set any expiry on a run's rk:events:<run_id> stream
// key, so every completed run's stream persisted in Redis forever.
// Confirmed 5,476 accumulated stream keys on a long-lived test instance,
// which made Queue.Len's SCAN-based lookup (called on every job dispatch
// at the time) the dominant cost per request under load -- the actual
// root cause of bench/REPORT.md's "Redis gets slower under concurrency"
// finding.
func TestRedisBroker_StreamExpiresAfterTerminalEvent(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)

	broker := redistransport.NewBroker(rdb)
	ctx := context.Background()
	runID := "run-ttl-001"
	streamKey := "rk:events:" + runID

	if err := broker.Publish(ctx, runID, &transport.RunEvent{Seq: 1, Method: "start"}); err != nil {
		t.Fatalf("Publish(start) failed: %v", err)
	}
	// A non-terminal event sets the longer "hung run" safety-net TTL
	// (TestRedisBroker_NonTerminalEventStillSetsATTL covers that
	// directly) -- what this test cares about is that the TERMINAL
	// event tightens it down to eventStreamTTL, not whether a TTL
	// exists at all before that point.
	if ttl, _ := rdb.TTL(ctx, streamKey).Result(); ttl <= 24*time.Hour {
		t.Errorf("expected the non-terminal event's longer safety-net TTL before the terminal event, got ttl=%v", ttl)
	}

	if err := broker.Publish(ctx, runID, &transport.RunEvent{Seq: 2, Method: "end"}); err != nil {
		t.Fatalf("Publish(end) failed: %v", err)
	}

	ttl, err := rdb.TTL(ctx, streamKey).Result()
	if err != nil {
		t.Fatalf("TTL check failed: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("stream key must have a positive TTL after a terminal event, got %v (bug: stream would persist forever)", ttl)
	}
	if ttl > 24*time.Hour {
		t.Errorf("stream TTL should be <= 24h, got %v", ttl)
	}
}

// TestRedisBroker_NonTerminalEventStillSetsATTL is the regression for
// the "hung run" residual limitation disclosed in bench/REPORT.md: a run
// that never reaches a terminal event (a genuine hang/crash, not a
// normal completion) used to leave its stream with no TTL at all,
// since only a terminal Publish or Close ever set one. A non-terminal
// event now sets a long safety-net TTL too, refreshed on every event.
func TestRedisBroker_NonTerminalEventStillSetsATTL(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)

	broker := redistransport.NewBroker(rdb)
	ctx := context.Background()
	runID := "run-ttl-hung-001"
	streamKey := "rk:events:" + runID

	if err := broker.Publish(ctx, runID, &transport.RunEvent{Seq: 1, Method: "start"}); err != nil {
		t.Fatalf("Publish(start) failed: %v", err)
	}

	ttl, err := rdb.TTL(ctx, streamKey).Result()
	if err != nil {
		t.Fatalf("TTL check failed: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("a non-terminal event must still set a safety-net TTL, got %v (bug: a hung/crashed run's stream would persist forever)", ttl)
	}
	if ttl <= 24*time.Hour {
		t.Errorf("a non-terminal event's safety-net TTL should be longer than the terminal eventStreamTTL (24h), got %v", ttl)
	}
}

// TestRedisBroker_CloseAlsoExpiresStream verifies Close (the alternate
// path to marking a run done, used by StatusCallback) also expires the
// stream, not just a terminal Publish.
func TestRedisBroker_CloseAlsoExpiresStream(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)

	broker := redistransport.NewBroker(rdb)
	ctx := context.Background()
	runID := "run-ttl-close-001"
	streamKey := "rk:events:" + runID

	if err := broker.Publish(ctx, runID, &transport.RunEvent{Seq: 1, Method: "start"}); err != nil {
		t.Fatalf("Publish(start) failed: %v", err)
	}
	if err := broker.Close(runID); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	ttl, err := rdb.TTL(ctx, streamKey).Result()
	if err != nil {
		t.Fatalf("TTL check failed: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("stream key must have a positive TTL after Close, got %v", ttl)
	}
}

func TestRedisPoisonPillPreservesColonRunIDs(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	ctx := context.Background()
	for _, id := range []string{"invoice:pending", "invoice:2", "invoice:2:retry", "tenant:作業"} {
		t.Run(id, func(t *testing.T) {
			if err := redistransport.FlushAll(ctx, rdb); err != nil {
				t.Fatal(err)
			}
			q := redistransport.NewQueue(rdb)
			job := &transport.RunAssignment{
				RunID: id, ThreadID: "thread", RunnerKind: "kind", GraphID: "agent", TenantID: "acme", Generation: 2,
			}
			if err := q.Enqueue(ctx, job); err != nil {
				t.Fatal(err)
			}
			if assignment, err := q.Dequeue(ctx, job.RunnerKind, time.Second); err != nil || assignment == nil {
				t.Fatalf("dequeue: assignment=%+v err=%v", assignment, err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				n, dead, err := q.ReclaimStale(ctx, -time.Second, 2)
				if err != nil || n != 0 || len(dead) != 1 {
					t.Fatalf("ceiling retry %d: n=%d pills=%+v err=%v", attempt, n, dead, err)
				}
				pill := dead[0]
				if pill.RunID != job.RunID || pill.Generation != 3 || pill.AgentID != job.GraphID || pill.TenantID != job.TenantID {
					t.Fatalf("poison identity changed: pill=%+v assignment=%+v", pill, job)
				}
				if live, err := q.LookupInflight(ctx, job.RunID); err != nil || live == nil || live.Generation != 2 {
					t.Fatalf("retained lease: assignment=%+v err=%v", live, err)
				}
			}
			if accepted, err := q.Ack(ctx, job.RunID, 2); err != nil || !accepted {
				t.Fatalf("exact generation Ack: accepted=%t err=%v", accepted, err)
			}
			if n, dead, err := q.ReclaimStale(ctx, -time.Second, 2); err != nil || n != 0 || len(dead) != 0 {
				t.Fatalf("completed pill returned: n=%d pills=%+v err=%v", n, dead, err)
			}
		})
	}
}

func TestRedisReclaimRetainsAssignmentWhenReadyQueueWriteFails(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	ctx := context.Background()
	if err := redistransport.FlushAll(ctx, rdb); err != nil {
		t.Fatal(err)
	}
	q := redistransport.NewQueue(rdb)
	job := &transport.RunAssignment{
		RunID: "reclaim-write-failure", ThreadID: "thread", RunnerKind: "kind", Generation: 1,
		Input: json.RawMessage(`{"large":9007199254740993,"items":[]}`),
	}
	if err := q.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	if assignment, err := q.Dequeue(ctx, job.RunnerKind, time.Second); err != nil || assignment == nil {
		t.Fatalf("dequeue: assignment=%+v err=%v", assignment, err)
	}
	before, err := rdb.HGet(ctx, "rk:inflight:data", job.RunID).Result()
	if err != nil {
		t.Fatal(err)
	}
	// Redis scripts do not roll back earlier writes when LPUSH fails.
	if err := rdb.Set(ctx, "rk:queue:kind", "wrong-type", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.ReclaimStale(ctx, -time.Second, 3); err == nil {
		t.Fatal("reclaim accepted an unwritable ready queue")
	}
	if after, err := rdb.HGet(ctx, "rk:inflight:data", job.RunID).Result(); err != nil || after != before {
		t.Fatalf("recoverable assignment was changed or removed: payload=%q err=%v", after, err)
	}
	if current, err := q.Renew(ctx, job.RunID, 1); err != nil || !current {
		t.Fatalf("failed reclaim advanced or removed the original fence: current=%t err=%v", current, err)
	}
	if err := rdb.Del(ctx, "rk:queue:kind").Err(); err != nil {
		t.Fatal(err)
	}
	if n, dead, err := q.ReclaimStale(ctx, -time.Second, 3); err != nil || n != 1 || len(dead) != 0 {
		t.Fatalf("reclaim after repair: n=%d pills=%+v err=%v", n, dead, err)
	}
	assignment, err := q.Dequeue(ctx, job.RunnerKind, time.Second)
	if err != nil || assignment == nil || assignment.Generation != 2 || string(assignment.Input) != string(job.Input) {
		t.Fatalf("repaired dispatch changed payload or generation: assignment=%+v err=%v", assignment, err)
	}
	if accepted, err := q.Ack(ctx, job.RunID, 2); err != nil || !accepted {
		t.Fatalf("repaired generation Ack: accepted=%t err=%v", accepted, err)
	}
}
