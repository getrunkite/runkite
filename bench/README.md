# Benchmarking Infrastructure

Internal profiling/load tools (pprof + `bench/loadgen`) for comparing backend
configs, plus a Locust-based load driver (`bench/locust/`) for
resource-constrained scenarios. See [`REPORT.md`](REPORT.md) for all results
and findings.

## pprof

Opt-in via `RUNKITE_PPROF=1` (never on by default -- these endpoints let
any caller dump goroutine/heap state and force real CPU load via
`/debug/pprof/profile`, both a real information-disclosure and DoS
surface):

```bash
RUNKITE_PPROF=1 ./runkite serve --config langgraph.json

go tool pprof http://localhost:2026/debug/pprof/heap
go tool pprof http://localhost:2026/debug/pprof/profile?seconds=30
curl "http://localhost:2026/debug/pprof/goroutine?debug=1"
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
SQLite/Postgres/MongoDB state backends and in-memory/Redis transports, and a
real correctness bug found and fixed along the way. Headline: the Redis
transport, not the state backend choice, is the dominant latency/memory cost
at high concurrency, and it's concurrency-dependent (contention), not a
fixed per-operation overhead.
