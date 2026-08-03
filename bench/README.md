# Benchmarking Infrastructure

Internal profiling/load tools (pprof + `bench/loadgen`) for comparing backend
configs, plus a Locust-based load driver (`bench/locust/`) for
resource-constrained scenarios. See [`REPORT.md`](REPORT.md) for all results
and findings.

## pprof

Opt-in via `RUNKITE_PPROF=1` **and** `RUNKITE_PPROF_TOKEN=<secret>` (never
on by default -- these endpoints let
any caller dump goroutine/heap state and force real CPU load via
`/debug/pprof/profile`, both a real information-disclosure and DoS
surface):

```bash
RUNKITE_PPROF=1 RUNKITE_PPROF_TOKEN=dev ./runkite serve --config langgraph.json
# then: curl -H "Authorization: Bearer dev" http://localhost:2026/debug/pprof/

go tool pprof http://localhost:2026/debug/pprof/heap
go tool pprof http://localhost:2026/debug/pprof/profile?seconds=30
curl "http://localhost:2026/debug/pprof/goroutine?debug=1"
```

`RUNKITE_PPROF=1` also sets `runtime.SetMutexProfileFraction(1)` and
`runtime.SetBlockProfileRate(1)` -- both default to 0 (no samples at all)
unless set explicitly, so `/debug/pprof/mutex` and `/debug/pprof/block`
would otherwise always come back empty regardless of real contention.
Added specifically to test (and, in `bench/REPORT.md` section 6, disprove)
a Go-mutex-contention hypothesis for the TS runner's SQLite-vs-Redis
finding -- useful for any future "is this actually lock contention"
question, not a one-off.

```bash
go tool pprof http://localhost:2026/debug/pprof/mutex
go tool pprof http://localhost:2026/debug/pprof/block
```

## Load generator

`bench/loadgen` drives concurrent create-thread-and-run-and-wait cycles
against a running control plane, reporting p50/p90/p99 latency, error rate,
and (with `-pid`) RSS growth over the run -- the same "does memory plateau
or grow unbounded" signal used to manually verify this project's core loop
during development, now a reusable tool instead of a one-off script.

```bash
go run ./bench/loadgen \
  -url http://localhost:2026 \
  -agent-id echo_agent \
  -concurrency 150 \
  -duration 60s \
  -pid $(pgrep -f 'runkite serve')
```

Flags: `-url`, `-agent-id`, `-concurrency`, `-duration`, `-pid` (0 to skip
RSS sampling), `-wait-timeout` (per-run HTTP client timeout).

## Locust load driver

`bench/locust/locustfile.py` drives the same create-thread-and-run-and-wait cycle as
`bench/loadgen`, via Locust, useful for its distributed/web-UI load-generation features:

```bash
locust -f bench/locust/locustfile.py --host http://localhost:2026 \
  --headless --users 50 --spawn-rate 10 --run-time 30s --csv /tmp/runkite
```

For resource-constrained testing, note that the plain `Dockerfile` at the repo root is
control-plane only -- a fair memory/CPU-limited comparison against any deployment that
runs everything in one container needs a combined image (control plane + Python runner in
one container, sharing one resource limit) rather than measuring the control plane alone.

## Findings

See [`REPORT.md`](REPORT.md) for full results: internal scale tests across
SQLite/Postgres/MongoDB/MySQL state backends and in-memory/Redis transports,
plus the TypeScript runner (section 6), and several real correctness/leak
bugs found and fixed along the way. Headline: investigating a surprising
early result (the TypeScript runner was slower than Python on the
zero-dependency SQLite default but on par with Python on Postgres+Redis,
unlike every other combination measured) led to finding and fixing a real,
previously-undetected bug: `modernc.org/sqlite`'s DSN had silently never
applied WAL mode or its busy_timeout for the project's entire history (it
used mattn/go-sqlite3 query-param forms the actual driver ignores). A
mutex-contention hypothesis for the original asymmetry was tested with real
profiling and disproven along the way (negligible contention either way).
Fixing the DSN alone -- no connection-pool change at all -- made SQLite the
**fastest default single-runner state-backend config** measured for both
runners: Python's SQLite p50 dropped from 463ms to ~360ms, TypeScript's
from ~650ms to ~365ms, both ahead of every other state backend including
each runner's own Postgres+Redis (the only faster row in the whole report
is a horizontal-runner-replicas config, a different lever entirely). A
connection-pool-widening idea (reasoned from a CPU profile that turned out
to be measuring the broken DSN's slow rollback-journal mode, not an
inherent driver cost) was tried twice and reverted twice: once against the
still-broken DSN (~99.8% error rate), once against the fixed DSN (worked,
0 errors, but measurably slower on p50/p90 than just fixing the DSN and
leaving the pool at 1 -- p99 was roughly a wash between the two pool
sizes, not a clean loss either way). See REPORT section 6 for the full
four-stage story.

Section 7 finally benchmarks `--concurrency` as a sustained-load,
realistic-latency workload (a new `llm_sim_agent`/`llm_sim_agent_ts`
example with a configurable simulated-LLM delay, not a real API call --
deterministic and free) instead of just the earlier burst test. Result:
throughput scales linearly with `--concurrency`, within ~2-3% of the
simple queueing-theory prediction (`N / delay` req/s) at every level --
and Python and TypeScript match each other even tighter (every p50
within 1-2%, and `--concurrency 10`/`20` totals identical) -- so
`--concurrency`'s scaling behavior is effectively runner-independent.
`--concurrency 1` against real concurrent demand is a severe,
directly-reproduced failure mode (96% of requests time out), not a
hypothetical edge case.
