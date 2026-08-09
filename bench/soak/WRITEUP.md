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
| Announce laptop bar (30 min) | `SOAK_DURATION=1800 make soak-multi` (default) |

Watch live: http://127.0.0.1:2026/admin/

Artifacts land under `bench/soak/out/<timestamp>/` (gitignored), including
auto-generated `REPORT.md`. Longer local runs (`SOAK_DURATION=21600`, etc.)
are optional; they are **not** required for this writeup's pass bar.

## Pass criteria (announce bar = 30 min)

Evaluate against end-of-run artifacts (`overview-end.json`, `REPORT.md`,
`samples.log`, compose logs):

1. **`/readyz` green** for the duration (stack stays up; smoke/soak refuse a
   colliding host process on `:2026`).
2. **Soak agents registered** — Admin overview / connectors snapshot shows
   the soak agent set from `examples/soak_multi` (not an empty fleet).
3. **Error rate** — from end overview run counts:
   `error / (success + error) < 0.01`. If total terminal runs are tiny
   (`success + error < 50`), require **zero** errors instead.
4. **No CP OOM / restart storm** — `samples.log` docker stats and
   `compose-tail.log` show no control-plane OOM kills or crash-loop restarts.
5. **`REPORT.md` produced** with the filtered metrics section present
   (`runkite_runs_total`, memory samples, etc.).

Paste into an announce note: duration, `OUT_DIR`, error rate, and a short
excerpt of the metrics filter from `REPORT.md`.

## Non-claims

- Not an EKS / kind / Helm-on-cluster soak.
- Not a multi-day wall-clock soak.
- Not a real-LLM / paid-model pass.
- Not proof of Compatible backends (SQLite, MySQL, Mongo, NATS, Kafka-alone).
