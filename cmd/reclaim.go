package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/getrunkite/runkite/internal/config"
	"github.com/getrunkite/runkite/internal/metrics"
	"github.com/getrunkite/runkite/internal/transport"
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
// recovery. Longer than the 2s reclaim ticker on purpose: the current
// leader renews via reclaimLeaderScript each tick (SET with PX when
// value still equals its holder id), so a live leader keeps the lock
// continuously. A blind SetNX with TTL > tick left ~1s gaps every
// cycle (live-confirmed) because the holder couldn't renew against
// its own unexpired key.
const reclaimLeaderTTL = 3 * time.Second

// reclaimLeaderScript atomically acquires or renews the reclaim-leader
// lease:
//   - key absent, or value == our holder → SET with PX (acquire/renew), return 1
//   - value is another holder → leave untouched, return 0
//
// KEYS[1] = reclaimLeaderKey
// ARGV[1] = holder id
// ARGV[2] = TTL milliseconds
const reclaimLeaderScript = `
local cur = redis.call('GET', KEYS[1])
if cur == false or cur == ARGV[1] then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
  return 1
end
return 0
`

// tryAcquireReclaimLeader attempts to become (or remain) the cluster's
// reclaim leader for ttl. Returns (true, nil) when this replica should
// run ReclaimStale this tick. When rdb is nil (no REDIS_URL), returns
// (true, nil) so every replica reaps -- same as historical behavior,
// required for Kafka-without-Redis and in-process single-node.
func tryAcquireReclaimLeader(ctx context.Context, rdb *goredis.Client, holder string, ttl time.Duration) (bool, error) {
	if rdb == nil {
		return true, nil
	}
	if ttl <= 0 {
		ttl = reclaimLeaderTTL
	}
	res, err := rdb.Eval(ctx, reclaimLeaderScript, []string{reclaimLeaderKey}, holder, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// staleReclaimer is implemented by in-process, Redis, NATS, and Kafka queues.
type staleReclaimer interface {
	ReclaimStale(ctx context.Context, maxAge time.Duration, maxRetries int) (int, []transport.PoisonPill, error)
}

// defaultReclaimMaxAge matches the reclaim loop comment block below:
// ~2s runner Heartbeat cadence, 2s reclaim ticker, keepalive ~4s.
const defaultReclaimMaxAge = 6 * time.Second

// defaultReclaimMaxRetries matches plans/poison_pill_max_retries.md and
// PROTOCOL.md §13: original attempt + limited reclaims before permanent
// failure. Explicit reclaim.max_retries=0 (or env 0) means unlimited.
const defaultReclaimMaxRetries = 3

// initReclaimMaxRetries reads langgraph.json "reclaim.max_retries" from
// the first discovered config file, then applies RUNKITE_RECLAIM_MAX_RETRIES
// if set. Absent config → default 3.
func initReclaimMaxRetries(configPath string) int {
	maxRetries := defaultReclaimMaxRetries
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) > 0 {
		if cfg, err := config.LoadLangGraphJSON(paths[0]); err == nil && cfg.Reclaim != nil && cfg.Reclaim.MaxRetries != nil {
			maxRetries = *cfg.Reclaim.MaxRetries
		}
	}
	if v := os.Getenv("RUNKITE_RECLAIM_MAX_RETRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			slog.Error("RUNKITE_RECLAIM_MAX_RETRIES invalid; keeping prior value", "value", v, "using", maxRetries)
		} else {
			maxRetries = n
		}
	}
	return maxRetries
}

// initReclaimMaxAge reads langgraph.json "reclaim.max_age" from the first
// discovered config file, then applies RUNKITE_RECLAIM_MAX_AGE if set.
// Absent/invalid config → default 6s.
func initReclaimMaxAge(configPath string) time.Duration {
	maxAge := defaultReclaimMaxAge
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) > 0 {
		if cfg, err := config.LoadLangGraphJSON(paths[0]); err == nil && cfg.Reclaim != nil && cfg.Reclaim.MaxAge != "" {
			d, parseErr := time.ParseDuration(cfg.Reclaim.MaxAge)
			if parseErr != nil || d <= 0 {
				slog.Error("reclaim.max_age invalid; keeping prior value", "value", cfg.Reclaim.MaxAge, "using", maxAge, "error", parseErr)
			} else {
				maxAge = d
			}
		}
	}
	if v := os.Getenv("RUNKITE_RECLAIM_MAX_AGE"); v != "" {
		d, parseErr := time.ParseDuration(v)
		if parseErr != nil || d <= 0 {
			slog.Error("RUNKITE_RECLAIM_MAX_AGE invalid; keeping prior value", "value", v, "using", maxAge, "error", parseErr)
		} else {
			maxAge = d
		}
	}
	return maxAge
}

func reclaimStaleJobs(ctx context.Context, queue transport.JobQueue, rdb *goredis.Client, maxAge time.Duration, maxRetries int, statusCallback func(runID, status, errorMsg string)) {
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
	// rather than frozen at dequeue time. Tune via reclaim.max_age or
	// RUNKITE_RECLAIM_MAX_AGE; default 6s (see initReclaimMaxAge).
	holder, _ := os.Hostname()
	if holder == "" {
		holder = "runkite"
	}
	slog.Info("reclaim loop started", "max_retries", maxRetries, "max_age", maxAge)
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
			n, dead, err := r.ReclaimStale(ctx, maxAge, maxRetries)
			if err != nil {
				slog.Warn("failed to reclaim stale jobs", "error", err)
				continue
			}
			for _, pill := range dead {
				msg := fmt.Sprintf(
					"max retries exceeded (generation %d): runner crashed %d times without completing",
					pill.Generation, pill.Generation,
				)
				slog.Warn("poison pill detected",
					"run_id", pill.RunID,
					"generation", pill.Generation,
					"max_retries", maxRetries,
					"agent_id", pill.AgentID,
					"tenant_id", pill.TenantID,
				)
				agent := pill.AgentID
				if agent == "" {
					agent = "unknown"
				}
				tenant := pill.TenantID
				if tenant == "" {
					tenant = "default"
				}
				metrics.PoisonPillTotal.WithLabelValues(agent, tenant).Inc()
				if statusCallback != nil {
					statusCallback(pill.RunID, "error", msg)
				}
			}
			if n > 0 {
				slog.Info("reclaimed stale jobs", "count", n)
			}
		}
	}
}
