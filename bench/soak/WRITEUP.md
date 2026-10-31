# Multi-CP soak writeup

Laptop-scale evidence for Runkite's **Supported** profile: Postgres + Redis,
multi-replica control plane, exercised via `docker-compose.multi.yml` +
`docker-compose.soak.yml`. Helm packages the same shape
([`values-supported.yaml`](../../deploy/helm/runkite/values-supported.yaml));
this writeup is the Compose proof path, not a Kubernetes soak.

## Commands

From repo root (requires Docker, `RUNNER_TOKEN` in the environment or `.env`):

| Goal | Command |
|------|---------|
| Quick health (minutes) | `make smoke-multi` |
| 10-minute rehearsal | `make soak-multi-short` (or `SOAK_DURATION=600 make soak-multi`) |
| Announce laptop bar (30 min) | `make multi-down` then `SOAK_DURATION=1800 make soak-multi` (default) |

**Graded / announce runs:** tear down first (`make multi-down`). The harness
leaves the stack up; a second soak on the same Postgres accumulates
`total_runs`, so end-state absolutes are **not** “what this window produced.”

Watch live: http://127.0.0.1:2026/admin/

Artifacts land under `bench/soak/out/<timestamp>/` (gitignored), including
auto-generated `REPORT.md`. Longer local runs (`SOAK_DURATION=21600`, etc.)
are optional; they are **not** required for this writeup's pass bar.

## Checkpoint and sink setup

The bundled multi-plane runners now use HTTP proxy checkpoints and store access:
their `POSTGRES_DSN` is empty and `RUNKITE_HTTP_URL` points at the plane. The planes
retain Postgres. This supports default `cron:<name>` threads under the native
checkpoint identity guard. Existing test volumes with native history need
explicit reconciliation before switching modes; new proxy state is not a history
migration. Older direct-mode timing results are not an identical workload.

`SOAK_SINK_PORT` selects both the host sink listener and fixture webhook/preflight
URLs; default `9099` is unchanged. The Python and JavaScript loaders also accept
optional `SOAK_API_KEY` for authenticated test overlays. The checked-in stack
keeps its documented local open-client mode unless an overlay configures auth.

## Bounded recovery checks

`run.sh` exercises steady create/wait traffic. Its completion message is not an
automatic grade of every pass criterion below. The separate
[`recovery.py`](recovery.py) harness asserts cache, cancel, process-loss reclaim
and full retained raw SSE replay through different replicas. It returns nonzero
on an assertion failure and restores the services it stops.

Use a disposable project named `runkite-ha-<name>`. The optional
[`docker-compose.recovery.yml`](../../docker-compose.recovery.yml) overlay
removes database debug ports and publishes distinct fixed loopback ports for
the planes and load balancer. It requires Compose support for `!reset` and
`!override` (validated with Compose 5.6). Check that ports `23261`, `23262`,
`23263`, `23266` and `25251` are free, or copy the overlay and choose free fixed
ports. Automatically allocated host port `0` can change after stop/start, which
invalidates recovery URLs.

Set `RUNNER_TOKEN` as required by the multi-plane fixture. Start the sink in one
terminal, retaining its JSONL file for per-run hook assertions:

```bash
mkdir -p bench/soak/out/recovery-local
python3 bench/soak/webhook_sink.py 9099 bench/soak/out/recovery-local/webhooks.jsonl
```

In another terminal, start the disposable project and run its checks. Include
the same overlays and explicit project name in every Compose command:

```bash
SOAK_SINK_PORT=9099 docker compose -p runkite-ha-local \
  -f docker-compose.multi.yml \
  -f docker-compose.soak.yml \
  -f docker-compose.recovery.yml up -d --build

python3 bench/soak/recovery.py \
  --url http://127.0.0.1:23266 \
  --replica-url http://127.0.0.1:23261 \
  --replica-url http://127.0.0.1:23262 \
  --replica-url http://127.0.0.1:23263 \
  --project runkite-ha-local \
  --compose-file docker-compose.multi.yml \
  --compose-file docker-compose.soak.yml \
  --compose-file docker-compose.recovery.yml \
  --webhook-log bench/soak/out/recovery-local/webhooks.jsonl \
  --output bench/soak/out/recovery-local
```

For an authenticated overlay, set `SOAK_API_KEY`. Optional
`SOAK_ADMIN_API_KEY` supplies separate Admin read access. The harness does not
record either key. The `metered_agent` fixture emits explicitly synthetic 20/4
token metadata so cache replay is tested against a positive usage ledger. It
does not invoke a provider or establish vendor billing accuracy.

Read `recovery-report.json` and its case evidence before quoting a result.
Full raw SSE replay does not establish managed UI reconnect or incremental cursor
behavior. Postgres and Redis remain alive during these process-loss cases.
The [engineering record](../../docs/internal/ha-recovery-validation.md) explains
the invariants, fixture correction and evidence limits.

## Pass criteria (announce bar = 30 min)

Evaluate against start/end artifacts (`overview-start.json`,
`overview-end.json`, `REPORT.md`, `samples.log`, compose logs, webhooks):

1. **`/readyz` green** for the duration (stack stays up; smoke/soak refuse a
   colliding host process on `:2026`).
2. **Soak agents registered** — Admin overview / connectors snapshot shows
   the soak agent set from `examples/soak_multi` (not an empty fleet).
3. **Error rate (delta, not end absolute)** — from
   `overview-start.json` → `overview-end.json` `runs_by_status`:
   `Δerror / (Δsuccess + Δerror) < 0.01`, where
   `Δstatus = end[status] - start[status]` (missing key = 0). If
   `Δsuccess + Δerror < 50`, require **zero** `Δerror` instead.
   Cross-check: webhook `run_start` / `run_complete` counts in
   `webhooks.jsonl` should nearly match `Δsuccess` (small skew from
   in-flight runs at snapshot time is OK). **Do not** quote end
   `total_runs` as this window’s throughput if the DB was pre-populated.
4. **No CP OOM / restart storm** — `samples.log` docker stats and
   `compose-tail.log` show no control-plane OOM kills or crash-loop restarts.
5. **`REPORT.md` produced** with the filtered metrics section present
   (`runkite_runs_total`, memory samples, etc.).

Paste into an announce note: duration, `OUT_DIR`, **delta** success/error
(and optionally webhook pairs), not the cumulative end `total_runs`, plus a
short metrics excerpt from `REPORT.md`.

## Non-claims

- Not an EKS / kind / Helm-on-cluster soak.
- Not a multi-day wall-clock soak.
- Not a real-LLM / paid-model pass.
- Not proof of Compatible backends (SQLite, MySQL, Mongo, NATS, Kafka-alone).
