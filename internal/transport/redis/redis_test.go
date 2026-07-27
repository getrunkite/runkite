package redistransport_test

import (
	"context"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/runkite/runkite/internal/transport"
	"github.com/runkite/runkite/internal/transport/conformance"
	redistransport "github.com/runkite/runkite/internal/transport/redis"
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
