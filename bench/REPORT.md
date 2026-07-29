# Internal Scale/Profiling Report

Produced via `bench/loadgen` against `examples/echo_agent` (a trivial, near-zero-compute
graph -- isolates the control plane's own overhead from any real agent's LLM/tool-call
latency). Machine: 14 logical CPUs,
Apple Silicon, all services local (control plane, Postgres, Redis, MongoDB, runner all on
one host -- no network latency between them).

## Results

| Config | Concurrency | Duration | Total | Errors | p50 | p90 | p99 | RSS delta |
|---|---|---|---|---|---|---|---|---|
| SQLite + in-process (**pre-DSN-fix** -- see section 6, superseded below) | 100 | 30s | 6,432 | 0 | 463ms | 522ms | 571ms | 48MB |
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
| MySQL + Redis (single sample -- see section 4 for a 3-run follow-up, superseded below) | 100 | 30s | 3,647 | 0 | 798ms | 912ms | 1,717ms | 0MB* |
| **TS runner**, SQLite + in-memory (**pre-DSN-fix**) | 100 | 30s | 4,639 | 0 | 662ms | 699ms | 742ms | 5MB |
| **TS runner**, SQLite + in-memory (pre-DSN-fix, repeat) | 100 | 30s | 4,754 | 0 | 650ms | 711ms | 736ms | 5MB |
| **TS runner**, Postgres + Redis | 100 | 30s | 5,029 | 0 | 598ms | 655ms | 760ms | 0MB* |
| **TS runner**, Postgres + Redis (reversed order, run 1st) | 100 | 30s | 5,144 | 0 | 580ms | 638ms | 752ms | 0MB* |
| **TS runner**, SQLite + in-memory (pre-DSN-fix, reversed order, run 2nd) | 100 | 30s | 4,798 | 0 | 630ms | 696ms | 764ms | 0MB* |
| **TS runner**, SQLite + in-memory (post-DSN-fix, pool=8 -- **tried, reverted**) | 100 | 30s | 7,440 | 0 | 399ms | 531ms | 602ms | 0MB* |
| **TS runner**, SQLite + in-memory (post-DSN-fix, pool=8, repeat -- tried, reverted) | 100 | 30s | 7,386 | 0 | 398ms | 534ms | 634ms | 0MB* |
| **Python runner**, SQLite + in-process (post-DSN-fix, pool=8 -- **tried, reverted**) | 100 | 30s | 7,652 | 0 | 383ms | 541ms | 673ms | 0MB* |
| **Python runner**, SQLite + in-process (post-DSN-fix, pool=8, repeat -- tried, reverted) | 100 | 30s | 7,627 | 0 | 392ms | 536ms | 658ms | 0MB* |
| **TS runner**, SQLite + in-memory (**post-DSN-fix, pool=1 -- shipped**) | 100 | 30s | 8,199 | 0 | **366ms** | 475ms | 549ms | 0MB* |
| **TS runner**, SQLite + in-memory (post-DSN-fix, pool=1, repeat -- shipped) | 100 | 30s | 8,492 | 0 | 364ms | 461ms | 628ms | 0MB* |
| **Python runner**, SQLite + in-process (**post-DSN-fix, pool=1 -- shipped**) | 100 | 30s | 8,154 | 0 | **359ms** | 499ms | 602ms | 0MB* |
| **Python runner**, SQLite + in-process (post-DSN-fix, pool=1, repeat -- shipped) | 100 | 30s | 8,055 | 0 | 362ms | 497ms | 660ms | 0MB* |

**"pre-DSN-fix" rows are stale, kept for historical context, not current behavior. "pool=8"
rows are also superseded** -- both are kept to show the investigation's actual path, not
edited away. A real bug (section 6) meant SQLite's DSN silently never applied WAL mode or its
busy_timeout for this project's entire history until that section's investigation found and
fixed it. A first instinct to also widen the connection pool to 8 was tested, worked (0
errors), but turned out unnecessary and mildly counterproductive on p50/p90 once isolated from
the DSN fix (p99 was a wash, not a clean loss for pool=8) -- **the shipped configuration is
the fixed DSN with `MaxOpenConns(1)` unchanged from its original value**, which beats pool=8
on p50/p90 for both runners. See section 6
for the full four-stage story.

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

### 1e. The TypeScript runner's own concurrency ceiling: same headline shape as Python, but the *control plane* hits its own limit first, not the runner

Follow-up to 1d's own flagged gap (this exact test hadn't been repeated for Node). Same
method: `examples/echo_agent_ts`, zero-dependency SQLite + in-process control plane,
`bench/loadgen -concurrency 100 -duration 30s` sweep across the TS runner's own
`--concurrency 1/20/100`, sampling the runner process's `ps %cpu` throughout.

**A methodology mistake was made and caught before it reached this report**: the first sweep
reused one SQLite file across all three `--concurrency` stages back-to-back, so the
`--concurrency 100` stage ran against a database that already had ~13,000 rows from the two
prior stages -- and showed total requests going *down* as `--concurrency` went *up* (7,739 ->
5,538 -> 4,053) with runner CPU also dropping (35.9% -> 23.9% -> 18.8% avg), which read at
first like `--concurrency` actively hurting TS throughput. That would have been a real,
reportable, and wrong finding: a quick fresh-database spot-check at `--concurrency 100`
immediately produced p50=193ms against the accumulated-DB stage's 730ms -- a growing-SQLite-
file effect (consistent with this report's own established pattern of SQLite performance
depending on accumulated table size), not a concurrency regression. Repeating the full sweep
with a **fresh SQLite file per stage** gives the real result:

| `--concurrency` | Total | p50 | p90 | p99 | Runner CPU (avg/max) |
|---|---|---|---|---|---|
| 1 | 7,775 | 390ms | 494ms | 523ms | 33.1% / 71.7% |
| 20 | 10,468 | 266ms | 503ms | 582ms | 36.6% / 88.1% |
| 100 | 10,575 | 249ms | 518ms | 784ms | 36.7% / 88.7% |

Surface shape looks like Python's finding 1d: a real jump from baseline to `--concurrency 20`
(+35% throughput, p50 ~266ms vs ~390ms -- a ~32% drop, not the halving it might look like at a
glance), then an early plateau (`--concurrency 100` adds only ~1% over `--concurrency 20`).
p99/max latency both *rise* with concurrency (523ms -> 784ms p99) even as p50 improves -- the
expected cost of oversubscribing one process with more concurrent work: some jobs queue longer
while most finish faster. **But the cause is not the same as Python's, and the first draft of
this section claimed it was without actually checking** -- worth walking through as a real
example of pattern-matching to a prior finding instead of verifying it:

Python's finding 1d directly implicated the *runner's own CPU core*: `ps` showed that runner
process at a sustained ~100-106% throughout the load, and the investigation explicitly ruled
out the control plane's Postgres pool size and checkpointer as causes first. The TS numbers
above don't show that pattern: **average** runner CPU stays at 33.1-36.7% across all three
concurrency levels -- only the **max** sample reaches 88-89%, meaning most of the 30s window
the runner isn't CPU-bound at all, just occasionally spiking. That's a materially weaker
signal than "sustained one-core saturation," and shouldn't have been described as the same
underlying limit as Python's. The actual cause traces to the *control plane* instead --
covered next, since the same measurement also explains the replica result below.

**Horizontal runner replicas did not help, and the control plane is why.** Running 2 TS
runner replicas (`--concurrency 20` each, same `runner_kind`, zero control-plane config
changes) against the same control plane produced 10,302 total -- statistically the same as one
replica's 10,468-10,575, not an uplift. Sampling the **control plane's own** `ps %cpu` during a
*separate*, standalone single-replica `--concurrency 20` verification run (10,349 total --
close to but not one of the three sweep rows above, run independently to get this measurement)
showed it running at **118.7% avg / 144.2% max** -- already over one full CPU core's worth of
work, at the same `--concurrency 20` setting and the same ~10,300-10,500-per-30s throughput
level as the single-runner sweep's plateau above. That's the more likely explanation for
*both* results, not just the replica one: the single-process Go control plane, not the TS
runner's event loop, is plausibly already the saturated resource by the time `--concurrency
20` is reached, which would explain why going to `--concurrency 100` on one runner barely
moves the needle (only ~1%) *and* why a second replica adds nothing -- both are trying to push
more work through a control plane that's already near its own ceiling. Consistent with section
6's own finding that SQLite's `MaxOpenConns` stays at 1 (serializing every write onto one
connection) even after the DSN/WAL fix -- a plausible mechanism for *this specific
configuration's* ceiling, though this section didn't isolate the control plane's CPU
independently at every sweep stage to confirm it's the cause at `--concurrency 100` too, only
at `--concurrency 20` in a separate verification run, so treat this as the best-supported
explanation rather than a fully isolated proof.

**Not a clean Python-vs-Node comparison, since the backends differ**: Python's 1d replica
test (and the pool-size/checkpointer investigation before it) ran against **Postgres**
(`pool_max_conns` 14 vs. 50, `AsyncPostgresSaver`), where the control plane's Postgres pool
was explicitly ruled out as the cause and the runner's own CPU was directly implicated
instead. This TS test ran against **SQLite + in-process** (`MaxOpenConns(1)`), a backend with
a much lower, more easily-reached write-serialization ceiling by design. So "Python's replica
helped, TS's didn't" is not evidence about Node vs. Python as runtimes -- it's two different
backends hitting two different bottlenecks (runner CPU vs. control-plane write path) at two
different throughput levels, and this report doesn't have a same-backend Python vs. TS
replica comparison to say whether Python's runner-CPU story would also apply to TS under
Postgres, or whether TS's control-plane story would also apply to Python under SQLite. Both
are plausible open questions, neither tested here. **Practical reading**: for this specific
zero-dependency (SQLite + in-process) configuration, the ceiling worth chasing next -- if this
throughput level is a real target -- is the control plane's own
write path, not more runner replicas or a bigger `--concurrency`; a Redis/Postgres transport
(this report's own section 6 default recommendation is still SQLite + in-process for typical
single-runner use, so this only matters at this specific higher-throughput tier) or genuine
horizontal control-plane scaling would be the next things to test, not attempted here.

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

**MySQL + Redis** was originally reported from a single 30s sample (798ms p50, 912ms p90,
1,717ms p99, 3,647 total) compared against a single Postgres + Redis sample that used a
**different duration** (post-fix, fresh keyspace: 910ms p50, 989ms p90, 1,028ms p99, 2,260
total over **20s**, not 30s) -- an honest same-percentiles-different-totals caveat at the
time, but still only an n=1-vs-n=1 comparison with a duration mismatch. Both numbers below are
now superseded by a proper repeat-run, matched-duration follow-up:

**Follow-up: 3x MySQL + Redis and 3x Postgres + Redis, both at the identical 100-concurrency,
30s-duration `bench/loadgen` config, same host, same commit, run back-to-back** (raw output in
`/tmp/loadgen_mysql_redis_results.txt` / `/tmp/loadgen_pg_redis_results.txt` at the time of
this pass -- not committed, reproduce via the commands in this section if needed):

| | p50 | p90 | p99 | total |
|---|---|---|---|---|
| MySQL + Redis, run 1 | 885ms | 970ms | 1,224ms | 3,377 |
| MySQL + Redis, run 2 | 916ms | 1,009ms | 1,065ms | 3,298 |
| MySQL + Redis, run 3 | 878ms | 930ms | 1,022ms | 3,466 |
| Postgres + Redis, run 1 | 700ms | 765ms | 801ms | 4,282 |
| Postgres + Redis, run 2 | 686ms | 749ms | 794ms | 4,407 |
| Postgres + Redis, run 3 | 769ms | 902ms | 1,092ms | 3,916 |

(An earlier, exploratory trio of MySQL + Redis runs in this same pass -- p99 935ms/1,033ms/
1,649ms -- pointed the same direction and is consistent with the table above, but its raw
`loadgen` stdout wasn't piped to a file, only observed in the terminal at the time, so it's
mentioned here for color and is deliberately **not** folded into the numbers/claims below,
which rest only on the three-plus-three runs with an auditable log on disk.)

**Two findings, both more modest than the original single-sample comparison suggested:**

1. **The p99 divergence direction holds, but at roughly 1.3x, not the originally-reported
   1.67x.** MySQL + Redis's p99s (1,022-1,224ms) and Postgres + Redis's p99s (794-1,092ms)
   overlap at the edges (Postgres's own worst run, 1,092ms, lands inside MySQL's range) -- a
   mild, noisy tendency for MySQL + Redis to run a somewhat higher tail in this environment,
   not a clean, reproducible MySQL-specific defect. The original 1,717ms was a real
   observation, not a fluke number invented after the fact, but it was the high end of natural
   run-to-run variance, not a representative constant -- three runs is still a small enough
   sample that this framing itself could still move with more data; treat "~1.3x, overlapping"
   as the current best estimate, not a settled ratio.
2. **In this specific matched-30s-duration campaign, Postgres + Redis was clearly faster on
   p50** (686-769ms vs MySQL + Redis's 878-916ms) -- the opposite ranking from the original
   table's mismatched-duration comparison (MySQL 798ms vs Postgres 910ms). This is **not**
   claimed as "the ranking flipped" in any general sense -- the original two numbers came from
   different sessions *and* different durations (20s vs 30s), so they were never a controlled
   comparison to begin with, and this new data doesn't retroactively falsify them. What this
   matched, controlled re-run *does* show cleanly: **within one same-host, same-commit,
   back-to-back session, which SQL backend "wins" on p50 is close enough (a ~160ms gap on a
   ~700-900ms baseline) that it's plausible for a different session, on the same shared dev
   machine running other services throughout, to land the other way.** That's a stronger,
   better-controlled version of finding #5's original point (transport choice, not SQL backend
   choice, is what actually separates the latency bands here) than the single-sample numbers
   ever demonstrated -- the backend-vs-backend ordering isn't stable enough within the noise of
   this measurement setup to be worth ranking at all.

**Honest bookkeeping**: n=3 vs n=3, not n=6 vs n=6 -- asymmetric sample sizes, stated plainly
rather than padded out. A single high-tail run on either side (Postgres's own 1,092ms p99 in
run 3, for instance) visibly moves a 3-sample mean; more repeats on both sides would tighten
these bands further but weren't run here since the qualitative conclusion (mild/overlapping,
not a clean backend defect) was already clear from what's above. RSS deltas are omitted from
this table -- the original single-sample run's "0MB" reading was already flagged as `ps`
sampling landing on an already-stabilized process rather than a real zero-growth finding, and
this follow-up didn't re-attempt RSS measurement.

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

### 6. TypeScript runner: SQLite-vs-Redis inversion investigated end to end -- a wrong mutex hypothesis disproven, a real DSN bug found and fixed, a connection-pool fix tried and found unnecessary, and SQLite ends up the fastest default single-runner state-backend config for both runners

Same methodology as the Python runner's own comparison rows (`examples/echo_agent_ts`,
concurrency=100, proxy mode for checkpoint/store since the TS runner has no direct-mode
option at all -- see README's Runners section). Five runs total, deliberately alternating
which config went first, specifically to test an obvious confound before trusting the
comparison at all:

- **SQLite + in-memory, run 1st in session**: 662ms p50 / 699ms p90 / 742ms p99 (4,639 total,
  30s).
- **SQLite + in-memory, repeated 2nd in session**: 650ms p50 / 711ms p90 / 736ms p99 (4,754
  total, 30s).
- **Postgres + Redis, run 3rd in session (after both SQLite runs above)**: 598ms p50 / 655ms
  p90 / 760ms p99 (5,029 total, 30s).
- **Postgres + Redis, repeated on a fresh process, run 1st this time (reversed order,
  no prior SQLite run in that process's lifetime)**: 580ms p50 / 638ms p90 / 752ms p99
  (5,144 total, 30s).
- **SQLite + in-memory, repeated on a fresh process, run 2nd this time (after the reversed
  Redis run above)**: 630ms p50 / 696ms p90 / 764ms p99 (4,798 total, 30s).

**Order-independence confirmed, not assumed.** Before trusting "Redis is faster than SQLite
for TS" as a real effect rather than a JIT-warm-up or OS-page-cache artifact (Node's V8 does
warm up hot code paths across repeated calls within a process's lifetime, and each config
here needs a fresh `tsx` process), the SQLite/Redis run order was deliberately reversed and
re-measured. Redis stayed in the ~580-598ms band whether it ran 1st or 3rd; SQLite stayed in
the ~630-662ms band whether it ran 1st, 2nd (repeat), or 2nd-after-Redis. This rules out
"whichever config runs last in a warmed-up process wins" as the explanation -- the two
configs occupy genuinely separate, non-overlapping latency bands regardless of run order.

**Root cause investigated with real profiles (RUNKITE_PPROF=1), not just reasoned about --
the original mutex hypothesis was wrong, and the actual mechanism is more interesting.**
`runtime.SetMutexProfileFraction(1)`/`runtime.SetBlockProfileRate(1)` were added to `cmd/serve.go`
(gated behind the same `RUNKITE_PPROF=1` opt-in, since `/debug/pprof/mutex` and
`/debug/pprof/block` are silently empty forever without them) specifically to test the
"`Broker`'s single global mutex" hypothesis directly, instead of leaving it as an untested
guess:

- **Mutex contention is negligible for both configs**: 118ms total lock-wait delay for
  SQLite+in-memory, 22ms for Postgres+Redis, over an 18s sampling window at concurrency=100.
  Not the cause. The original hypothesis above was concrete and testable, and testing it
  disproved it -- worth stating plainly rather than quietly dropping.
- **CPU profiling found the real, much larger effect**: over the same 18s window, the SQLite
  config burned **7.03s of CPU** vs. Postgres+Redis's **4.24s** -- SQLite consumed ~66% MORE
  total CPU for the identical workload, despite needing zero network hops. `syscall.rawsyscalln`
  alone accounted for 76.5% of the SQLite config's CPU time (vs. 42.7% for Postgres+Redis,
  where `runtime.kevent` -- real network I/O polling -- accounts for another 28.3%). The pure-Go
  `modernc.org/sqlite` driver (no CGo, so no native SQLite C code -- everything, including
  low-level file I/O, goes through Go's own syscall layer) is simply syscall-heavy per query.

**Why this doesn't show up for Python**: this cost lives entirely in the Go control plane,
identical code regardless of which runner language is attached -- so it should, in principle,
be paid equally by both. The likely explanation is proportional, not mechanistic: Python's own
per-job overhead is already large and CPU-bound (findings 1c/1d: one process pegged at
~100-106% of a single core), so this SQLite driver cost is a small fraction of Python's ~463ms
total, while switching Python to Postgres+Redis adds pure network-hop latency on top of an
already CPU-starved process with no offsetting benefit -- hence Python's SQLite number stays
its fastest. TS/Node's own per-job overhead for this trivial graph appears to be much smaller
(unmeasured directly, but consistent with Node's design), so the same SQLite driver cost is a
proportionally larger share of TS's total, and removing it (switching to Postgres+Redis) is a
net win for TS despite adding network hops. Not confirmed by isolating TS's own baseline
overhead directly -- stated as the working theory, not a proven fact.

**First attempt at a fix: widen the connection pool. Made things drastically worse --
~99.8% errors -- for a reason that turned out to be a completely separate, much older bug.**
`internal/state/sqlite/sqlite.go` had `db.SetMaxOpenConns(1)`, forcing literally every query
(reads included) through one Go connection -- an obvious thing to try widening given the
CPU-cost finding above. Widening it to 8 (matching the MySQL backend's own
`SetMaxOpenConns(5)`-style convention) passed the full `-race` test suite cleanly, but under
the actual concurrency=100 loadgen sweep produced a **~99.8% error rate**, every single one
`SQLITE_BUSY: database is locked`. Reverted immediately, confirmed the revert restored the
original ~630-662ms/0-error baseline exactly.

**An independent review then found the actual bug the pool-widening experiment had exposed
-- and it predates this whole investigation.** `New()`'s DSN used mattn/go-sqlite3 query-param
forms (`?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON`), but `modernc.org/sqlite`
(the driver this project actually uses) only honors its own `_pragma=name(value)` syntax --
the mattn-style params are silently ignored, no error, connection opens fine. Verified live,
independently, against the actual driver in this repo:

```
mattn-style DSN (what New() actually used) => journal_mode=delete  busy_timeout=0    foreign_keys=0
_pragma=... DSN (what modernc.org/sqlite requires) => journal_mode=wal  busy_timeout=5000  foreign_keys=1
```

WAL mode and the 5-second busy_timeout this file's own comments claimed were active had
**never once been applied, for the entire project history** -- the explicit follow-up
`db.Exec("PRAGMA foreign_keys = ON")` a few lines later was real SQL (not a DSN param) and did
correctly enable that one pragma independently, so foreign-key cascades were never actually
broken; only WAL and busy_timeout were silent no-ops. This had zero observable effect under
the normal `MaxOpenConns(1)` configuration (a single connection is never internally lock-
contended regardless of journal mode), which is exactly why it went unnoticed until an
experiment introduced real concurrent connections for the first time. The ~99.8% failure
wasn't "waited 5 seconds then gave up" as this report first assumed -- it was "collided and
failed instantly, zero retry budget," a more severe failure mode than documented.

**Fixed the DSN** (`?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)`,
`:memory:` gets `_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)` only, since an in-memory
database can't actually be WAL) and pinned it with two new regression tests
(`pragma_test.go`) asserting the real `PRAGMA journal_mode`/`busy_timeout`/`foreign_keys`
values a connection sees, not just that `New()` returns no error -- exactly the gap that let
the original bug survive undetected: nothing ever checked that a DSN param actually applied
its pragma.

**Re-ran the pool-widening experiment under the corrected DSN -- a clean, verified pass,
but a fourth stage below shows it wasn't even the right fix.** Widened `MaxOpenConns` to 8
again (still pinned to 1 for `:memory:` -- an in-memory SQLite database is connection-scoped,
not process-scoped, so a second connection there gets its own separate, empty database, not a
shared one, confirmed the hard way earlier in this same investigation). Full `-race` suite
green (3x). Four independent 30s/concurrency-100 loadgen runs, two per runner, all zero
errors:

| Config | Run | p50 | p90 | p99 | Total |
|---|---|---|---|---|---|
| TS runner, SQLite (fixed DSN + pool=8) | 1st | 399ms | 531ms | 602ms | 7,440 |
| TS runner, SQLite (fixed DSN + pool=8) | 2nd | 398ms | 534ms | 634ms | 7,386 |
| Python runner, SQLite (fixed DSN + pool=8) | 1st | 383ms | 541ms | 673ms | 7,652 |
| Python runner, SQLite (fixed DSN + pool=8) | 2nd | 392ms | 536ms | 658ms | 7,627 |

Both runners got meaningfully faster on p50, not just error-free: TS's SQLite p50 dropped from
the original ~650ms to ~399ms. Python's dropped from ~463ms to ~387ms, landing in the same
band as Mongo+in-memory (384ms) and Postgres-with-`--concurrency 100` (389ms) -- a tie for
best Python p50, not a unique winner. **A real p99 cost, not papered over**: Python's
post-fix p99 (658-673ms) is ~100ms *worse* than its pre-fix single-connection p99 (571ms).
Independent review flagged this and asked whether the p50 win genuinely needed the pool
widening, or whether it came entirely from the DSN fix -- these four runs alone can't answer
that, since they change DSN and pool size together.

**Stage 4: isolated the two changes -- the pool widening turned out to be unnecessary for the
win, and mildly counterproductive on p50/p90. Reverted a second time, this time for good.**
Ran the fixed DSN with `MaxOpenConns` held at 1 (no widening at all) and compared directly
against the pool=8 numbers above, same workload, same machine, back to back -- two runs per
runner, matching the pool=8 table above:

| Config | Run | p50 | p90 | p99 | Total |
|---|---|---|---|---|---|
| TS runner, SQLite (fixed DSN, **pool=1**) | 1st | 366ms | 475ms | 549ms | 8,199 |
| TS runner, SQLite (fixed DSN, **pool=1**) | 2nd | 364ms | 461ms | 628ms | 8,492 |
| Python runner, SQLite (fixed DSN, **pool=1**) | 1st | 359ms | 499ms | 602ms | 8,154 |
| Python runner, SQLite (fixed DSN, **pool=1**) | 2nd | 362ms | 497ms | 660ms | 8,055 |

**p50 and p90 are a clean, consistent win for pool=1 on both runners, across all four
runs**: ~364-366ms p50 for TS (vs. ~398-399ms at pool=8), ~359-362ms p50 for Python (vs.
~383-392ms at pool=8) -- no overlap between the two pool sizes' p50/p90 ranges in either
runner. **p99 is noisier and not a clean win either way between pool sizes**: TS pool=1's
two runs (549ms, 628ms) straddle pool=8's two runs (602ms, 634ms) rather than beating them
outright; Python pool=1's two runs (602ms, 660ms) land in essentially the same band as
pool=8's (658ms, 673ms) -- pool=1 vs. pool=8 is a wash on p99 for both runners, not a
regression either way at this sample size (2 runs per config isn't enough to resolve a
~50-70ms difference with confidence).

**Versus the *original* pre-DSN-fix single-connection p99s, the two runners diverge**: TS's
own pre-fix p99 was 736-742ms, so both pool=1 (549-628ms) and pool=8 (602-634ms) are a real
tail improvement for TS. Python's pre-fix p99 was 571ms -- lower than *either* post-fix
config (602-660ms at pool=1, 658-673ms at pool=8) -- so the DSN fix improved Python's p50/p90
substantially but did not improve its p99 versus that original baseline; if anything it's
mildly worse there, at either pool size. Not papered over: the DSN fix is still worth
shipping (p50/p90 dominate this workload's overall latency far more than a ~30-90ms p99
shift), but "the fixed DSN improved the tail for both runners" would overstate it -- it's
true for TS, not clearly true for Python.

**Given p50/p90 favor pool=1 clearly and p99 doesn't favor pool=8 at all, pool=1 is the
better choice on the evidence, with less code besides.** The connection-pool widening idea
that motivated this stage was itself reasoned from a CPU profile that turned out to be
confounded: that profile's "SQLite driver is syscall-heavy" conclusion (stage 1) was
measuring `journal_mode=delete` (the broken DSN's rollback-journal mode, which fsyncs a
separate journal file per transaction) mislabeled as WAL. Once WAL is genuinely active, most
of that syscall cost disappears on its own, and no amount of connection-pool tuning was ever
going to fix what was actually a journal-mode problem. Widening the pool to 8 does still work
correctly (0 errors, stage 3) -- it's not unsafe, just unnecessary complexity that doesn't
pay for itself on this write-heavy workload (SQLite's single-writer nature caps write
throughput at 1 regardless of pool size).

**Final, shipped configuration**: DSN fixed to `_pragma=` form, `MaxOpenConns(1)` kept exactly
as it always was (pool-widening code written, tested, and reverted twice in this
investigation -- see `internal/state/sqlite/sqlite.go`'s own doc comment for the full
four-stage account). The two new pool-oriented tests (`pool_smoke_test.go`) still pass and
are still meaningful at pool=1 (concurrent Go-level callers correctly queue for the single
connection without ever seeing `SQLITE_BUSY`), even though they were originally written to
validate the now-abandoned pool=8 configuration.

**Scoping the "fastest" claim precisely**: SQLite (fixed DSN, pool=1) is the fastest *default
single-runner state-backend* config measured for both runners -- ahead of Mongo+in-memory
(384ms), Postgres+in-memory (559ms), MySQL+in-memory (494ms), and each runner's own
Postgres+Redis number. It is *not* the fastest row in this entire report: "Postgres, 2 runner
replicas `--concurrency 100` each" (308ms p50) is faster, but that's a runner-scaling change
(horizontal replicas + concurrent dispatch), not a state-backend choice -- comparing it
directly to a single-runner SQLite number would conflate two different levers this report
treats separately (see findings 1c/1d and the Optimization opportunities section).

**Also worth restating plainly, per this report's own MySQL-vs-Redis precedent**: TS's
Postgres+Redis p50 (598ms, or 580ms on the reversed-order run) is close to Python's
Postgres+Redis post-fix p50 (910ms) only in the sense that both exist in the same rough
"couple hundred ms above the zero-dependency default" territory -- the actual gap (roughly
35% lower) is not small, and Python's row ran for 20s while every TS row here ran for 30s, so
none of these totals are directly comparable to each other, only the percentile latencies are.

Bottom line: what started as "is TS's SQLite-vs-Redis inversion real, and if so why" ended up
surfacing and fixing a real bug (`modernc.org/sqlite`'s DSN silently never applying WAL mode
or busy_timeout, for the project's entire history) that had nothing to do with the original
question -- and fixing *only* that bug, with zero connection-pool changes, makes SQLite the
fastest *default single-runner state-backend* config measured for both runners (~365ms TS,
~359ms Python p50, ahead of every other state-backend config including each runner's own
Postgres+Redis number -- but not ahead of the horizontal-runner-replicas row, a different
lever entirely, see the scoping note above). Two other ideas this investigation generated
along the way were tested and both turned out wrong, or at least oversold: the mutex
hypothesis (118ms/22ms of contention, negligible) and the connection-pool-widening fix
(works correctly, but adds complexity for a p50/p90 loss and an at-best-neutral p99 versus
the DSN fix alone). The original TS-vs-Python asymmetry this section opened with is largely
gone too: TS's SQLite p50 (~364-366ms) and Python's (~359-362ms) now sit within noise of
each other.

### 7. `--concurrency`'s value proposition, finally benchmarked as a sustained-load, realistic-latency workload instead of just a burst -- and it scales exactly as the theory predicts, identically for both runners

Finding 1d proved `--concurrency` works via a burst test (20 concurrent runs, ~14x speedup)
and explicitly flagged its own limitation: "a genuinely rigorous before/after benchmark for
the *intended* workload would use an agent with an artificial async sleep standing in for LLM
latency, not `echo_agent`... flagged as a good follow-up, not done here since it's a
benchmarking-methodology task." This closes that gap.

**New example agents** (`examples/llm_sim_agent/graph.py`, `examples/echo_agent_ts/
llmSimGraph.ts`): a single node that `await`s a configurable delay (`LLM_SIM_DELAY_MS`,
default 800ms -- a fast, typical single-turn LLM response, not a worst case) before
returning a canned response. Deliberately a sleep, not a real LLM API call: deterministic,
free, no external dependency, no rate limits/retries/non-determinism to pollute a benchmark
meant to be re-run repeatedly -- and a sleep *is* what a real LLM call looks like from the
runner's own perspective (an awaited I/O wait, not CPU work), so it exercises exactly the
mechanism `--concurrency` (asyncio.Task / un-awaited-promise fan-out) is supposed to help
with, without the noise a real API call would add.

**Methodology**: `bench/loadgen -concurrency 100 -duration 30s -wait-timeout 10s` (100
concurrent clients -- deliberately far more than any single concurrency setting below can
keep up with, so the *runner's own* concurrency setting is always the bottleneck being
measured, not client-side demand) against SQLite + in-memory (the current fastest default,
per section 6), varying only `--concurrency` on the runner. Same matrix, same parameters, for
both Python and TypeScript:

| Runner | `--concurrency` | Total | Errors | p50 | Effective throughput |
|---|---|---|---|---|---|
| Python | 1 | 312 | 300 (96.2%, all 10s timeouts) | 10.00s | ~0.4 req/s (12 successes) |
| Python | 10 | 470 | 0 | 8.07s | ~12.2 req/s |
| Python | 20 | 840 | 0 | 4.04s | ~24.3 req/s |
| TypeScript | 1 | 312 | 300 (96.2%, all 10s timeouts) | 10.00s | ~0.4 req/s (12 successes) |
| TypeScript | 10 | 470 | 0 | 8.06s | ~12.2 req/s |
| TypeScript | 20 | 840 | 0 | 4.03s | ~24.3 req/s |

**How "Effective throughput" is computed, precisely (two different formulas, by row):**
- **`--concurrency 1` row**: `successes / nominal 30s duration` = `12 / 30` ≈ 0.4 req/s. Not
  comparable to the other rows -- at `--concurrency 1`, 96% of requests never complete at all
  (they hit the 10s client-side timeout), so "throughput" here really means "rate of requests
  that got through the queue," not a steady-state completion rate.
- **`--concurrency 10`/`20` rows**: `Total / loadgen's own measured wall-clock runtime`, NOT
  `Total / 30`. `loadgen`'s `-duration` is a deadline each worker checks only *between* its
  own create-wait-read cycles, not a hard cutoff -- a worker already mid-cycle when the
  deadline passes keeps running until that cycle finishes, so the command's real wall-clock
  time exceeds the nominal 30s. Dividing by 30 instead would overstate throughput (e.g.
  470/30 ≈ 15.7 req/s -- a real but wrong number for this table). The reported ~12.2/~24.3
  match two independent cross-checks, which is why they're trusted as the real steady-state
  rate: Little's Law from p50 (`clients / p50` = `100 / 8.07s` ≈ 12.4, `100 / 4.04s` ≈ 24.8)
  and the simple theoretical ceiling below.

**Three things stand out:**

1. **At `--concurrency 1`, 100 concurrent clients against an 800ms-per-job runner is a
   genuine, severe failure mode, not a benign slowdown**: 96% of requests hit a 10s
   client-side timeout waiting in queue. This is the honest, sharp edge behind "default
   `--concurrency 1` deadlocks/starves under real concurrent demand" -- not a hypothetical,
   a directly reproduced one.
2. **Throughput scales linearly with `--concurrency`, matching the simple theoretical model
   almost exactly**: at `--concurrency N` with an 800ms per-job delay, the theoretical
   ceiling is `N / 0.8` req/s -- 12.5 req/s at N=10, 25 req/s at N=20. Measured: ~12.2 and
   ~24.3 req/s respectively, both within ~2-3% of the prediction. p50 latency also tracks the
   theory: doubling concurrency from 10 to 20 almost exactly halved p50 (8.07s -> 4.04s for
   Python, 8.06s -> 4.03s for TypeScript) -- the signature of a supply-constrained queue (not
   labeled "M/M/c" deliberately: service time here is a fixed 800ms sleep, not exponentially
   distributed, so the classic M/M/c model doesn't strictly apply -- but the qualitative
   behavior, throughput capped by server count and latency dominated by queueing delay ahead
   of a fixed service time, is the same fixed-service-time queueing signature regardless), not
   noise.
3. **Python and TypeScript are indistinguishable here** -- every p50 above matches within
   1-2% across the two runners at every concurrency level, and the `--concurrency 10`/`20`
   *totals* (470 and 840) match EXACTLY between the two, not just closely. That exactness is
   expected, not suspicious: both runs used the identical closed-loop `loadgen` parameters
   (100 clients, same 800ms delay, same 10s timeout) against a demand/supply ratio stable
   enough that the same number of complete-or-timeout cycles fit in a comparable wall-clock
   window for both -- including the exact same 96.15% error rate at `--concurrency 1`, the
   same kind of coincidence for the same reason. This is the expected, correct outcome for a
   purely I/O-wait-dominated workload: neither `asyncio`'s single-core GIL ceiling (finding
   1d) nor Node's single-threaded event loop ever becomes the bottleneck when every job spends
   800ms doing nothing but waiting -- the language/runtime underneath the `await` genuinely
   doesn't matter for this workload shape, only whether it can suspend efficiently while
   waiting, which both do.

**What this does and doesn't establish**: this proves `--concurrency`'s throughput-scaling
claim under sustained load, with a workload shape (an awaited delay) representative of real
LLM-call latency, for both runners identically. It does NOT reproduce finding 1d's own
"CPU-bound ceiling" result (a near-zero-compute agent like `echo_agent` pins one core and
`--concurrency` can't exceed that) -- this workload has essentially zero CPU work per job by
design, so it was never going to hit that ceiling; the two findings are complementary, not
contradictory. It also doesn't include a real LLM API call (network variance, provider rate
limits, token-count-dependent latency) -- a deliberate scope choice for a repeatable,
zero-cost, zero-flakiness benchmark, not an oversight; see "What this report does NOT cover."

## What this report does NOT cover

- Real agent workloads (LLM calls, tool calls) -- `echo_agent` isolates control-plane
  overhead deliberately; a real agent's own latency will dwarf these numbers in practice.
  Section 7's `llm_sim_agent`/`llm_sim_agent_ts` narrow this gap for the *shape* of a real
  agent's latency (an awaited delay) without a real API call's cost/variance/non-determinism
  -- still not a substitute for measuring an actual production agent's own LLM/tool-call mix.
- Multi-node control plane or multi-node Redis/Postgres -- single-host only.
- The CrewAI/LlamaIndex/plain-LangChain adapters' own overhead -- not benchmarked here,
  out of scope for "state backend + transport" comparison; worth a follow-up if any of
  those frameworks' own execution latency becomes a concern.
- Finding each system's actual breaking point (OOM, sustained failures) -- the
  resource-constrained comparison above stopped at 100 concurrent users with both systems
  still producing zero errors; pushing further until one fails would need a dedicated run.
- ~~The TypeScript runner's own concurrency ceiling under sustained CPU-bound load~~ --
  **done, see finding 1e**: real throughput gain to `--concurrency 20` then an early plateau,
  but -- unlike Python -- the runner's own CPU never shows sustained saturation (avg stays
  33-37%, only brief spikes to ~88%), and the control plane's CPU (118-144%, measured
  separately) is the better-supported explanation for both the single-runner plateau and a
  failed 2-replica test. Explicitly not a same-backend comparison to Python's 1d (Postgres
  there, SQLite + in-process here), so it isn't evidence about Node vs. Python as runtimes.
  Two things were caught and corrected before this made it into the report: a methodology
  mistake (reusing one growing SQLite file across concurrency stages, which briefly looked
  like `--concurrency` actively hurting TS throughput) via a fresh-database spot-check, and an
  analysis mistake (pattern-matching to Python's "runner CPU ceiling" story without checking
  whether the TS data actually supported it) caught on external review -- see 1e for both.
- **A dedicated write connection + separate read-only pool for SQLite** -- section 6's DSN
  fix alone (WAL mode + real busy_timeout, `MaxOpenConns` unchanged at 1) already made SQLite
  the fastest default single-runner state-backend config measured for both runners in this
  report; a connection-pool redesign would only matter for a read-heavy workload this report
  didn't test (this benchmark's create-thread-and-run cycle is write-heavy). Low priority
  given the current numbers, not attempted here.
