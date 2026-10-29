package redistransport_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
	redistransport "github.com/getrunkite/runkite/internal/transport/redis"
	goredis "github.com/redis/go-redis/v9"
)

func TestRedisPoisonPillRetainsOriginalGenerationWhenIndexMissing(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	ctx := context.Background()
	redistransport.FlushAll(ctx, rdb)
	producer := redistransport.NewQueue(rdb)
	job := &transport.RunAssignment{RunID: "poison-missing-index", RunnerKind: "kind", GraphID: "agent", TenantID: "acme", Generation: 2, Input: json.RawMessage(`{"generation":999}`), ConnectorNeeds: []string{}}
	if err := producer.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	if assignment, err := producer.Dequeue(ctx, "kind", time.Second); err != nil || assignment == nil {
		t.Fatal(assignment, err)
	}
	if err := rdb.HDel(ctx, "rk:inflight:generation", job.RunID).Err(); err != nil {
		t.Fatal(err)
	}
	reaper := redistransport.NewQueue(rdb)
	for attempt := 0; attempt < 3; attempt++ {
		n, dead, err := reaper.ReclaimStale(ctx, -time.Second, 2)
		if err != nil || n != 0 || len(dead) != 1 || dead[0].Generation != 3 {
			t.Fatalf("retry %d: n=%d pills=%+v err=%v", attempt, n, dead, err)
		}
		live, err := producer.LookupInflight(ctx, job.RunID)
		if err != nil || live == nil || live.Generation != 2 || string(live.Input) != string(job.Input) || live.ConnectorNeeds == nil {
			t.Fatalf("original dispatch was changed: assignment=%+v err=%v", live, err)
		}
		if current, err := producer.Renew(ctx, job.RunID, 0); err != nil || current {
			t.Fatalf("zero renewed positive poison lease: current=%t err=%v", current, err)
		}
		if accepted, err := producer.Ack(ctx, job.RunID, 3); err != nil || accepted {
			t.Fatalf("next generation consumed original lease: accepted=%t err=%v", accepted, err)
		}
	}
	if current, err := producer.Renew(ctx, job.RunID, 2); err != nil || !current {
		t.Fatalf("original generation renewal failed: current=%t err=%v", current, err)
	}
	if accepted, err := reaper.Ack(ctx, job.RunID, 2); err != nil || !accepted {
		t.Fatalf("durable completion Ack failed: accepted=%t err=%v", accepted, err)
	}
	if n, dead, err := producer.ReclaimStale(ctx, -time.Second, 2); err != nil || n != 0 || len(dead) != 0 {
		t.Fatalf("acknowledged poison returned: n=%d pills=%+v err=%v", n, dead, err)
	}
}

func TestRedisReclaimDoesNotCoerceMissingIndexPayloadGeneration(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	ctx := context.Background()
	redistransport.FlushAll(ctx, rdb)
	const runID = "poison-string-generation"
	const payload = `{"run_id":"poison-string-generation","runner_kind":"kind","generation":"2","input":{"generation":999}}`
	if err := rdb.HSet(ctx, "rk:inflight:data", runID, payload).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.ZAdd(ctx, "rk:inflight:zset", goredis.Z{Score: 0, Member: runID}).Err(); err != nil {
		t.Fatal(err)
	}
	q := redistransport.NewQueue(rdb)
	if _, _, err := q.ReclaimStale(ctx, -time.Second, 2); err == nil {
		t.Fatal("string-valued generation was coerced into a numeric fence")
	}
	if got, err := rdb.HGet(ctx, "rk:inflight:data", runID).Result(); err != nil || got != payload {
		t.Fatalf("invalid assignment was lost: payload=%s err=%v", got, err)
	}
	if _, err := rdb.ZScore(ctx, "rk:inflight:zset", runID).Result(); err != nil {
		t.Fatalf("invalid assignment's recoverable index was removed: %v", err)
	}
	if exists, err := rdb.HExists(ctx, "rk:inflight:generation", runID).Result(); err != nil || exists {
		t.Fatalf("invalid generation was indexed: exists=%t err=%v", exists, err)
	}
}
