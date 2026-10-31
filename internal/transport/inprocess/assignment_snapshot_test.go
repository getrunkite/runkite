package inprocess

import (
	"context"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
)

func TestQueueGenerationSnapshotsDoNotAliasCallers(t *testing.T) {
	ctx := context.Background()
	q := NewQueue()
	source := &transport.RunAssignment{RunID: "snapshot", RunnerKind: "kind", Generation: 1}
	if err := q.Enqueue(ctx, source); err != nil {
		t.Fatal(err)
	}
	source.Generation = 900
	first, err := q.Dequeue(ctx, "kind", time.Second)
	if err != nil || first == nil || first.Generation != 1 {
		t.Fatalf("enqueue snapshot: job=%v err=%v", first, err)
	}
	first.Generation = 700
	if current, err := q.Renew(ctx, source.RunID, 1); err != nil || !current {
		t.Fatalf("dequeue copy mutated queue generation: current=%t err=%v", current, err)
	}
	if n, _, err := q.ReclaimStale(ctx, 0, 0); err != nil || n != 1 {
		t.Fatalf("reclaim: n=%d err=%v", n, err)
	}
	if first.Generation != 700 {
		t.Fatalf("reclaim mutated a dispatched assignment: %d", first.Generation)
	}
	second, err := q.Dequeue(ctx, "kind", time.Second)
	if err != nil || second == nil || second.Generation != 2 {
		t.Fatalf("replacement snapshot: job=%v err=%v", second, err)
	}
}
