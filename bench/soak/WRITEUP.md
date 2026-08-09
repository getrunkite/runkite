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
