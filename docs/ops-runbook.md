# Ops runbook (Supported profile)

Thin operator guide for the **Supported** production shape: **Postgres + Redis**, client auth, runner tokens. Not a second architecture doc — deep detail lives in the linked pages.

## 1. Supported install

**What “Supported” means:** multi-replica control plane + at least one runner, durable Postgres, Redis for shared queue/broker/cancel, runner tokens + client `auth`. Compose soak (`make smoke-multi` / `make soak-multi`) is the correctness proof. Helm/kind packages the same shape; cluster installs stay **Compatible** until a named cloud soak exists.

### Compose (fastest real multi-CP)

```bash
cp .env.example .env   # set required secrets (see file comments)
docker compose -f docker-compose.multi.yml up -d --build
curl -sf http://localhost:2026/readyz
```

Zero-deps laptop demo (SQLite + in-memory, **not** Supported):

```bash
docker compose -f docker-compose.dev.yml up -d --build
```

Site walkthrough: [Try it](https://getrunkite.github.io/runkite/#try).

### Helm

Postgres and Redis are **external**. Prefer an existing Secret (keys from the chart README):

```bash
kubectl create secret generic runkite-creds \
  --from-literal=POSTGRES_DSN='postgres://…' \
  --from-literal=REDIS_URL='redis://…' \
  --from-literal=RUNNER_TOKEN_PYTHON_LANGGRAPH='…' \
  --from-literal=RUNNER_TENANTS_PYTHON_LANGGRAPH='default' \
  --from-literal=RUNNER_TOKEN='…' \
  --from-literal=RUNKITE_API_KEY='…'

helm upgrade --install runkite ./deploy/helm/runkite \
  -f deploy/helm/runkite/values.yaml \
  -f deploy/helm/runkite/values-supported.yaml \
  --set secrets.existingSecret=runkite-creds
```

Ops smokes (not a soak): `make kind-helm-all` (K0–K3) or the individual `kind-helm-smoke` / `kind-helm-rotate` / `kind-helm-reclaim` / `kind-helm-net` targets. Results: [k8s-kind-proof.md](k8s-kind-proof.md).

Details: [Deployment](deployment.md) · [Helm README](../deploy/helm/runkite/README.md#supported-profile).

### Health

| Probe | Use |
|-------|-----|
| `/livez` | Process up (liveness) |
| `/readyz` | Dependencies reachable — **LB / readinessProbe** |
| `/health` | Legacy alias; prefer `/readyz` for routing |

`serve` refuses to boot without durable store + shared transport + runner tokens + client auth + `RUNNER_TENANTS_*` for each tokenized kind (use `default` for single-tenant) unless `RUNKITE_ALLOW_INSECURE_SERVE=1`. `runkite dev` stays open for local work.

## 2. Reclaim (crash-loop ceiling)

When a runner dies mid-job, the control plane reclaims the lease and may re-enqueue. Generation fencing already exists on Redis, NATS, and Kafka.

**Ceiling:** `reclaim.max_retries` in `langgraph.json` (default **3**). When the next reclaim would exceed the ceiling, the run is marked `error`, the thread is released, and a terminal status/hook fires — it is **not** re-enqueued forever.

```json
"reclaim": { "max_retries": 3 }
```

- Absent section / field → default `3`
- Explicit `0` → unlimited (legacy)
- Env override: `RUNKITE_RECLAIM_MAX_RETRIES` (loaded at process start)

This is distinct from **cancel** (live runner) and **kill switches** (admission + drain). See [Limitations](limitations.md).

## 3. Kill and cancel

| Action | When | How |
|--------|------|-----|
| **Cancel one run** | Live run should stop | Agent Protocol / Admin cancel → runner sees cancel stream |
| **Kill / pause** | Stop new work for a tenant or agent; optionally drain in-flight | Admin UI `/admin/kill` or `POST /admin-api/kill-switches` (SQL backends). Unless `pause_only`, cancels non-terminal runs in scope |
| **Break-glass** | Time-bounded **policy** bypass (max 24h) | Does **not** bypass kill, `agents:<id>:run`, or rate/admission limits |

Mongo: kill/break-glass Admin APIs return `501` — governance durability is SQL-only. Details: [Admin UI](admin.md) · [Trust & governance](trust-governance.md).

## 4. Secrets patterns

| Secret | Where | Notes |
|--------|-------|--------|
| `POSTGRES_DSN` / `REDIS_URL` | CP env or K8s Secret | Required for Supported; never bake into images |
| `RUNNER_TOKEN_<KIND>` | CP env | Kind encoding: `PYTHON_LANGGRAPH` → `python-langgraph`. Value may be a **comma-separated allowlist** for rotation |
| `RUNNER_TENANTS_<KIND>` | CP env | Required with client auth + runner tokens; tenant allow-list for unbound `/internal/*` (use `default` for single-tenant) |
| `RUNNER_TOKEN` | Runner env | Single token the runner presents; must match one allowlisted value for its kind |
| Client `auth` (API key / JWT) | `langgraph.json` + env substitution | `serve` admission requires this in production (`RUNKITE_API_KEY` in the stock Helm config) |
| Connector OAuth / API keys | Connector YAML via `${ENV}` or `auth.secret_ref` | Prefer env/file/Vault refs; MCP session tokens are short-lived and run-bound |
| Vault (`vault:` secret_ref) | CP env: `VAULT_ADDR`, `VAULT_TOKEN` or `VAULT_TOKEN_FILE`, optional `VAULT_NAMESPACE` / `VAULT_ALLOWED_PREFIX` | KV v2 paths under allowlisted prefix (default `secret/data/runkite/`). Site: [Secrets](../site/support/secrets.html) |

**Rotation (runners):** set CP allowlist to `old,new` → restart CP → roll runners to `new` → drop `old` → restart CP. Allowlists load at process start. Full auth surface: [Auth](auth.md).

**Do not:** put runner tokens in Redis/NATS/Kafka connection strings; embed long-lived connector secrets in the run assignment payload (helpers mint at use time).

## 5. Checkpoint mode reminder

| Mode | Runner needs | Survives restart |
|------|----------------|------------------|
| **Direct** | `POSTGRES_DSN` (same DB as CP) | Yes — LangGraph Postgres tables |
| **Proxy** | `RUNKITE_HTTP_URL`, no runner DB creds | Yes — opaque blobs on CP (`/internal/checkpoints/*`) |
| **Memory** | neither | No |

Retention `checkpoints_keep_last` prunes Agent Protocol history **and** opaque proxy blobs — never LangGraph direct-mode tables. Threat model: [Runner Protocol §6.4](../runner-protocol/PROTOCOL.md#64-threat-model-proxy-checkpoints).

## See also

- [Deployment](deployment.md) — Docker, Helm, graceful shutdown
- [Configuration](configuration.md) — `langgraph.json` knobs
- [Auth](auth.md) — client + runner credentials, TLS
- [Limitations](limitations.md) — Supported vs Compatible honesty
