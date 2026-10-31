package redistransport_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
	redistransport "github.com/getrunkite/runkite/internal/transport/redis"
)

func TestNackRetainsLeaseWhenReturningToReadyQueueFails(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	ctx := context.Background()
	redistransport.FlushAll(ctx, rdb)
	q := redistransport.NewQueue(rdb)
	input := json.RawMessage(`{"large":9007199254740993,"decimal":0.12345678901234567890123456789}`)
	job := &transport.RunAssignment{RunID: "nack-retention", RunnerKind: "kind", Generation: 1, Input: input}
	if err := q.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	if got, err := q.Dequeue(ctx, "kind", time.Second); err != nil || got == nil {
		t.Fatalf("dequeue: job=%v err=%v", got, err)
	}
	if err := rdb.Set(ctx, "rk:queue:kind", "wrong-type", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := q.Nack(ctx, job.RunID, 1); err == nil {
		t.Fatal("expected the ready-queue write to fail")
	}
	if current, err := q.Renew(ctx, job.RunID, 1); err != nil || !current {
		t.Fatalf("failed nack lost recoverable assignment: current=%t err=%v", current, err)
	}
	if err := rdb.Del(ctx, "rk:queue:kind").Err(); err != nil {
		t.Fatal(err)
	}
	if err := q.Nack(ctx, job.RunID, 1); err != nil {
		t.Fatal(err)
	}
	got, err := q.Dequeue(ctx, "kind", time.Second)
	if err != nil || got == nil || string(got.Input) != string(input) {
		t.Fatalf("retry changed or lost assignment: job=%v err=%v", got, err)
	}
}
