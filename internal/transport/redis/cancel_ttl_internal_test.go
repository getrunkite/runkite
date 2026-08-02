package redistransport

import (
	"context"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/getrunkite/runkite/internal/transport"
)

// TestCancelMarkerExpires proves canceled run_ids are not kept forever:
// after canceledMemberTTL, Enqueue of the same run_id is allowed again
// (the marker is treated as gone). Without the ZSET+TTL, rk:canceled
// would suppress that run_id for the life of the Redis.
func TestCancelMarkerExpires(t *testing.T) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set")
	}
	opt, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := goredis.NewClient(opt)
	defer rdb.Close()
	_ = FlushAll(context.Background(), rdb)
	q := NewQueue(rdb)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	orig := nowMillis
	t.Cleanup(func() { nowMillis = orig })
	nowMillis = func() int64 { return base }

	runID := "run-cancel-ttl"
	if err := q.Cancel(ctx, runID); err != nil {
		t.Fatal(err)
	}
	ok, err := q.isCanceled(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected cancel marker present immediately after Cancel")
	}

	// Past TTL: marker must not suppress enqueue.
	nowMillis = func() int64 { return base + canceledMemberTTL.Milliseconds() + 1 }
	ok, err = q.isCanceled(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expired cancel marker must not still count as canceled")
	}

	job := &transport.RunAssignment{
		RunID: runID, ThreadID: "t1", GraphID: "echo", RunnerKind: "test-runner",
	}
	if err := q.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	got, err := q.Dequeue(ctx, "test-runner", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.RunID != runID {
		t.Fatalf("expected re-enqueue after TTL, got %+v", got)
	}
}
