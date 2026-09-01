# Trust & governance

Runkite is a control plane. Security that fits is **governance of the plane**: who can run which agent, with which connectors/tools/secrets, with kill-switches, break-glass, and an audit trail — self-hosted. (Not a FinOps product; spend dashboards are out of scope here.)

This page documents the trust boundary and what is (and is not) enforced today. It deliberately under-promises.

**Announce bar (verify, don’t rebuild):** run-binding, connector policy deny + durable audit, Admin audit search, and connector HITL (pending → approve → one-shot retry) are exercised by `make smoke-governance` on Postgres. That smoke is the laptop proof for those exits — not Mongo parity, not in-graph tool sandboxing, not a paid cloud soak, and not FinOps dashboards.

**Admin UI (SQL):** `/admin/grants`, `/admin/mandatory-hitl`, `/admin/pending`, `/admin/kill`, `/admin/break-glass`, `/admin/audit` — see [Admin UI](admin.md#governance-pages-sql-backends).

## Trust boundary

```
Clients (SDK / HTTP / Admin UI)
        │  Agent Protocol + client auth
        ▼
┌─────────────────────────────┐
│  Runkite control plane      │  ← enforcement surface
│  auth · tenants · runs      │
│  connectors · MCP proxy     │
│  store/vector proxy         │
└───────────┬─────────────────┘
            │ Runner Protocol (gRPC) + /internal/* HTTP
            ▼
        Runners (framework code, LLM calls, native tools)
```

**What the plane always sees:** client identity (when auth is configured), run create/cancel/stream, job dispatch/fencing, connector session minting, MCP proxy forwards, proxy-mode store/vector calls.

**What the plane does not see:** raw LLM prompts/completions (unless you add your own proxy), framework-native tool bodies that bypass connectors, direct-mode Postgres/pgvector access from the runner process.

## Run-binding (current)

Proxy paths that touch tenant data or credentials require an **active run**:

| Path | Binding |
|------|---------|
| `POST /internal/connectors/{name}/session` | Required |
| `POST /internal/connectors/{name}/mcp` | Required |
| `/internal/store/*` | Required |
| `/internal/vectors/*` | Required |
| Other `/internal/*` (schema, status, A2A, connector metadata) | Runner token only |

Runners send `X-Runkite-Run-Id` and `X-Runkite-Generation` (the fencing generation from `GetJob`). The control plane looks up the in-flight `RunAssignment` and derives `tenant_id` / agent (`graph_id`) / principal (`user`) from that record. Runner-supplied `X-Runkite-Tenant-Id` is **not** trusted on bound paths. Generation `0` on either side bypasses fencing (pre-fencing runner compat, same as Ack/Renew) — production runners must send the real generation from `GetJob`.

Stable `reason_code` values on denials include:

| `reason_code` | Meaning |
|---------------|---------|
| `runner_credentials_invalid` | Missing/wrong runner kind token |
| `runner_tenant_denied` | Tenant not on `RUNNER_TENANTS_<KIND>` |
| `run_binding_required` | Missing/invalid run id or generation |
| `run_not_inflight` | No active assignment for that run id |
| `run_generation_mismatch` | Stale/superseded generation |
| `run_binding_lookup_failed` | Queue lookup error |

## Tiers

| Posture | Notes |
|---------|--------|
| Local (`runkite dev`) | Runner auth off; fine for single-user laptops |
| Production baseline | Durable store + shared transport + `RUNNER_TOKEN_*` (optional comma allowlist per kind for fleets/rotation) + client `auth` (admission refuses bare `serve`) |
| Multi-tenant | Client auth + run-binding on proxy paths; `serve` requires `RUNNER_TENANTS_<KIND>` for every tokenized kind so unbound `/internal/*` routes cannot claim arbitrary tenants |

## Explicit non-goals (today)

- Prompt-injection / jailbreak classifiers
- PII scanning or output moderation
- Sandboxing arbitrary tool code inside the runner
- Transparent egress MITM for every BYO `httpx`/`fetch` call
- Replacing your IdP
- Equal security / governance proofs on Mongo or Experimental backends (see [Governance durability](#governance-durability-sql-backends))

## Governance durability (SQL backends)

Connector **enforcement** (static grants, sync webhook, fail-closed Decide) runs on every state backend when `policy` is configured. The **durable governance trail** needs SQL:

| Capability | Postgres / MySQL / SQLite | Mongo |
|------------|---------------------------|-------|
| In-process Decide (grants + webhook) | Yes | Yes |
| Durable `audit_events` + Admin search / Audit UI | Yes | No (writes skipped; Admin `501`) |
| Durable `policy_grants` Admin CRUD overlays | Yes | No (`501`) |
| Durable `pending_actions` HITL approve/deny | Yes | No (pending fails closed to deny; Admin `501`) |
| Durable `kill_switches` (tenant/agent pause + drain) | Yes | No (`501`) |
| Durable `break_glass_windows` (time-bounded policy bypass) | Yes | No (`501`) |
| Durable `mandatory_hitl_rules` Admin overlays | Yes | No (`501`) |
| Run admission Gate (`agents:<id>:run` + kill + optional `run.create` Decide) | Yes | Authz works; kill / break-glass durability SQL-only |
| OTel `policy.decide` + optional SIEM `policy_decision` | Yes | Yes (not store-backed) |
| `tool_auth` RunEvents | Yes | Yes |

**Security marketing rule:** claim audit search, Admin grant overlays, connector HITL, kill switches, break-glass, and mandatory HITL for SQL state backends. Mongo is fine for agent runs and connector policy *enforcement* when grants/webhook live in `langgraph.json`, but it is **not** an equal governance proof. Production HA still prefers Postgres + Redis (direct-mode checkpoints; Compose soak); Helm packages that backend profile but cluster install stays Compatible until a cloud soak. MySQL/SQLite share the governance tables.

## Connector policy

When `langgraph.json` has a `policy` section (including empty `"policy": {}`), connector access is **fail-closed**:

| Stage | Gate |
|-------|------|
| `POST .../session` | Grant must match `(tenant_id, agent_id, connector)`; MCP responses include a short-lived `session_token` |
| `POST .../mcp` | `X-Runkite-Connector-Session` must match that token for `(run_id, generation, connector)` (15m absolute TTL) |
| `POST .../mcp` `tools/call` | Same grant + optional per-grant tool allow/deny |

Static grants evaluate in-process. An optional sync webhook (HMAC, same shape as `preflight_hooks`) can add an external deny — first deny wins, and a static allow does **not** bypass the webhook. Policy denials use JSON-RPC `-32000` with `data.reason_code` on MCP, or HTTP `403` + `reason_code` on session. **Denials do not trip the connector circuit breaker.**

When grants and a webhook are both configured, grants act as a first-pass allowlist: a request with no matching grant is denied immediately and the webhook is never called. `default_effect: allow` is only honored when there is **no** webhook (grants-only setups). With a webhook and no grants, the webhook decides; `default_effect` is the fallback if nothing else returns.

Every decision is written to `audit_events` on SQL backends when `policy.audit` is true (default). Mongo skips the write — see [Governance durability](#governance-durability-sql-backends).

**Admin search:** `GET /admin-api/audit-events` lists decisions newest-first with cursor paging (`X-Next-Cursor`) and filters (`tenant_id`, `decision`, `action`, `run_id`, `agent_id`, `connector`, `tool`, `since`/`until` as RFC3339). Example — denials for tenant `acme` in the last 7 days:

```
GET /admin-api/audit-events?tenant_id=acme&decision=deny&since=<RFC3339>
```

Returns `501` on Mongo. The Admin UI **Audit** page (`/admin/audit`) lists the same data with tenant/decision/connector filters.

**Observability:** every Decide adds an OTel span event `policy.decide` (effect, reason_code, run/agent/connector/tool) on the active HTTP span when OTLP is configured. Optional async SIEM export:

```json
"policy": {
  "grants": [ ... ],
  "siem": { "url": "https://siem.example/hooks/runkite", "secret": "..." }
}
```

That registers a `policy_decision` webhook sink (HMAC + retries + dead letters) — it does **not** block connector/MCP paths.

**Run stream DX:** when `policy.run_events` is true (default), session and MCP `tools/call` denials / pending HITL also publish a `tool_auth` RunEvent on the run's event broker so Agent Protocol SSE/WS clients see the decision in-stream (`effect`, `reason_code`, connector, tool, optional `action_id`). Event IDs use `{run_id}_tool_auth_{hex}` (not the runner `{run_id}_evt_{n}` namespace).

**Admin grant CRUD:** `langgraph.json` `policy.grants` are immutable deployment defaults. Durable overlays live in SQL `policy_grants` and are mutated via `/admin-api/policy-grants` without redeploy — DB rows win on the same `(tenant_id, agent_id, connector)`. A second create for the same key with a different `id` returns `409`. An empty `"policy": {}` section still enables the engine (fail-closed) so deployments can be 100% Admin-managed. The writing replica hot-reloads immediately; every other control-plane replica converges via a 15s SQL fingerprint poll (same cadence as cron). Mongo returns `501`.

**Connector HITL (`pending`) — Admin-only approve, agent must retry:** a sync policy webhook may return `"effect": "pending"` for MCP `tools/call` (e.g. `delete_repo`). The control plane does **not** pause the LangGraph run via Agent Protocol interrupt/resume, and there is **no** client-facing approve API — operators list/approve/deny only via `/admin-api/pending-actions` (Admin UI `/admin/pending`). The tool call is refused with JSON-RPC `-32000`, `reason_code: policy_pending`, and an `action_id`; a SQL `pending_actions` row is persisted and `tool_auth` is emitted with `effect: pending`. Approve re-evaluates policy (hard deny refuses); otherwise the row becomes `approved` (one-shot capability). The **agent must issue another** matching `tools/call` for the same `(run_id, generation, connector, tool)` after approval — that retry consumes the capability before Decide and is forwarded once; further calls go pending/deny again. Frameworks that swallow tool errors without retrying will look “stuck” until they retry. Pending decisions are never cached. Without a SQL store (Mongo), pending cannot be persisted — the MCP path fails closed to deny (same `reason_code`, no `action_id`).

**Mandatory HITL (`policy.mandatory_hitl` + Admin overlays):** rules that force matching `tool.call` **allows** to `pending` (`reason_code: mandatory_hitl`) even when static grants or the sync webhook would allow — defense-in-depth against a misconfigured/compromised PDP. Hard deny still wins (never elevated to pending). Empty `agent_id` = whole tenant; empty `tools` = every tool on that connector. Same `pending_actions` / Admin approve / one-shot retry path as webhook pending. `langgraph.json` `policy.mandatory_hitl` are immutable deployment defaults; durable SQL overlays (`mandatory_hitl_rules`) via `/admin-api/mandatory-hitl` win on the same `(tenant_id, agent_id, connector)` — writer hot-reloads; siblings converge via the 15s overlay poll. Break-glass skips `Decide` entirely, so it also skips this override; the break-glass audit Attrs note `mandatory_hitl_bypassed` when a rule would have matched. Mongo → `501`.

**Run admission:** every `createRunCtx` path shares one `hooks.Gate` pipeline (same as `preflight_hooks`). The admission Gate checks, in order: (1) SQL kill/pause switch for tenant or tenant+agent, (2) agent-scoped authz `agents:<id>:run` (route-level write only on run-create paths; blanket `write`/`admin` still allow any agent; empty permissions stay unrestricted), (3) when policy is enabled, `Decide` at stage `run.create` (skips connector grants; optional sync webhook can still deny; decisions are audited). Deny → HTTP 403. Kill activation (`POST /admin-api/kill-switches`, Admin UI `/admin/kill`) refuses new creates immediately on every replica (SQL read) and, unless `pause_only`, drains **all** pending/running runs in scope on the writing replica (paginated SearchRuns until empty; interrupted/success/error/timeout are already terminal and are not re-cancelled).

**Break-glass windows:** a separate SQL table (`break_glass_windows`) for time-bounded **policy** bypass — not kill columns. While a window is active for the tenant (or tenant+agent), admission and connector gates skip `policy.Decide` for `run.create` / `connector.session` / `tool.call` after a durable audit row is written (`reason_code: break_glass`). Fail-closed if the audit write fails. Hard max duration **24h** on mint. Does **not** bypass kill, `agents:<id>:run`, `admission_limits`, or rate limits. Admin: `GET|POST /admin-api/break-glass`, `GET|DELETE /admin-api/break-glass/{id}`, UI `/admin/break-glass`. Mongo → `501`.

Optional **`admission_limits`** (occupancy/quota, not request-rate): per-tenant / per-agent `max_concurrent` (pending+running) and `max_daily` (UTC day), enforced atomically with run INSERT under a scope lock. Exceed → HTTP 429. See [Configuration](configuration.md).

### Bring your own PDP (sync `policy.webhook`)

Runkite does not ship a proprietary policy product. You point `policy.webhook.url` at **your** decision service (OPA, Cedar, internal ABAC, or a 50-line script). The control plane sends `policy.decide` JSON (HMAC optional), fail-closes on errors, and enforces `allow` / `deny` / `pending` on connector paths.

Reference example (deny `delete_repo`, pending `transfer_funds`, HMAC + `self_check.py`): [`examples/policy_webhook/`](../examples/policy_webhook/). If that PDP works against the contract, any service that speaks the same envelope can replace it. This governs the **control plane** (connectors / admission), not arbitrary in-graph tools that never call a Runkite connector.

Example:

```json
"policy": {
  "default_effect": "deny",
  "grants": [
    {
      "id": "acme-sales-sf-read",
      "tenant_id": "acme",
      "agent_id": "sales-assistant",
      "connector": "salesforce",
      "tools": { "allow": ["query", "getRecord"], "deny": ["updateRecord"] }
    }
  ]
}
```

Absent / empty `policy` preserves V1 open access after runner auth + run-binding.
