# Internal Scale/Profiling Report

Produced via `bench/loadgen` against `examples/echo_agent` (a trivial, near-zero-compute
graph -- isolates the control plane's own overhead from any real agent's LLM/tool-call
latency). Machine: 14 logical CPUs,
Apple Silicon, all services local (control plane, Postgres, Redis, MongoDB, runner all on
one host -- no network latency between them).

## Results

| Config | Concurrency | Duration | Total | Errors | p50 | p90 | p99 | RSS delta |
|---|---|---|---|---|---|---|---|---|
| SQLite + in-process | 100 | 30s | 6,432 | 0 | 463ms | 522ms | 571ms | 48MB |
| Postgres + Redis (pre-fix) | 100 | 30s | 2,608 | 0 | **1,221ms** | 1,463ms | 1,669ms | **223MB** |
| Postgres + in-memory | 100 | 30s | 5,388 | 0 | 559ms | 597ms | 660ms | 40MB |
| MongoDB + in-memory | 100 | 30s | 7,752 | 0 | 384ms | 410ms | 474ms | 58MB |
| Postgres + Redis (pre-fix) | 20 | 20s | 1,219 | 0 | 331ms | 357ms | 428ms | 111MB |
| Postgres + Redis (**post-fix**, fresh keyspace) | 100 | 20s | 2,260 | 0 | **910ms** | 989ms | 1,028ms | -- |
| SQLite + in-process (isolating the runner) | 1 | 10s | 635 | 0 | 15ms | 17ms | 24ms | -- |
| SQLite + in-process (isolating the runner) | 20 | 10s | 927 | 0 | 213ms | 232ms | 338ms | -- |
| SQLite + in-process (isolating the runner) | 100 | 20s | 3,669 | 0 | 533ms | 707ms | 927ms | -- |
| Postgres, runner `--concurrency 20`, pool_max_conns=14 | 100 | 30s | 7,120 | 0 | 415ms | 462ms | 519ms | -- |
| Postgres, runner `--concurrency 100`, pool_max_conns=50 | 100 | 20s | 5,033 | 0 | 389ms | 426ms | 525ms | -- |
| Postgres, **2 runner replicas** `--concurrency 100` each | 100 | 20s | 6,169 | 0 | 308ms | 520ms | 732ms | -- |
| MySQL + in-memory | 100 | 30s | 6,031 | 0 | 494ms | 546ms | 679ms | 5MB |
| MySQL + Redis | 100 | 30s | 3,647 | 0 | 798ms | 912ms | 1,717ms | 0MB* |

Zero errors across every configuration and every concurrency level -- the correctness bar
(from the earlier smoke test and the full conformance suite) holds under load too, this
report is purely about latency/memory characteristics.

**This section was corrected after profiling.** The original findings 1-2 below (kept,
struck through in spirit, superseded by 1a/1b/1c) hypothesized Redis connection-pool
contention as the cause of the concurrency=20-vs-100 non-linearity and flagged it as
needing a CPU profile to confirm. That profile was taken. It pointed somewhere completely
different: two real, fixable Redis-specific bugs, plus a third, much bigger finding that
has nothing to do with Redis at all.

### 1a. Real bug, fixed: every run leaked a Redis Pub/Sub subscription + 2 goroutines, forever

`internal/bridge/server.go`'s `GetJob` called
`s.cancelBus.SubscribeCancel(context.Background(), runID)` -- `context.Background()` never
cancels. `SubscribeCancel`'s own internal goroutine (`internal/transport/redis/redis.go`)
only exited via a **real** cancel message arriving; for a run that completes normally (the
common case -- most runs are never cancelled), that goroutine, and the Redis Pub/Sub
connection `pubsub.Channel()` holds open, leaked for the rest of the process's life.

Confirmed via `/debug/pprof/goroutine?debug=1` taken mid-load: **11,252 total goroutines**
after roughly 1,800 completed runs against one long-lived control plane -- 3,646 each stuck
in `PubSub.ReceiveTimeout`, `channel.initHealthCheck` (go-redis's own per-subscription
health-check ticker), and this package's own `SubscribeCancel.func1`. A fresh instance with
zero prior runs starts at ~15 goroutines.

**Fixed** two ways, both required: (1) `GetJob` now creates a run-scoped
`context.WithCancel` *before* calling `SubscribeCancel` and passes that in, so
`cleanupRun` (already called from `ReportStatus`, which fires for every terminal run)
cancels the subscription too, not just the outer watcher goroutine. (2)
`SubscribeCancel`'s internal goroutine no longer closes its return channel on `ctx.Done()`
(only a real cancel message does) -- closing it there too would have made "ctx cancelled"
indistinguishable from "real cancel arrived" to a caller selecting on both, which would
have spuriously fired cancels for normally-completing runs. Verified: goroutine count now
stays flat at 15 after thousands of completed runs (was 11,252). The same leak class
existed in the in-process `CancelBus` too (an unbounded `c.subs` map, no Pub/Sub connection
involved so lower-cost, but a real unbounded memory leak on the zero-dependency default
transport) -- fixed identically. `CancelBroker`'s interface doc comment now states the
contract explicitly (ctx cancellation must release the subscription). Regression tests:
`TR-024`/`TR-025` in `internal/transport/conformance/conformance.go`, run against both
implementations; verified both fail against the pre-fix Redis behavior and pass after.

### 1b. Real bug, fixed: run event streams never expired, and a per-dispatch `SCAN` made that expensive

Redis's own `commandstats` taken during a clean load run told the real story: `SCAN`
accounted for over 90% of total command time (1.6s of ~1.7s) -- **152,214 `SCAN` calls for
750 completed runs in 15 seconds**, ~203 per run. Two compounding causes:

- `internal/bridge/server.go`'s `GetJob` called `s.queue.Len(ctx)` synchronously on
  **every single job dispatch**, purely to update a metrics gauge. `Queue.Len`
  (`internal/transport/redis/redis.go`) finds queue keys via `SCAN ... MATCH rk:queue:*`,
  which is cursor-based and must visit every key in the keyspace regardless of how few
  actually match -- there's no way to skip non-matching keys cheaply.
- `Broker.Publish` (`XAdd`) never set any expiry on a run's `rk:events:<run_id>` stream, so
  every completed run's stream persisted in Redis **forever**. A long-lived test instance
  had accumulated 5,476 such keys (10,952 total) purely from historical test runs going
  back to container start -- that's the keyspace `SCAN` was paying to traverse on every
  dispatch, for a lookup that only cares about a handful of `rk:queue:*` keys.

**Fixed** two ways: (1) the per-request `Len()` calls are gone from both `GetJob` and
`api.Server.enqueue` (the HTTP create path still called `Len` after the first pass of
this fix — same SCAN cost, just one fewer call site), replaced with a single
`pollQueueDepth` background ticker (`cmd/serve.go`, every 5s) -- a gauge metric doesn't
need per-request freshness. (2) `Publish` and `Close`
(`internal/transport/redis/redis.go`) now set a 24h expiry on the stream key itself
whenever a run reaches a terminal state, matching `closedKey`'s existing 24h TTL
convention, so streams no longer accumulate without bound over a deployment's lifetime.
Regression tests: `TestRedisBroker_StreamExpiresAfterTerminalEvent`,
`TestRedisBroker_CloseAlsoExpiresStream` (`internal/transport/redis/redis_test.go`).

A follow-up pass caught that `TR-024`/`TR-025` (finding 1a) only prove the *returned
channel* behaves correctly on ctx cancellation -- not that Redis itself released the
underlying subscription. Added `TestCancelBusContextCancelReleasesPubSub`
(`internal/transport/redis/cancel_leak_internal_test.go`) as harder, direct proof: it
inspects Redis via `PUBSUB NUMSUB` and asserts subscription count drops to zero after ctx
cancellation, not just that the Go-level channel stays quiet. Added the equivalent direct
check for the in-process implementation too
(`TestCancelBusContextCancelDrainsSubsMap`, inspects the `subs` map size directly).

**Honestly disclosed, one fixed since, others not blockers:**
- ~~A run that never reaches a terminal event or `Close` leaves its event stream without a
  TTL~~ -- **fixed**: a non-terminal `Publish` now also sets a long (7-day) safety-net
  expiry, refreshed on every event; only a terminal event or `Close` tightens it down to
  the 24h window. See `hungRunStreamTTL` in `internal/transport/redis/redis.go` and
  `TestRedisBroker_NonTerminalEventStillSetsATTL`.
- A runner process that crashes before its `ReportStatus` call leaves that run's
  cancel-watch goroutine/Redis subscription alive until the runner process itself dies --
  narrower than the pre-fix window (which was "forever, for every run"), but not zero.
- `Queue.Len` is still `SCAN`-based under the hood. Moving it off the per-request hot path
  onto a 5s ticker (finding 1b) makes this a non-issue at current scale, but it's still the
  wrong primitive for a very large keyspace -- an explicit counter (e.g. `INCR`/`DECR`
  alongside enqueue/dequeue) would be the correct long-term fix if `Queue.Len` ever needs
  to be called more frequently or the keyspace grows much larger than tested here.

Effect on the original benchmark: re-running the exact same Postgres+Redis,
concurrency=100 config against a fresh keyspace with both fixes applied dropped p50 from
1,221ms to 910ms (~25% improvement) -- real, but nowhere near enough to explain the
original 1,221ms-vs-331ms (concurrency=100-vs-20) gap. That gap has a different, much
larger cause.

### 1c. The actual dominant cause: the Python runner processes exactly one run at a time, and this has nothing to do with Redis

`python/runkite_runner/worker.py`'s `_poll_loop` is a single `while True` loop, invoked
once (`await _poll_loop(...)` in `run_worker`, not wrapped in `asyncio.gather` or spawned
as multiple tasks). Each iteration calls `GetJob`, then `await execute_run(...)` **for the
full duration of that run**, before looping back to poll for the next job. One runner
process therefore has a job-processing concurrency of exactly 1, regardless of how many
requests hit the control plane concurrently or which transport it uses.

Proven by isolating the variable: running the identical loadgen sweep
(concurrency 1/20/100) against the **zero-dependency SQLite + in-process backend** --
no Redis, no network hop between the control plane and its queue/broker at all --
reproduces the *same shape* of concurrency-dependent slowdown: 15ms p50 at concurrency=1,
213ms at concurrency=20 (~10.7ms per concurrent unit), 533ms at concurrency=100 (~5.3ms
per concurrent unit). This is textbook single-server-queue behavior (each request waits
for its turn behind however many are ahead of it in the one runner's sequential loop), and
it shows up identically whether or not Redis is anywhere in the picture. The original
report's framing -- "Redis transport is the dominant bottleneck," "this points at
contention" -- was measuring a real effect but attributing it to the wrong layer: swap
Redis for SQLite+in-process and the *non-linear-looking* concurrency=20-vs-100 jump is
still there, just at smaller absolute numbers (Redis's per-operation network round trip is
a worse constant factor than SQLite's local file I/O, which is why Redis's numbers were
worse in absolute terms -- but it's a difference of degree on top of the same underlying
single-runner queueing bottleneck, not a different mechanism).

**This means the previously-planned "shared/multiplexed stream-tailing design" and further
Redis-side pool tuning are not the next lever** -- they'd shave a further constant-factor
sliver off an already-secondary cost. The actual lever for higher concurrent throughput is
runner-side: give a runner process more than one in-flight job at a time (e.g. multiple
concurrent `_poll_loop`-style workers sharing one process, or horizontally scaling runner
processes/replicas of the same `runner_kind`, which the queue's fair-dispatch design
already supports since any runner of that kind can pick up any job). That's a runner
concurrency-model change, not a transport tuning task -- out of scope for this pass,
flagged here as the real next step if higher concurrent throughput is a goal.

### 1d. Runner-side concurrency: built, and it genuinely works -- with an honest, important limit

Following up on 1c's own flagged next step: both Python poll loops
(`python/runkite_runner/worker.py` and `generic_worker.py`, the latter shared by the
CrewAI/LlamaIndex/LangChain adapters) now dispatch up to `--concurrency`/`RUNKITE_CONCURRENCY`
jobs at once per process (semaphore-bounded dispatcher spawning one `asyncio.Task` per job,
default 1 -- fully backward compatible), and `RunkiteStore`/`vectorstore.py`'s direct-mode
Postgres access moved from a single shared connection behind an `asyncio.Lock` to a real
`psycopg_pool.AsyncConnectionPool` so concurrent jobs' store ops don't serialize on one
connection.

**The dispatch mechanism is proven correct and effective for a burst of work**: 20
concurrent runs against the same static `graph_id`, each with a unique identifiable input,
came back with zero cross-contamination and a combined wall time of 0.11s versus a 1.58s
sequential sum (~14x) -- see the unit tests (`test_generic_worker.py`,
`test_worker_concurrency.py`) and the live e2e verification for the receipts. Cancelling one
of two concurrent runs (both unit-level, against fakes, and live against a real 3-step
`slow_agent`) leaves the other completely unaffected.

**Under sustained, continuous high-volume load, a single runner process hits a different,
real ceiling: its own CPU core.** Re-running the exact `bench/loadgen` concurrency=100 sweep
from finding 1c, now with `--concurrency 20` and `--concurrency 100`, barely improved on the
sequential baseline (p50 ~390-420ms vs. the original ~518-533ms) -- a real but modest
improvement, not the order-of-magnitude jump the burst test above would suggest. Investigated
properly rather than accepted at face value: ruled out the control plane's own Postgres pool
size (identical result at pool_max_conns=14 vs. 50), ruled out the checkpointer specifically
(MemorySaver and pooled `AsyncPostgresSaver` showed the *same* plateau, which also rules out
`AsyncPostgresSaver`'s own internal `asyncio.Lock()` -- confirmed by reading
`langgraph/checkpoint/postgres/aio.py`'s `_cursor()` that it serializes on that lock
regardless of whether `conn` is a single connection or a pool, a real limitation in that
library, not something this fix could address by pooling harder). Direct measurement found
the actual cause: `ps` showed the runner process at **~100-106% CPU** throughout the load --
one Python process, one CPU core saturated, GIL-bound. `asyncio` concurrency only overlaps
*waiting* (I/O), not CPU work, and `echo_agent` (deliberately near-zero-compute, per this
report's own opening description) has almost nothing but CPU-bound overhead per run
(JSON/protobuf (de)serialization, LangGraph's own graph-execution bookkeeping) and no slow
I/O wait to actually overlap -- close to a worst case for this specific optimization, not a
representative one. Running a second runner replica against the same control plane (zero
config changes, same `runner_kind`) pushed measured total completed requests up ~22% in the
same window, directionally confirming horizontal replicas are the correct remedy once one
process's CPU core is the limit -- not a clean 2x on this shared, heavily-loaded dev machine
(also running the control plane, Postgres, and an unrelated runner/control
plane throughout this session), so treat that specific number as directional, not precise.

**Honest takeaway**: `--concurrency` is a real, correct, and valuable fix for the workload it
actually targets -- an agent whose wall-clock time is dominated by *waiting* (slow LLM API
calls, tool calls, external HTTP requests), where many concurrent jobs really do spend most
of their time not touching the CPU at all. It is not a way to exceed one CPU core's worth of
throughput for a CPU-bound or near-zero-compute agent like `echo_agent` -- that needs
horizontal runner replicas (already supported, zero control-plane changes, confirmed
directionally above), not a bigger `--concurrency` number. A genuinely rigorous
before/after benchmark for the *intended* workload would use an agent with an artificial
async sleep standing in for LLM latency, not `echo_agent` -- flagged as a good follow-up, not
done here since it's a benchmarking-methodology task, not a code change.

Also found and fixed along the way: CrewAI's `Crew` object is **not** safe for concurrent
`akickoff()` calls on the same shared instance -- confirmed by reading crewai's own
`crew.py`: both `kickoff`/`akickoff` write results directly onto shared instance attributes
(`self.usage_metrics`, `self._task_output_handler.reset()`/`.update()`), which would race
and corrupt each other under concurrency, unlike a LangGraph compiled graph (state is
checkpointer-keyed by thread_id, not stored on the graph object) or a plain LangChain
Runnable (stateless by design). Fixed with a per-`graph_id` `asyncio.Lock` in
`python/adapters/crewai_adapter/adapter.py` serializing concurrent `akickoff()` calls on the
same shared Crew -- correct, though it means CrewAI runs sharing a `graph_id` don't get real
parallelism from `--concurrency` the way LangGraph/LangChain/LlamaIndex runs do (building a
fresh Crew per run, LangGraph's Factory Graph pattern, would remove this ceiling; a bigger
change than a concurrency-safety fix warrants on its own). LlamaIndex's adapter was already
safe by design (its own docstring: reconstructs `chat_history` from `RunAssignment.input`
every call rather than relying on a shared engine's mutable history).

### 3. Memory growth plateaus under sustained load, doesn't run away

The original manual smoke test (8 rounds of 150 concurrent runs against SQLite) showed
RSS growth decelerating and flattening (48MB -> ... -> ~60MB total) rather than growing
per-round indefinitely -- normal Go heap sizing to a working set, not a leak. This
report's per-config RSS deltas are single 20-30s snapshots, consistent with that same
early-growth-then-plateau shape, not independent confirmation of a longer plateau -- a
real regression-guard would sample RSS over several consecutive multi-minute windows per
config, not a single window each.

### 4. MongoDB and MySQL both perform comparably to (within the same band as) SQLite for this workload

MongoDB: 384ms p50 vs SQLite's 463ms, roughly the same RSS delta magnitude (58MB vs 48MB).
MySQL (added in a later pass, once `internal/state/mysql` and its `cmd/serve.go` wiring
existed): 494ms p50, between SQLite and Postgres-in-memory's 559ms, closer to SQLite than to
Postgres despite MySQL and Postgres both being row-store SQL engines with a similar driver
shape (`database/sql` + a Go driver) -- not surprising given the earlier finding (#5) that all
state backends land in the same reasonable band for this workload; a 65-115ms spread inside
that band isn't a meaningful backend-choice signal on its own.

All three run the Python runner in **proxy mode** for checkpoints/store here (SQLite because
the control plane owns the file exclusively; MongoDB and MySQL because there's no direct-mode
checkpointer for either yet -- see README's Retention/Runners sections), which is the fairer
comparison: this isolates the *state backend's* own read/write cost from any transport
differences, and both MongoDB and MySQL hold up well against the SQLite baseline they're most
naturally compared to.

**MySQL + Redis** (798ms p50, 912ms p90, 1,717ms p99, 3,647 total over 30s) was also run as
the direct MySQL analogue of the existing "Postgres + Redis (post-fix, fresh keyspace)" row
(910ms p50, 989ms p90, 1,028ms p99, 2,260 total over 20s) -- **note the different durations
(30s vs 20s), so the two totals aren't a like-for-like throughput comparison, only the
percentile latencies are.** At **p50 and p90 the two are close** (798ms vs 910ms; 912ms vs
989ms), reinforcing finding #5's conclusion again: the queue/broker transport choice, not the
SQL backend behind it, is what separates the in-memory-transport rows (~400-560ms) from the
Redis-transport rows (~800-910ms) here. **The tail does NOT hold up the same way**: MySQL +
Redis's p99 (1,717ms) is ~67% higher than Postgres + Redis's post-fix p99 (1,028ms), despite a
*better* p50 -- a real, honestly-reported divergence, not glossed over as "close enough" the
way the p50/p90 comparison is. One 30s sample isn't enough to say whether this is a genuine
MySQL-driver-under-Redis-contention tail effect or a one-off (a slow migration/connection-pool
warm-up moment, GC pause, or shared-machine noise from the other services also running
throughout this session -- see this report's own opening caveat) -- flagged as a real, open
question rather than either explained away or silently dropped. RSS delta reads as 0MB for
this specific run (`ps`-based single-process RSS sampling landed on an already-stabilized
process this time, not a real "zero growth" finding) -- treat that one number as noise, not a
result; the p50/p90/p99 numbers are the real signal from this run.

### 5. Real correctness bug found and fixed: `/wait` could report "success" while a plain GET immediately after still showed "pending"

Discovered while extending `bench/loadgen` to check status via a separate GET after the
blocking `/wait` call, instead of trusting `/wait`'s own embedded status field -- a
stricter test pattern that exposed that `waitForExistingRun`/`waitForRun`
(`internal/api/runs.go`) derived the terminal status
from the event stream and patched it into the in-memory response object **without writing
it back to the store**. The actual persistence normally happens in `StatusCallback`,
triggered by the runner's separate `ReportStatus` RPC -- which arrives some (usually small,
but real) amount of time after the last event streams through. A client that calls `/wait`
then immediately does a plain `GET` on the same run -- a completely reasonable pattern --
could see a stale `"pending"` status. Reproduced reliably even at concurrency=2 once the
loadgen tool checked status via a separate GET instead of trusting `/wait`'s own embedded
field. **Fixed**: both functions now persist the terminal status via `UpdateRunStatus`
before responding, closing the window; `StatusCallback`'s later write of the same value is
a harmless idempotent no-op. Regression tests: `TestTS_WaitPersistsStatusBeforeResponding`,
`TestTS_CreateAndWaitPersistsStatusBeforeResponding` (`internal/api/api_test.go`) --
verified both fail against the pre-fix code and pass after.

## Optimization opportunities (ranked)

1. ~~Explicitly tune the Redis client's `PoolSize`/`MaxActiveConns`~~ -- **done, findings
   1a/1b above.** Profiling under load found two real, unrelated-to-pool-size bugs
   (a permanent cancel-subscription leak, and an unbounded event-stream keyspace making a
   per-dispatch `SCAN` expensive) instead of pool contention. Both fixed; `PoolSize` itself
   was never the issue (go-redis's `10 * GOMAXPROCS` default was comfortably above the
   concurrency levels tested).
2. ~~CPU-profile a concurrency=100 Postgres+Redis run~~ -- **done.** Also captured a
   goroutine profile and Redis's own `commandstats`/keyspace size, which is what actually
   surfaced 1a/1b -- the CPU profile alone (dominated by syscall/netpoll time, ~34% overall
   utilization) wasn't enough to see either bug; both are I/O/keyspace-shaped problems, not
   CPU-shaped ones.
3. ~~Runner-side concurrency~~ -- **done, finding 1d above.** Both Python poll loops now
   dispatch multiple in-flight jobs per process (`--concurrency`/`RUNKITE_CONCURRENCY`), and
   `RunkiteStore`/`vectorstore.py` moved to real connection pools. Genuinely effective for
   I/O-wait-dominated workloads (proven: ~14x wall-time speedup for a burst of concurrent
   runs); does **not** exceed one CPU core's worth of throughput for a CPU-bound/near-zero-
   compute agent under sustained load (confirmed via direct measurement: the runner process
   at ~100-106% CPU during the loadgen sweep) -- horizontal runner replicas are the correct
   remedy for that case, not a bigger `--concurrency` number, and already work with zero
   control-plane changes.
4. A shared/multiplexed stream-tailing design (one `XREAD` per stream instead of per
   subscriber) is no longer believed to be a meaningful win given 1c -- demoted from the
   original ranking. Each active run already has exactly one tailer in practice (Broker.
   Subscribe is called once per `/wait` or SSE connection per run, not per poll), so the
   per-connection overhead this would have removed was smaller than originally assumed.
5. **Not a priority:** the state backend choice itself (SQLite/Postgres/MongoDB) -- all
   three perform within a reasonable band of each other for this workload once 1a/1b are
   fixed; the queue/broker layer's remaining gap versus SQLite/in-process (910ms vs 533ms
   at concurrency=100) is consistent with Redis's inherent network-round-trip cost, not a
   backend-specific defect worth chasing further right now.

## What this report does NOT cover

- Real agent workloads (LLM calls, tool calls) -- `echo_agent` isolates control-plane
  overhead deliberately; a real agent's own latency will dwarf these numbers in practice.
- Multi-node control plane or multi-node Redis/Postgres -- single-host only.
- The CrewAI/LlamaIndex/plain-LangChain adapters' own overhead -- not benchmarked here,
  out of scope for "state backend + transport" comparison; worth a follow-up if any of
  those frameworks' own execution latency becomes a concern.
- Finding each system's actual breaking point (OOM, sustained failures) -- the
  resource-constrained comparison above stopped at 100 concurrent users with both systems
  still producing zero errors; pushing further until one fails would need a dedicated run.
- **The TypeScript runner's own overhead.** Every number in this report is the Python
  runner. The TypeScript runner reached full feature parity with it in a later pass (A2A
  client, vector store client, factory graphs), but its latency/throughput characteristics
  under load haven't been measured at all -- worth a dedicated follow-up given it's a
  structurally different runtime (Node's event loop vs. Python's asyncio/GIL, which finding
  1d above found to be the actual ceiling for the Python runner under sustained CPU-bound
  load).
