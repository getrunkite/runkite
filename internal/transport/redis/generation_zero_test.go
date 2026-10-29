package redistransport_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
	redistransport "github.com/getrunkite/runkite/internal/transport/redis"
)

func TestMissingGenerationIndexDoesNotMakePositiveAssignmentLegacy(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	ctx := context.Background()
	for _, generation := range []int64{0, 2} {
		t.Run(fmt.Sprint(generation), func(t *testing.T) {
			redistransport.FlushAll(ctx, rdb)
			q := redistransport.NewQueue(rdb)
			job := &transport.RunAssignment{RunID: "missing-index", RunnerKind: "kind", Generation: generation, Input: json.RawMessage(`{"generation":999}`)}
			if err := q.Enqueue(ctx, job); err != nil {
				t.Fatal(err)
			}
			if got, err := q.Dequeue(ctx, "kind", time.Second); err != nil || got == nil {
				t.Fatalf("dequeue: job=%v err=%v", got, err)
			}
			if err := rdb.HDel(ctx, "rk:inflight:generation", job.RunID).Err(); err != nil {
				t.Fatal(err)
			}
			wrong := int64(0)
			if generation == 0 {
				wrong = 2
			}
			if current, err := q.Renew(ctx, job.RunID, wrong); err != nil || current {
				t.Fatalf("mismatched Renew changed missing-index assignment: current=%v err=%v", current, err)
			}
			if accepted, err := q.Ack(ctx, job.RunID, wrong); err != nil || accepted {
				t.Fatalf("mismatched Ack changed missing-index assignment: accepted=%v err=%v", accepted, err)
			}
			if current, err := q.Renew(ctx, job.RunID, generation); err != nil || !current {
				t.Fatalf("exact Renew rejected: current=%v err=%v", current, err)
			}
			if accepted, err := q.Ack(ctx, job.RunID, generation); err != nil || !accepted {
				t.Fatalf("exact Ack rejected: accepted=%v err=%v", accepted, err)
			}
		})
	}
}

func TestMissingGenerationIndexRejectsMalformedPayloadGeneration(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	ctx := context.Background()
	for _, malformed := range []string{`false`, `null`, `"0"`, `{}`, `[]`} {
		t.Run(malformed, func(t *testing.T) {
			redistransport.FlushAll(ctx, rdb)
			q := redistransport.NewQueue(rdb)
			const runID = "malformed-generation"
			payload := fmt.Sprintf(`{"run_id":%q,"runner_kind":"kind","generation":%s}`, runID, malformed)
			if err := rdb.HSet(ctx, "rk:inflight:data", runID, payload).Err(); err != nil {
				t.Fatal(err)
			}
			if current, err := q.Renew(ctx, runID, 0); err != nil || current {
				t.Fatalf("malformed Renew accepted: current=%v err=%v", current, err)
			}
			if accepted, err := q.Ack(ctx, runID, 0); err != nil || accepted {
				t.Fatalf("malformed Ack accepted: accepted=%v err=%v", accepted, err)
			}
			if exists, err := rdb.HExists(ctx, "rk:inflight:data", runID).Result(); err != nil || !exists {
				t.Fatalf("rejected malformed assignment was removed: exists=%v err=%v", exists, err)
			}
		})
	}
}
