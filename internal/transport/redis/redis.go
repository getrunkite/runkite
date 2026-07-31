// Package redistransport implements the transport.JobQueue, transport.EventBroker,
// and transport.CancelBroker interfaces using Redis.
//
// JobQueue uses Redis Lists (LPUSH/BRPOP) per runner_kind.
// EventBroker uses Redis Streams (XADD/XREAD) per run_id — every subscriber
// tails the stream directly via a blocking XREAD goroutine, so delivery works
// identically whether publisher and subscriber are in the same process or on
// different nodes.
// CancelBroker uses Redis Pub/Sub — SubscribeCancel blocks on the Redis
// subscription, not a local map.
package redistransport

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/sharanharsoor/runkite/internal/transport"
)

// --------------------------------------------------------------------------
// JobQueue
// --------------------------------------------------------------------------

// Queue implements transport.JobQueue using Redis Lists, with in-flight
// tracking held in Redis itself (not process memory) -- see the two
// package-level keys below and reclaimStaleScript's doc comment for why.
//
// Previously (superseded after a confirmed incident): in-flight tracking
// (Ack/Nack/ReclaimStale) was a plain Go map
// on this struct, scoped to whichever control-plane process happened to
// call Dequeue. That's fine for a single instance, but breaks in exactly
// the multi-instance topology this project's own docker-compose.multi.yml
// exists to test, in two confirmed ways: (1) if the replica that dequeued
// a job crashes, no *other* replica's map ever had that entry, so nobody
// can reclaim it -- silent loss, not just "unreaped for a while"; (2) with
// a plain load-balanced gRPC bridge (no session affinity), a runner's
// GetJob and its later completion call can land on different replicas --
// the completing replica's Ack is a no-op on a map that never had the
// entry, the dequeuing replica's entry lingers, and its own reaper
// eventually re-enqueues an already-finished job. Moving the bookkeeping
// into Redis (visible to every replica) closes both.
type Queue struct {
	rdb *redis.Client
}

// NewQueue creates a new Redis-backed job queue.
func NewQueue(rdb *redis.Client) *Queue {
	return &Queue{rdb: rdb}
}

func queueKey(runnerKind string) string { return "rk:queue:" + runnerKind }

const canceledSetKey = "rk:canceled"

// inflightZSetKey holds one entry per in-flight (dequeued, not yet Ack'd)
// job: member=run_id, score=dequeued-at as a Unix millisecond timestamp.
// A sorted set (not a plain set) specifically so ReclaimStale can ask
// Redis directly for "everything older than this cutoff" via
// ZRANGEBYSCORE, rather than fetching every in-flight job and filtering
// in Go -- the same shape as SQS's visibility-timeout queue or a Redis
// Streams consumer group's pending-entries list, adapted to this
// project's plain-list queue instead of adopting Streams wholesale.
const inflightZSetKey = "rk:inflight:zset"

// inflightDataKey holds the actual job payload for each in-flight run_id
// (field=run_id, value=JSON-encoded RunAssignment) -- needed so ANY
// replica's ReclaimStale can re-enqueue the job, not just the one that
// originally dequeued it.
const inflightDataKey = "rk:inflight:data"

// inflightGenKey holds each in-flight run_id's CURRENT fencing
// generation as a plain integer (field=run_id, value=generation),
// separate from the job payload in inflightDataKey. This is
// deliberately NOT read out of the job's own JSON via a Lua string
// search: a job's own input/config is arbitrary content the payload
// carries verbatim, and a naive search for the generation field's text
// inside that whole string can find a coincidental match nested inside
// that content instead of the real top-level field, silently pointing
// the fencing check at the wrong value. A single, dedicated Redis field
// per run_id, read via a plain hash lookup, removes that ambiguity
// entirely. Dequeue writes it once; ReclaimStale is the only thing
// that ever increments it (via HINCRBY, atomic).
const inflightGenKey = "rk:inflight:generation"

// nowMillis is a var (not a plain time.Now call) so tests can freeze/
// control it without sleeping in wall-clock time -- see redis_test.go's
// use of this for exercising ReclaimStale's cutoff boundary precisely.
var nowMillis = func() int64 { return time.Now().UnixMilli() }

func (q *Queue) Enqueue(ctx context.Context, job *transport.RunAssignment) error {
	ok, err := q.rdb.SIsMember(ctx, canceledSetKey, job.RunID).Result()
	if err != nil {
		return err
	}
	if ok {
		return nil
	}

	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return q.rdb.LPush(ctx, queueKey(job.RunnerKind), data).Err()
}

// dequeueBlockCap bounds a single BRPOP call's blocking duration,
// regardless of how much of the overall Dequeue timeout remains. This
// matters for a reason beyond "don't block too long on one call": a
// runner whose gRPC connection just died (crash, restart) leaves its
// GetJob call's ctx blocked inside exactly this BRPOP until grpc-go's
// keepalive detects the dead peer and cancels it -- confirmed live to
// take ~4-6s. BRPOP's blocking-command
// semantics mean Redis can atomically pop a freshly-pushed item to
// deliver to a connection at almost the same instant that connection is
// being torn down for cancellation -- a genuine Redis/network race
// (not a Go channel, which never loses a value this way) where the item
// is removed from the list server-side but never actually received
// client-side, because the connection closed mid-delivery. Confirmed
// live via a real flake: a resume request's newly-enqueued job vanished
// this exact way, at the same millisecond a zombie GetJob's 30s-long
// single BRPOP call got cancelled. Capping each call to this shorter
// duration doesn't make the race impossible (it can still happen on any
// individual call), but shrinks a zombie's vulnerable window from up to
// the full GetJob timeout (~30s) down to this cap, and the explicit
// ctx.Err() check below means a cancelled context is noticed within one
// cap-sized tick instead of only when BRPOP itself eventually surfaces
// the cancellation.
const dequeueBlockCap = 2 * time.Second

func (q *Queue) Dequeue(ctx context.Context, runnerKind string, timeout time.Duration) (*transport.RunAssignment, error) {
	deadline := time.Now().Add(timeout)
	key := queueKey(runnerKind)

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil
		}

		// BRPOP timeout is integer seconds in Redis. Sub-second values get
		// truncated to 0 which means "block forever." Clamp to 1s minimum
		// to avoid that; the slight over-block is harmless. Also capped at
		// dequeueBlockCap -- see its own doc comment for why.
		blockTime := remaining
		if blockTime > dequeueBlockCap {
			blockTime = dequeueBlockCap
		}
		if blockTime < time.Second {
			blockTime = time.Second
		}

		result, err := q.rdb.BRPop(ctx, blockTime, key).Result()
		if err != nil {
			if err == redis.Nil {
				// Timeout — check if our actual deadline has passed
				if time.Now().After(deadline) {
					return nil, nil
				}
				continue
			}
			return nil, err
		}
		if len(result) < 2 {
			return nil, nil
		}

		var job transport.RunAssignment
		if err := json.Unmarshal([]byte(result[1]), &job); err != nil {
			continue
		}

		canceled, _ := q.rdb.SIsMember(ctx, canceledSetKey, job.RunID).Result()
		if canceled {
			continue
		}

		// Record in-flight state in Redis itself (not a local map) so any
		// replica's Ack/ReclaimStale can see it -- see this type's own doc
		// comment. One small, deliberately-accepted residual risk (a
		// "ponytail" ceiling, not an oversight): if this process crashes in
		// the gap between BRPop succeeding and this pipeline completing,
		// the job is popped from the queue but never recorded in-flight, so
		// it's lost the same way the old design lost it for a job's ENTIRE
		// execution time. That window here is a single Redis round-trip
		// (single-digit milliseconds), not minutes -- a large, worthwhile
		// improvement even though it isn't a mathematically perfect
		// zero-gap guarantee. Closing the gap fully would need BRPOPLPUSH/
		// LMOVE's atomic pop-and-transfer semantics instead of BRPOP, a
		// larger change than this fix warrants on its own.
		// job.Generation here comes from Go's own structured JSON
		// unmarshal above, which resolves the top-level "generation"
		// field unambiguously regardless of anything else the payload
		// contains -- unlike inflightGenKey's own doc comment describes
		// for the Lua side, there's no string-search risk here at all.
		if _, err := q.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.HSet(ctx, inflightDataKey, job.RunID, result[1])
			pipe.ZAdd(ctx, inflightZSetKey, redis.Z{Score: float64(nowMillis()), Member: job.RunID})
			pipe.HSet(ctx, inflightGenKey, job.RunID, job.Generation)
			return nil
		}); err != nil {
			return nil, err
		}
		return &job, nil
	}
}

// fencingCheck is shared Lua logic (via a comment, not a callable Lua
// function -- these scripts are each their own standalone EVAL) between
// ackScript and renewScript: given the CURRENT generation on record for
// a run_id (from inflightGenKey, not parsed out of the job payload --
// see that key's own doc comment for why) and a presented generation,
// decide whether to proceed. generation 0 (from a pre-fencing runner
// build) or a current generation of 0 (a run_id tracked before this
// field existed -- only possible during a rolling upgrade window) both
// mean "don't enforce fencing here," matching how Ack/Renew behaved
// unconditionally before fencing existed -- see transport.JobQueue's
// Ack/Renew doc comments for the full rationale.

// ackScript atomically checks the presented generation against the
// in-flight run's own recorded generation before removing it -- see
// transport.JobQueue's Ack doc comment for why this must be one
// Redis-side operation, not a separate HGET-then-decide-then-HDEL from
// Go (a concurrent ReclaimStale between those steps could otherwise
// remove a CURRENT job based on a check that was true a moment ago but
// isn't anymore).
const ackScript = `
local data_key = KEYS[1]
local zset_key = KEYS[2]
local gen_key = KEYS[3]
local run_id = ARGV[1]
local generation = tonumber(ARGV[2])

if redis.call('HEXISTS', data_key, run_id) == 0 then
    return 0
end

if generation ~= 0 then
    local current_gen_str = redis.call('HGET', gen_key, run_id)
    local current_gen = current_gen_str and tonumber(current_gen_str) or 0
    if current_gen ~= 0 and current_gen ~= generation then
        return 0
    end
end

redis.call('HDEL', data_key, run_id)
redis.call('ZREM', zset_key, run_id)
redis.call('HDEL', gen_key, run_id)
return 1
`

// Ack acknowledges successful processing of a job, fenced by generation
// -- see transport.JobQueue's Ack doc comment for the full rationale.
func (q *Queue) Ack(ctx context.Context, runID string, generation int64) (bool, error) {
	result, err := q.rdb.Eval(ctx, ackScript,
		[]string{inflightDataKey, inflightZSetKey, inflightGenKey},
		runID, generation,
	).Result()
	if err != nil {
		return false, err
	}
	n, _ := result.(int64)
	return n == 1, nil
}

// renewScript atomically resets an in-flight job's staleness score, but
// ONLY if it's still tracked in inflightDataKey AND (fencing) the
// presented generation still matches the run's own recorded generation
// -- a plain two-step "check, then ZADD" from Go would have a race
// window where a concurrent ReclaimStale on another replica removes or
// supersedes the job between the two calls, and this Renew would
// either resurrect a phantom zset entry with no backing data, or
// (worse, post-fencing) extend the lease for a generation that's
// already been superseded.
const renewScript = `
local data_key = KEYS[1]
local zset_key = KEYS[2]
local gen_key = KEYS[3]
local run_id = ARGV[1]
local score = ARGV[2]
local generation = tonumber(ARGV[3])

if redis.call('HEXISTS', data_key, run_id) == 0 then
    return 0
end

if generation ~= 0 then
    local current_gen_str = redis.call('HGET', gen_key, run_id)
    local current_gen = current_gen_str and tonumber(current_gen_str) or 0
    if current_gen ~= 0 and current_gen ~= generation then
        return 0
    end
end

redis.call('ZADD', zset_key, score, run_id)
return 1
`

// Renew extends an in-flight job's lease -- see transport.JobQueue's
// Renew doc comment for the heartbeat mechanism and fencing this backs.
func (q *Queue) Renew(ctx context.Context, runID string, generation int64) (bool, error) {
	result, err := q.rdb.Eval(ctx, renewScript,
		[]string{inflightDataKey, inflightZSetKey, inflightGenKey},
		runID, nowMillis(), generation,
	).Result()
	if err != nil {
		return false, err
	}
	n, _ := result.(int64)
	return n == 1, nil
}

func (q *Queue) Nack(ctx context.Context, runID string) error {
	data, err := q.rdb.HGet(ctx, inflightDataKey, runID).Result()
	if err == redis.Nil {
		return nil // not in-flight (already Ack'd/reclaimed/never existed) -- nothing to do
	}
	if err != nil {
		return err
	}

	if _, err := q.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HDel(ctx, inflightDataKey, runID)
		pipe.ZRem(ctx, inflightZSetKey, runID)
		pipe.HDel(ctx, inflightGenKey, runID)
		return nil
	}); err != nil {
		return err
	}

	var job transport.RunAssignment
	if err := json.Unmarshal([]byte(data), &job); err != nil {
		return err
	}
	return q.Enqueue(ctx, &job)
}

func (q *Queue) Cancel(ctx context.Context, runID string) error {
	if _, err := q.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HDel(ctx, inflightDataKey, runID)
		pipe.ZRem(ctx, inflightZSetKey, runID)
		pipe.HDel(ctx, inflightGenKey, runID)
		return nil
	}); err != nil {
		return err
	}
	return q.rdb.SAdd(ctx, canceledSetKey, runID).Err()
}

// reclaimStaleScript atomically finds every in-flight job older than the
// given cutoff, removes it from in-flight tracking, and re-enqueues it
// (unless it's been canceled meanwhile) -- all as one Redis-server-side
// operation. That atomicity is the actual point, not just an optimization:
// Redis executes one Lua script to completion before starting the next
// command from ANY client, so if every control-plane replica's reaper
// ticker calls this same script, only the replica whose call Redis happens
// to run first will find and claim each stale entry -- by the time a
// second replica's call runs (a few milliseconds later at most, never
// concurrently), those entries are already gone from the zset. This is
// what makes ReclaimStale safe to call from every replica simultaneously,
// the same correctness property TryClaimCronFire's SQL "INSERT ... ON
// CONFLICT DO NOTHING" gives cron claiming, achieved here via Redis's own
// single-threaded script execution instead of a SQL unique constraint.
//
// Bumping the fencing generation is done via HINCRBY on the dedicated
// gen_key, NOT by finding-and-replacing an existing "generation":N
// substring inside the job's own JSON payload -- an earlier version did
// exactly that and had a confirmed, live-reproduced bug: a plain text
// search for that substring can match a coincidental occurrence nested
// inside the job's own input/config content instead of the real
// top-level field (Go marshals those fields before the top-level one,
// so a naive first-match search finds the wrong one whenever a nested
// value happens to contain that same text), silently leaving the real
// generation unbumped while corrupting unrelated user data. Reading and
// writing the generation from its own dedicated Redis field sidesteps
// that ambiguity entirely; the job payload itself is still given a
// fresh top-level "generation" field for the runner's benefit, by
// APPENDING one rather than searching for and replacing anything --
// every mainstream JSON parser (Go, Python, JavaScript, this same
// Lua library) resolves a duplicate key by taking the LAST value, so
// appending always wins over whatever the payload already contains,
// with no search needed at all.
const reclaimStaleScript = `
local zset_key = KEYS[1]
local data_key = KEYS[2]
local canceled_key = KEYS[3]
local gen_key = KEYS[4]
local cutoff = ARGV[1]

local stale_ids = redis.call('ZRANGEBYSCORE', zset_key, '-inf', cutoff)
local reclaimed = 0

for _, run_id in ipairs(stale_ids) do
    local job_json = redis.call('HGET', data_key, run_id)
    redis.call('ZREM', zset_key, run_id)
    redis.call('HDEL', data_key, run_id)

    if job_json then
        local is_canceled = redis.call('SISMEMBER', canceled_key, run_id)
        if is_canceled == 0 then
            local job = cjson.decode(job_json)
            local new_gen = redis.call('HINCRBY', gen_key, run_id, 1)
            -- Go's json.Marshal never emits trailing whitespace, so a
            -- compact JSON object's very last byte is always '}' --
            -- anchoring on that (not searching for any existing
            -- "generation" text) is what makes this append safe
            -- regardless of what the rest of the payload contains.
            local new_job_json = job_json:gsub('}$', ',"generation":' .. new_gen .. '}')
            redis.call('LPUSH', 'rk:queue:' .. job.runner_kind, new_job_json)
            reclaimed = reclaimed + 1
        else
            redis.call('HDEL', gen_key, run_id)
        end
    end
end

return reclaimed
`

// ReclaimStale re-enqueues jobs dequeued more than maxAge ago without Ack.
// Safe to call concurrently from multiple control-plane replicas -- see
// reclaimStaleScript's own doc comment for why.
func (q *Queue) ReclaimStale(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := nowMillis() - maxAge.Milliseconds()
	result, err := q.rdb.Eval(ctx, reclaimStaleScript,
		[]string{inflightZSetKey, inflightDataKey, canceledSetKey, inflightGenKey},
		cutoff,
	).Result()
	if err != nil {
		return 0, err
	}
	n, ok := result.(int64)
	if !ok {
		return 0, fmt.Errorf("unexpected reclaim script result type %T", result)
	}
	return int(n), nil
}

func (q *Queue) Len(ctx context.Context) (int64, error) {
	var total int64
	iter := q.rdb.Scan(ctx, 0, "rk:queue:*", 100).Iterator()
	for iter.Next(ctx) {
		n, err := q.rdb.LLen(ctx, iter.Val()).Result()
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, iter.Err()
}

// Ping is a plain PING, deliberately not the SCAN-based Len above --
// O(1) regardless of keyspace size, safe to call on every readiness probe.
func (q *Queue) Ping(ctx context.Context) error {
	return q.rdb.Ping(ctx).Err()
}

// --------------------------------------------------------------------------
// EventBroker
// --------------------------------------------------------------------------

// Broker implements transport.EventBroker using Redis Streams.
// Every Subscribe call spawns an XREAD-based goroutine that tails the stream
// directly from Redis — no local fan-out map. This means delivery works
// identically whether publisher and subscriber are in the same process or on
// separate nodes.
type Broker struct {
	rdb *redis.Client

	mu      sync.Mutex
	nextID  uint64
	tailers map[string]map[uint64]context.CancelFunc // run_id -> tailer_id -> cancel
}

// NewBroker creates a new Redis-backed event broker.
func NewBroker(rdb *redis.Client) *Broker {
	return &Broker{
		rdb:     rdb,
		tailers: make(map[string]map[uint64]context.CancelFunc),
	}
}

// Ping verifies Redis is reachable. transport.EventBroker itself has no
// Ping method (in-process and NATS brokers share their queue's own
// already-checked connection, so requiring every implementation to add
// one would be pure boilerplate for them) -- GET /readyz instead type-
// asserts for this optional method, which only Redis needs: it's the
// one broker that can be paired with a DIFFERENT queue backend
// (KAFKA_URL + REDIS_URL, see cmd/serve.go's initTransport), so its own
// connection genuinely needs its own check.
func (b *Broker) Ping(ctx context.Context) error {
	return b.rdb.Ping(ctx).Err()
}

func streamKey(runID string) string { return "rk:events:" + runID }
func closedKey(runID string) string { return "rk:closed:" + runID }

// eventStreamTTL bounds how long a completed run's event stream survives
// in Redis before expiring, matching closedKey's own existing 24h
// window. Real gap found via pprof/keyspace inspection under load:
// XAdd never set any expiry on the stream key, so EVERY run's stream
// persisted in Redis forever -- confirmed 5,476 accumulated rk:events:*
// keys (one per historical run, going back to container start) on a
// long-lived test Redis instance. That bloated keyspace is what made
// Queue.Len's SCAN-based lookup (see below) slow, which is what
// actually caused bench/REPORT.md's "Redis gets slower under
// concurrency" finding -- not connection-pool contention, and not the
// separate (also real, already fixed) cancel-subscription leak.
const eventStreamTTL = 24 * time.Hour

// hungRunStreamTTL is a safety-net expiry refreshed on every non-terminal
// event, not just the terminal one. Without this, a run that never
// reaches a terminal event or Close at all -- a genuine hang or a
// runner crash mid-execution, not a normal completion -- would leave
// its stream with no TTL forever, since eventStreamTTL above is only
// ever applied once a run actually finishes. Long enough not to
// interfere with any legitimately long-running agent execution, short
// enough that an abandoned run's stream doesn't accumulate permanently
// either. Superseded by the tighter eventStreamTTL the moment a run
// does reach a terminal event, since Expire always resets the
// countdown to exactly the given duration from now, not "extend if
// longer."
const hungRunStreamTTL = 7 * 24 * time.Hour

// Publish appends an event to the run's Redis Stream. If the event is terminal,
// sets a closed marker so late subscribers get an immediately-closed channel,
// and expires the stream itself so it doesn't accumulate forever.
// No local fan-out — subscribers read from the stream via XREAD.
func (b *Broker) Publish(ctx context.Context, runID string, event *transport.RunEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}

	err = b.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey(runID),
		Values: map[string]interface{}{
			"data": string(data),
			"seq":  event.Seq,
		},
	}).Err()
	if err != nil {
		return err
	}

	if event.IsTerminal() {
		b.rdb.Set(ctx, closedKey(runID), "1", eventStreamTTL)
		b.rdb.Expire(ctx, streamKey(runID), eventStreamTTL)
	} else {
		b.rdb.Expire(ctx, streamKey(runID), hungRunStreamTTL)
	}

	return nil
}

// Subscribe returns a channel that receives events for the given runID by
// tailing the Redis Stream via blocking XREAD. The goroutine reads directly
// from Redis, so it works across processes/nodes.
func (b *Broker) Subscribe(ctx context.Context, runID string) (<-chan *transport.RunEvent, error) {
	ch := make(chan *transport.RunEvent, 4096)

	// If stream is already closed, return a closed channel immediately
	closed, _ := b.rdb.Exists(ctx, closedKey(runID)).Result()
	if closed > 0 {
		close(ch)
		return ch, nil
	}

	// Capture the current stream tail synchronously. Using Redis's "$"
	// marker would resolve when XREAD actually blocks on the server, not
	// when Subscribe returns — any Publish landing in that gap is silently
	// lost forever. Reading the tail here closes that race.
	lastID := "0-0"
	msgs, err := b.rdb.XRevRangeN(ctx, streamKey(runID), "+", "-", 1).Result()
	if err == nil && len(msgs) > 0 {
		lastID = msgs[0].ID
	}

	subCtx, cancel := context.WithCancel(ctx)

	b.mu.Lock()
	b.nextID++
	tailerID := b.nextID
	if b.tailers[runID] == nil {
		b.tailers[runID] = make(map[uint64]context.CancelFunc)
	}
	b.tailers[runID][tailerID] = cancel
	b.mu.Unlock()

	go b.tailStream(subCtx, runID, tailerID, lastID, ch)

	return ch, nil
}

// tailStream reads from the Redis Stream via blocking XREAD, forwarding events
// to ch. Exits on terminal event, context cancellation, or stream closure.
func (b *Broker) tailStream(ctx context.Context, runID string, tailerID uint64, lastID string, ch chan *transport.RunEvent) {
	defer close(ch)
	defer b.removeTailer(runID, tailerID)

	key := streamKey(runID)
	for {
		results, err := b.rdb.XRead(ctx, &redis.XReadArgs{
			Streams: []string{key, lastID},
			Count:   100,
			Block:   time.Second, // short block so we can check context/closed
		}).Result()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if err == redis.Nil {
				// Timeout with no new entries — check if stream was closed
				cl, _ := b.rdb.Exists(ctx, closedKey(runID)).Result()
				if cl > 0 {
					return
				}
				continue
			}
			// Transient Redis error — back off briefly and retry
			time.Sleep(50 * time.Millisecond)
			continue
		}

		for _, stream := range results {
			for _, msg := range stream.Messages {
				lastID = msg.ID

				dataStr, ok := msg.Values["data"].(string)
				if !ok {
					continue
				}
				var event transport.RunEvent
				if err := json.Unmarshal([]byte(dataStr), &event); err != nil {
					continue
				}

				select {
				case ch <- &event:
				case <-ctx.Done():
					return
				}

				if event.IsTerminal() {
					return
				}
			}
		}
	}
}

// Replay returns stored events after the given sequence number.
func (b *Broker) Replay(ctx context.Context, runID string, sinceSeq int64) ([]*transport.RunEvent, error) {
	msgs, err := b.rdb.XRange(ctx, streamKey(runID), "-", "+").Result()
	if err != nil {
		return nil, err
	}

	var events []*transport.RunEvent
	for _, msg := range msgs {
		dataStr, ok := msg.Values["data"].(string)
		if !ok {
			continue
		}
		var event transport.RunEvent
		if err := json.Unmarshal([]byte(dataStr), &event); err != nil {
			continue
		}
		if event.Seq > sinceSeq {
			events = append(events, &event)
		}
	}
	return events, nil
}

// removeTailer cleans up a specific tailing goroutine from the tailers map
// when it exits naturally (terminal event, closed marker, or context cancel).
func (b *Broker) removeTailer(runID string, tailerID uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m, ok := b.tailers[runID]; ok {
		delete(m, tailerID)
		if len(m) == 0 {
			delete(b.tailers, runID)
		}
	}
}

// Close marks a run's stream as finished. Cancels all tailing goroutines
// for this run (they close their channels on exit).
func (b *Broker) Close(runID string) error {
	b.rdb.Set(context.Background(), closedKey(runID), "1", eventStreamTTL)
	b.rdb.Expire(context.Background(), streamKey(runID), eventStreamTTL)

	b.mu.Lock()
	m := b.tailers[runID]
	delete(b.tailers, runID)
	b.mu.Unlock()

	for _, cancel := range m {
		cancel()
	}
	return nil
}

// --------------------------------------------------------------------------
// CancelBroker
// --------------------------------------------------------------------------

// CancelBus implements transport.CancelBroker using Redis Pub/Sub.
// SubscribeCancel creates a real Redis subscription — delivery works across
// nodes, not just in-process.
type CancelBus struct {
	rdb *redis.Client
}

// NewCancelBus creates a new Redis-backed cancel broker.
func NewCancelBus(rdb *redis.Client) *CancelBus {
	return &CancelBus{rdb: rdb}
}

// Ping verifies Redis is reachable -- see Broker.Ping's doc comment for
// why this optional method exists only on the Redis implementation.
func (c *CancelBus) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

func cancelChannel(runID string) string { return "rk:cancel:" + runID }

func (c *CancelBus) PublishCancel(ctx context.Context, runID string) error {
	return c.rdb.Publish(ctx, cancelChannel(runID), "cancel").Err()
}

// SubscribeCancel creates a Redis Pub/Sub subscription synchronously (the
// subscription is confirmed before returning) then spawns a goroutine to
// wait for the cancel message.
//
// ctx cancellation MUST release this subscription (see CancelBroker's
// doc comment) -- this used to be silently violated in practice because
// every caller passed context.Background(), which never cancels, so
// this goroutine (and the Redis connection pubsub.Channel() holds) only
// ever exited via a real cancel message. For a run that completes
// normally (never cancelled -- the common case), that meant the
// subscription leaked for the rest of the process's life. Confirmed via
// pprof under concurrent load: 3646 leaked subscriptions (each with 2
// background goroutines: the message reader and go-redis's own
// per-channel health-check ticker) after roughly 1800 completed runs
// against a single long-lived control plane -- the actual root cause of
// bench/REPORT.md's "Redis transport latency/memory blows up under
// concurrency" finding, not connection-pool contention as originally
// hypothesized there.
func (c *CancelBus) SubscribeCancel(ctx context.Context, runID string) (<-chan struct{}, error) {
	ch := make(chan struct{}, 1)

	pubsub := c.rdb.Subscribe(ctx, cancelChannel(runID))

	// Block until subscription is confirmed by Redis — ensures no race
	// between SubscribeCancel returning and a concurrent PublishCancel.
	if _, err := pubsub.Receive(ctx); err != nil {
		pubsub.Close()
		return nil, err
	}

	go func() {
		defer pubsub.Close()
		msgCh := pubsub.Channel()
		select {
		case <-msgCh:
			select {
			case ch <- struct{}{}:
			default:
			}
			close(ch)
		case <-ctx.Done():
			// ctx cancelled without a real cancel message (the run
			// completed normally) -- exit and release the Redis
			// subscription (deferred pubsub.Close() above) WITHOUT
			// closing ch. A caller selecting on both ch and this same
			// ctx must be able to tell "real cancel" (value sent, then
			// closed) apart from "I stopped waiting" -- closing ch here
			// too would make a closed-channel read indistinguishable
			// from a genuine cancel signal to that caller.
		}
	}()

	return ch, nil
}

// FlushAll clears all Redis keys used by this transport. For testing only.
func FlushAll(ctx context.Context, rdb *redis.Client) error {
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, "rk:*", 1000).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			rdb.Del(ctx, keys...)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return nil
}

// Compile-time interface checks.
var (
	_ transport.JobQueue     = (*Queue)(nil)
	_ transport.EventBroker  = (*Broker)(nil)
	_ transport.CancelBroker = (*CancelBus)(nil)
)
