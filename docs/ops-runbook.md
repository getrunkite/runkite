# Ops runbook (Supported profile)

Thin operator guide for the **Supported** production shape: **Postgres + Redis**, client auth, runner tokens. Not a second architecture doc — deep detail lives in the linked pages.

## Connector action evidence upgrade

**Unreleased source after v0.6.1:** rebuild the control plane; published v0.6.1
binaries do not implement `policy.action_evidence`. Take a verified backup and
stop control-plane writers before `runkite db upgrade`. This adds Postgres/SQLite
schema 17 and MySQL schema 18, including action history and immutable run
incarnations. On MySQL, upgrade also includes the earlier identity-collation
requirements below. Existing rows are not reconstructed into tool histories.

Managed connector approvals now carry the original stored run's immutable
incarnation, even when action capture is off. Historical managed approvals
without this binding remain audit records but cannot be approved or consumed.
Resubmit the operation with its exact arguments and obtain fresh review; do not
backfill authority onto old rows. Deleting and recreating the same run ID and
generation does not transfer an existing approval to the replacement run.
Standalone decision approvals remain in their separate, expiring namespace and
do not require a managed run.

The SQLite upgrade also normalizes retained checkpoint timestamps to fixed-width
UTC text without changing their instants or payloads. This repairs latest/history
ordering for whole-second or trimmed fractional timestamps. Normalization uses
bounded pages in one transaction; invalid historical timestamps stop the upgrade
for explicit repair instead of assigning a new time or dropping history. Allow
time for this scan and keep writers stopped until the upgrade succeeds.

Leave capture off during upgrade. Deploy matching binaries and set both
`policy.audit_required: true` and `policy.action_evidence: true` consistently on
every serving replica before relying on write-before-dispatch evidence. An old
or flag-off replica still uses the prior path. Verify SQL availability, a safe
fixture tool response in Admin Connector actions, approval behavior and a denied
request with no protected side effect before production traffic.

Before dispatch, evidence failure blocks the tool. A confirmed SQL rollback leaves
its approval unconsumed; a lost commit acknowledgement can leave a committed
intent and consumed approval even when this handler refuses to send. Investigate
the action and approval records rather than assuming rollback. After intent,
502/503 or disconnect can mean the tool executed.
Investigate using the action ID and target's own records before retrying. The
30-second reconciler only marks expired intent unknown; it never replays tools.
A late observation can remain attached to the original attempt after reclaim.

Turning new capture off does not remove history or stop reconciliation of retained
intents. Run pruning also retains it. `retention.connector_actions_max_age` is
optional and off by default; it prunes only old blocked, not-dispatched and
response-observed records. Pending and unknown history remains, so plan capacity
and backup/archive procedures. Do not downgrade/drop evidence tables to silence
an unresolved operation. Preserve a verified backup and explicitly review any
pending/uncertain effects before rollback. See the
[manual](../site/support/connector-actions.html) and
[engineering rationale](internal/connector-action-evidence.md).

Completion checkpoints now use bounded opaque IDs on SQL and Mongo. Existing
IDs remain valid; callers must use returned IDs rather than construct one from a
run ID. New SQL completion history follows its locked parent by at least one
microsecond; Mongo uses one millisecond. History timestamps can lead wall time
when a parent was future-dated. Upgrade does not reconstruct old commit order or
change historical payloads. Before resuming sensitive retained conversations,
compare the selected checkpoint with current thread state and repair any known
mismatch explicitly.

## Tenant review upgrade

This slice is unreleased source after v0.6.1 and adds no SQL migration. Complete
the preceding security migrations first when upgrading from a released binary.
Deploy matching new binaries to every serving replica before issuing
`governance:read` / `governance:approve` grants. Old JWT permission filtering does
not understand these strings. Do not rely on mixed-version/default-open behavior
for reviewer isolation. Use `strict_permissions: true` and explicit primary
identity/tenant mapping, then grant the minimum role required. Keep a working
global Admin credential for bootstrap and lockout recovery.

Review uses its own `runkite_review_session` cookie and `rk:review:sess:` Redis
namespace. Admin sessions and keys retain their existing behavior. Review role
snapshots expire absolutely after at most 15 minutes regardless of activity;
IdP grant changes do not instantly revoke an existing cookie. Logout confirms
store deletion or reports an unconfirmed failure. Independent Admin and Review
sessions have independent logout. A custom session store must implement the
optional isolated Review-session creation capability; unsupported stores refuse
Review login rather than reuse Admin authority.

Verify two differently scoped synthetic users before granting operational access:
readers cannot mutate, approvers cannot reach another tenant, and both remain
outside global Admin/conversation routes. No-provider/default-open Review must
remain denied. The [operator guide](../site/support/tenant-review.html) covers
login, fixed expiry, 403/404/409/501 and recovery. The
[engineering note](internal/tenant-scoped-review.md) records scope, session
purpose, SQL predicates and regression rationale.

The replica upgrade is an operator rollout step, not a missing permission
implementation. Configured API-key/JWT/webhook providers already default to
strict permissions unless explicitly overridden, and the Helm example sets it
to true. A new binary cannot teach an older replica the new role vocabulary.

For a disposable local qualification, run `make test-review-ha`. It starts two
real strict-auth control planes sharing its own Postgres/Redis pair, then checks
cross-replica sessions, tenant isolation, approval/dispatch races and process-loss
reconciliation. Use `RUNKITE_TEST_REVIEW_REPETITIONS=3` for bounded repetition.
The target uses synthetic credentials and MCP effects, makes no model-provider
calls, prints its evidence directory and cleans up its own infrastructure.
Use an isolated CI worker or firewall-protected workstation: database mappings
are loopback-only, but the CLI's HTTP/gRPC listeners follow their existing
all-interface binding. This is a finite qualification, not a long cloud soak.

## Hardening upgrade

**MySQL operators:** state schema version 17 changes identity/security columns to
`utf8mb4_0900_bin` (NO PAD). Take a verified backup and stop all writers, including
old control-plane replicas, before running `runkite db upgrade`. Allow time for
ALTER operations; startup also runs migrations, so avoid rolling this upgrade.
The server must support that MySQL 8 collation. Display/search fields retain
their existing rules. That identity-collation migration does not change Postgres,
SQLite or Mongo; the later action-evidence migration is described above.

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

## Follow-up identity and completion upgrade

New cache hits do not bill the original result's usage again. Historical usage
ledger rows are retained; audit earlier cache-hit accounting before correcting
estimated spend. No vendor invoice or historical refund is changed automatically.

Upgrade the plane before runners that use `X-Runkite-Run-Id-B64` or
`X-Runkite-Tenant-Id-B64` for exact Unicode/legacy identities. Safe printable ASCII
uses the existing headers. Do not strip identities in a proxy. Tenant allowlists
match exact identities; configuration-list whitespace is not part of a tenant ID.
New run IDs with controls or outer whitespace return 400. Existing stored IDs are
not renamed, and encoded bindings preserve their exact value.

**Review direct Postgres LangGraph before rollout: every named tenant must use
HTTP proxy checkpoints.** The runner refuses non-default tenants before factory,
state access or execution. A delimiter-safe new named identity can still alias
an old default-tenant native row, and there is no ownership attestation to prove
that row belongs to it. Default direct thread IDs without `:` keep their existing
history; default colon thread IDs remain refused.

Fresh shared memory retains the delimiter guard: default thread IDs containing
`:`, named tenant IDs containing `:`, and tenant IDs with outer whitespace or
ASCII controls fail before factory/state access. **Default cron threads are
`cron:<name>` and need HTTP proxy mode too.** Unset runner `POSTGRES_DSN` and set
`RUNKITE_HTTP_URL`. Proxy supports all tenants, but switching modes imports no
history. Inspect and explicitly resolve prior native history before continuing;
a blank proxy transcript is not a successful migration. No owner marker,
automatic import or silent blank/memory fallback is provided. Managed AI SDK,
Microsoft, OpenAI Agents, Pydantic AI and Google ADK already use opaque HTTP
state and are unaffected by this direct LangGraph restriction.

Cached results now use atomic admission/completion on built-in stores. A failed
write returns an error instead of exposing partial success. Cache replay does not
bill original tokens again, create a usage hold or refresh source TTL; newly
created runs still count toward daily run quotas. Custom Go stores without the
optional `state.CachedRunCreator` use ordinary dispatch on a cache lookup.

**Compatible NATS operators:** run IDs must use nonempty ASCII letters/digits,
`-`, `_`, `/`, `=` or single internal dots, without leading/trailing dots or
empty dot segments. Wildcards, whitespace, colons and Unicode are rejected by
queue, stream and cancel operations. Run creation rejects unsupported IDs with
400 before thread/run/cache mutation. Valid keys/subjects are unchanged; there is
no namespace migration. Legacy unsupported metadata may remain readable, but its
stream/cancel operations fail closed. An unsupported queued assignment is retained
with a delayed negative acknowledgement and explicit error. Inspect the stored
run and any external effects before repairing or resubmitting it; do not assume
it completed or delete work merely to silence the error. These transport limits
do not apply to the Supported Postgres/Redis pair.

NATS runner kinds must also have distinct derived durable names. A dot becomes
an underscore, so `a.b` and `a_b` both request `jobs-a_b`. The plane now refuses a
conflicting consumer filter rather than retargeting an existing fleet. Inspect
its configuration and pending work, then rename the conflicting kind consistently
in agent config, token/tenant environment and runner flags. Do not delete or
retarget the durable without reconciling queued work. Existing names and identical
same-kind replica configuration are unchanged; consumer/history migration is not
automatic. The bridge independently rejects an actual dequeued assignment for a
different authenticated kind before attempt advancement and returns its lease.

In local in-process mode, a full ready buffer or cancelled return/reclaim call
now reports an error while retaining the lease and original generation. Once
capacity is available, normal retry/reclaim can recover it; partial reclaim counts
include only successful ready handoffs. This is not process-restart durability
or an HA upgrade. Use the Supported Postgres/Redis profile for that deployment.

Parent ownership is checked inside child-write transactions, and Mongo thread
removal cascades atomically within the deleted tenant. These changes need no
schema migration and do not repair historical malformed links. The separate
MySQL version 17 maintenance instructions above still apply when upgrading from
an older schema. See [the follow-up design record](internal/atomic-results-and-identities.md).

## Retained recovery work

Redis reclaim now keeps caller-selected run IDs separate from poison metadata
and retains the original lease and generation if a ready-list write fails. The
structured result exists only between the Lua call and Go parser; no Redis key
format or stored-record migration is needed. If logs report an incorrectly typed
queue key, inspect and reconcile that key's contents before repair. Do not delete
an in-flight lease merely to clear a reclaim error.

Finish upgrading all control-plane replicas before relying on literal poison-ID
handling. Old colon parsing could select a different prefix ID and generation,
including another tenant's run. If custom colon IDs were used, review affected
historical runs and dead-letter records explicitly. This patch does not rewrite
prior outcomes or reconstruct lost history.

A cancel request can fail after its interruption committed if usage, hold,
broker or hook finalization is unavailable. Read the durable run before treating
the request as ineffective. The plane keeps its lease so normal reclaim can
retry finalization after the backend recovers, and attempts a runner notification.
This does not undo an external side effect or guarantee immediate cooperative
stop. See [the recovery design record](internal/ha-recovery-validation.md#keep-retry-identity-and-leases-intact-on-failures).

The opt-in `run_timeout` sweep now commits timeout and thread release atomically
through the existing completion transaction. It retains prior conversation state,
records only available matching-attempt usage and releases a busy thread as
`idle` when no other run is active.
Finalization failures keep their lease for recovery; a timeout error in logs can
therefore coexist with an already stored `timeout` result. Restore the affected
backend and verify the durable result, usage hold and thread state. Do not start
another external operation merely to clear a bookkeeping error. Historical busy
threads or old timeout outcomes are not rewritten automatically.

## 1. Supported install

**What “Supported” means:** multi-replica control plane + at least one runner, durable Postgres, Redis for shared queue/broker/cancel, runner tokens + client `auth`. Compose soak (`make smoke-multi` / `make soak-multi`) is the correctness proof. Helm/kind packages the same shape; cluster installs stay **Compatible** until a named cloud soak exists.

### Compose (fastest real multi-CP)

```bash
cp .env.example .env   # set required secrets (see file comments)
docker compose -f docker-compose.multi.yml up -d --build
curl -sf http://localhost:2026/readyz
```

The bundled multi-plane runners now use HTTP proxy checkpoints, with runner
`POSTGRES_DSN` empty and `RUNKITE_HTTP_URL` set. The plane still persists to
Postgres. This lets default cron threads (`cron:<name>`) satisfy the native
checkpoint identity guard. Proxy mode does not import existing native history;
resolve that history before changing an existing deployment.

`make smoke-multi` checks readiness and a successful run. The load harness and
its 30-minute announce criteria are separate from the v0.6.0 bounded
[recovery harness](../bench/soak/WRITEUP.md#bounded-recovery-checks).
Run the recovery harness only against an explicitly named disposable local
project. It interrupts runner and plane processes and checks cache completion,
cross-replica cancel, reclaim and retained raw SSE. Read the case report before
claiming recovery passed. The [validation record](internal/ha-recovery-validation.md)
labels the earlier short load rehearsal separately from later scripted
retained-backend outage probes. The probes pause and restore the same containers;
they do not establish a cloud deployment, lost Redis state recovery or universal
SDK outage behavior.

Zero-deps laptop demo (SQLite + in-memory, **not** Supported):

```bash
docker compose -f docker-compose.dev.yml up -d --build
```

Static walkthrough: [Demo guide](https://getrunkite.com/support/try.html).
The [public Admin route](https://getrunkite.com/admin/) currently shows demo
availability rather than a running backend. The Compose instance above serves
its own Admin at `http://localhost:2026/admin/`. See
[website and backend hosting](site-hosting.md) for the public-domain boundary.

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
For failed opaque state writes in the v0.6.0 adapter update, expect withheld
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

## Authority hardening upgrade

Build this source with Go 1.26.11 or a newer fixed toolchain. The module and
container builder now require 1.26.11 for the net/http header-race fix
[GO-2026-6859](https://pkg.go.dev/vuln/GO-2026-6859). Existing binaries and images
must be rebuilt; a source or documentation update does not patch a running
process. This toolchain patch does not require a state migration.

This change has no database migration. Before rolling out the plane, verify that custom A2A callers send their current parent run and generation binding. Deploy compatible runner helpers with the plane release; requests with missing binding or elevated `on_behalf_of` are rejected. Existing client-facing run creation is unchanged.

Enable `policy.audit_required: true` only after checking durable SQL audit availability. An audit outage intentionally blocks admission and connector calls, even under break-glass. Restore audit storage rather than disabling the gate as an automatic recovery action. A request rejected before dispatch is safe to resubmit; an earlier downstream timeout still requires reconciliation. The normal mode remains best-effort unless this option is selected.

OAuth endpoint errors retain status and recognized standard OAuth codes, but omit arbitrary provider bodies. Custom connector error mappings that matched descriptive body text must use the supported safe codes. Token files are reread on resolution; existing OAuth access tokens can remain cached until expiry. Rotating a client secret does not revoke those tokens. Vault lookup failure does not silently enable a stale-secret fallback.

See [design and regression coverage](internal/action-authority-hardening.md), [A2A upgrade guidance](a2a.md#upgrade-and-troubleshooting), and [required audit configuration](configuration.md#required-policy-audit).
