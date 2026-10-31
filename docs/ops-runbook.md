# Ops runbook (Supported profile)

Thin operator guide for the **Supported** production shape: **Postgres + Redis**, client auth, runner tokens. Not a second architecture doc — deep detail lives in the linked pages.

## Hardening upgrade (unreleased)

**MySQL operators:** state schema version 17 changes identity/security columns to
`utf8mb4_0900_bin` (NO PAD). Take a verified backup and stop all writers, including
old control-plane replicas, before running `runkite db upgrade`. Allow time for
ALTER operations; startup also runs migrations, so avoid rolling this upgrade.
The server must support that MySQL 8 collation. Display/search fields retain
their existing rules. Postgres, SQLite and Mongo schemas are unaffected.

Preflight rejects aliased foreign-key IDs, mismatched child/parent tenants and
incompatible custom constraints before DDL. Investigate and repair explicitly;
do not disable foreign-key checks. DDL autocommits. If an ALTER fails, keep
writers stopped, fix its cause and rerun upgrade. The migration resumes without
stamping version 17 until constraints are restored. Confirm upgrade success
before reopening traffic. Version 17 downgrade refuses unsafe identity merging;
rollback requires the verified backup. Data overwritten under the old insensitive
collation cannot be reconstructed by this migration. See the
[MySQL engineering rationale](internal/runtime-hardening.md#mysql-identity-comparison-must-preserve-tenant-boundaries).

Upgrade the plane before runners and use canonical API endpoint URLs without
redirects. New helpers renew capabilities only after the plane explicitly returns
`connector_session_invalid`. A new helper against an old unmarked 401 fails safely.

Drain older control-plane replicas before relying on v2 approval binding. Old
argument digests were produced by a lossy parser and are intentionally not reused.
Resubmit pending operations with their exact original arguments and obtain fresh
review. Old pending/approved rows remain audit records; Admin rejects new approval
of them, and app action polling returns `rejected` with `approval_binding_legacy`.
Do not replay an uncertain external write merely because its old approval is no
longer usable. There is no approval migration or repair of historical precision.

Expect HTTP 502 `connector_upstream_unauthorized` when the upstream returns 401.
The tool may have executed; automatic capability renewal is inappropriate. HTTP
503 `admin_session_revoke_failed` means logout is unconfirmed. Restore session
storage and retry; clearing local UI state is not server revocation.

Completion checkpoints now contain merged thread state. Run outputs remain per-run
results, and old checkpoint rows are not rewritten. New exact `.` and `..` resource
IDs are rejected with 400. Custom Go queue/session implementations must adopt the
new generation-bearing `Nack` and error-returning `SessionStore.Delete` signatures.
The public Runner Protocol wire format is unchanged.

See [the engineering record](internal/runtime-hardening.md) for the rationale,
validation boundaries and retained product limitations.

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

Site walkthrough: [Try it](https://getrunkite.com/support/try.html).

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

**Staleness (`max_age`):** how long an in-flight job may go without a successful Heartbeat/Renew before reclaim re-enqueues it. Default **6s** — sized for ~2s runner heartbeat cadence and the 2s reclaim ticker. Lower → false reclaims (duplicate LLM work); higher → slower crash recovery.

```json
"reclaim": { "max_age": "6s", "max_retries": 3 }
```

- Absent `max_age` → default `6s`
- Env override: `RUNKITE_RECLAIM_MAX_AGE` (Go duration string, loaded at process start)

**Ceiling (`max_retries`):** when another generation would exceed the ceiling,
the queue retains the current assignment and stops re-enqueueing it. The reaper
first tries to recover a terminal event from that exact attempt; otherwise it
attempts an error completion. It acknowledges the retained generation only after
durable completion and finalization succeed. Database, broker or finalization
failures leave it for the next tick. The poison record names the rejected next
generation, not a newly dispatched attempt.

- Absent section / field → default `3`
- Explicit `0` → unlimited (legacy)
- Env override: `RUNKITE_RECLAIM_MAX_RETRIES` (loaded at process start)

This is distinct from **cancel** (live runner) and **kill switches** (admission + drain). See [Limitations](limitations.md).

**Upgrade custom runners together with the plane.** Every run-specific RPC and
bound HTTP call must echo the generation assigned by `GetJob`. Generation zero
matches only an actual legacy-zero assignment. Kind tokens cannot dequeue,
watch or act on another kind's run. A stale/zero event is not promoted into the
current attempt. A delivery failure must leave completion recoverable instead
of reporting a replacement agent error.

**Reconcile durable completion.** Raw SSE is advisory during the check/publish
race. Read the stored run or use `/wait` before presenting success as final.
For failed opaque state writes in the unreleased adapter update, expect withheld
completion and reclaim rather than a successful run with lost history. Inspect
state availability before retrying external writes; they still need idempotency.

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
| `POSTGRES_DSN` / `REDIS_URL` | CP env or K8s Secret | Required for Supported; never bake into images. Redis also holds connector sessions (`rk:conn:sess:*`), Admin sessions (`rk:admin:sess:*`), rate-limit buckets (`rk:rl:*`), and opt-in payload shrink (`rk:payload:*`) |
| `RUNNER_TOKEN_<KIND>` | CP env | Kind encoding: `PYTHON_LANGGRAPH` → `python-langgraph`. Value may be a **comma-separated allowlist** for rotation |
| `RUNNER_TENANTS_<KIND>` | CP env | Required with client auth + runner tokens; tenant allow-list for unbound `/internal/*` (use `default` for single-tenant) |
| `RUNNER_TOKEN` | Runner env | Single token the runner presents; must match one allowlisted value for its kind |
| Client `auth` (API key / JWT) | `langgraph.json` + env substitution | `serve` admission requires this in production (`RUNKITE_API_KEY` in the stock Helm config) |
| Connector OAuth / API keys | Connector YAML via `${ENV}` or `auth.secret_ref` | Prefer env/file/Vault refs; MCP session tokens are short-lived and run-bound |
| Vault (`vault:` secret_ref) | CP env: `VAULT_ADDR`, `VAULT_TOKEN` or `VAULT_TOKEN_FILE`, optional `VAULT_NAMESPACE` / `VAULT_ALLOWED_PREFIX` | KV v2 (`…/data/…`) or KV v1; paths cleaned then prefix-checked (no `..` escapes). Default prefix `secret/data/runkite/`. Site: [Secrets](../site/support/secrets.html) |
| Postgres RLS (opt-in) | CP env: `RUNKITE_POSTGRES_RLS=true` | FORCE RLS + `app.tenant_id` / `app.is_system` + `SET ROLE runkite_app` (DSN needs `CREATEROLE` + `GRANT … SET TRUE`). Flag off + restart clears sticky FORCE. Defense-in-depth only; app `WHERE tenant_id` remains required. Site: [Multi-tenant](../site/support/multi-tenant.html) |

**Rotation (runners):** set CP allowlist to `old,new` → restart CP → roll runners to `new` → drop `old` → restart CP. Allowlists load at process start. Full auth surface: [Auth](auth.md).

**Do not:** put runner tokens in Redis/NATS/Kafka connection strings; embed long-lived connector secrets in the run assignment payload (helpers mint at use time).

## 5. Checkpoint mode reminder

The modes below describe LangGraph. The managed AI SDK backend (v0.5.8+) uses
the run-bound HTTP opaque API, even when the plane stores data in Postgres. It
does not use LangGraph's direct-mode tables or support native approval resume.
Successful-turn receipts support recovery after commit; a new turn after cancel
uses only previously committed history.

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
