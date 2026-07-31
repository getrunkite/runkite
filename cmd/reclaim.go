package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/sharanharsoor/runkite/internal/transport"
)

// reclaimLeaderKey is a cluster-wide Redis lock so only one control-plane
// replica's reaper tick runs ReclaimStale at a time. Closes Kafka's
// non-atomic dual-reaper double-dispatch window when KAFKA_URL is paired
// with REDIS_URL (the only real multi-replica Kafka posture -- events/
// cancel already need Redis there). Redis/NATS queues don't need this
// for correctness (their own ReclaimStale is already CAS/Lua-atomic),
// but a single leader is harmless for them and keeps one code path.
const reclaimLeaderKey = "rk:reclaim-leader"

// reclaimLeaderTTL must outlast one ReclaimStale call plus a little
// slack, and expire soon enough that a dead holder doesn't stall
// recovery. Matched to the 2s reclaim ticker in reclaimStaleJobs.
const reclaimLeaderTTL = 3 * time.Second

// tryAcquireReclaimLeader attempts to become the cluster's reclaim
// leader for ttl. Returns (true, nil) when this replica should run
// ReclaimStale this tick. When rdb is nil (no REDIS_URL), returns
// (true, nil) so every replica reaps -- same as historical behavior,
// required for Kafka-without-Redis and in-process single-node.
func tryAcquireReclaimLeader(ctx context.Context, rdb *goredis.Client, holder string, ttl time.Duration) (bool, error) {
	if rdb == nil {
		return true, nil
	}
	if ttl <= 0 {
		ttl = reclaimLeaderTTL
	}
	return rdb.SetNX(ctx, reclaimLeaderKey, holder, ttl).Result()
}

// staleReclaimer is implemented by in-process, Redis, NATS, and Kafka queues.
type staleReclaimer interface {
	ReclaimStale(ctx context.Context, maxAge time.Duration) (int, error)
}

func reclaimStaleJobs(ctx context.Context, queue transport.JobQueue, rdb *goredis.Client) {
	r, ok := queue.(staleReclaimer)
	if !ok {
		return
	}
	// Keepalive detects a dead runner in ~4s; reclaim shortly after so a
	// resume-after-crash can recover within a normal client retry window.
	//
	// This threshold now protects a job's WHOLE execution, not just the
	// zombie-GetJob window it was originally sized for: the runner's
	// periodic Heartbeat RPC (bridge/server.go) and StreamEvents' first-event
	// Renew both reset the same in-flight clock this reads, at roughly
	// the same 2s cadence as this ticker. A live runner heartbeating
	// every ~2s never gets within one missed beat of this 6s cutoff;
	// a crashed one (zero heartbeats, not just a slow one) reliably
	// does. No retuning needed to cover the larger window -- the same
	// numbers that worked for "dequeue to first event" also work for
	// "dequeue to completion" once the clock is reset throughout,
	// rather than frozen at dequeue time.
	const maxAge = 6 * time.Second
	holder, _ := os.Hostname()
	if holder == "" {
		holder = "runkite"
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			won, err := tryAcquireReclaimLeader(ctx, rdb, holder, reclaimLeaderTTL)
			if err != nil {
				// Fail closed for this tick: a Redis blip must not reopen
				// dual-reaper double-dispatch. The next tick retries.
				slog.Warn("reclaim leader lock failed, skipping tick", "error", err)
				continue
			}
			if !won {
				continue
			}
			n, err := r.ReclaimStale(ctx, maxAge)
			if err != nil {
				slog.Warn("failed to reclaim stale jobs", "error", err)
				continue
			}
			if n > 0 {
				slog.Info("reclaimed stale jobs", "count", n, "max_age", maxAge)
			}
		}
	}
}
