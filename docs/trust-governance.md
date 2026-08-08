# Trust & governance

Runkite is a control plane. Security that fits is **governance of the plane**: who can run which agent, with which tools/secrets, under which budgets, with kill-switches and an audit trail — self-hosted.

This page documents the trust boundary and what is (and is not) enforced today. It deliberately under-promises.

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

Runners send `X-Runkite-Run-Id` and `X-Runkite-Generation` (the fencing generation from `GetJob`). The control plane looks up the in-flight `RunAssignment` and derives `tenant_id` / agent (`graph_id`) / principal (`user`) from that record. Runner-supplied `X-Runkite-Tenant-Id` is **not** trusted on bound paths.

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
| Production baseline | Durable store + shared transport + `RUNNER_TOKEN_*` + client `auth` (admission refuses bare `serve`) |
| Multi-tenant | Client auth + run-binding on proxy paths; set `RUNNER_TENANTS_<KIND>` so unbound `/internal/*` routes cannot claim arbitrary tenants |

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
| OTel `policy.decide` + optional SIEM `policy_decision` | Yes | Yes (not store-backed) |
| `tool_auth` RunEvents | Yes | Yes |

**Security marketing rule:** claim audit search, Admin grant overlays, and connector HITL for SQL state backends. Mongo is fine for agent runs and connector policy *enforcement* when grants/webhook live in `langgraph.json`, but it is **not** an equal governance proof. Production HA still prefers Postgres + Redis (direct-mode checkpoints, Helm defaults); MySQL/SQLite share the governance tables.

## Connector policy

When `langgraph.json` has a `policy` section (including empty `"policy": {}`), connector access is **fail-closed**:

| Stage | Gate |
|-------|------|
| `POST .../session` | Grant must match `(tenant_id, agent_id, connector)` |
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

**Admin grant CRUD:** `langgraph.json` `policy.grants` are immutable deployment defaults. Durable overlays live in SQL `policy_grants` and are mutated via `/admin-api/policy-grants` without redeploy — DB rows win on the same `(tenant_id, agent_id, connector)`. A second create for the same key with a different `id` returns `409`. An empty `"policy": {}` section still enables the engine (fail-closed) so deployments can be 100% Admin-managed. Each replica reloads overlays on its own Admin write (no cross-replica watch yet). Mongo returns `501`.

**Connector HITL (`pending`):** a sync policy webhook may return `"effect": "pending"` for MCP `tools/call` (e.g. `delete_repo`). The control plane does **not** pause the LangGraph run via interrupt/resume — it refuses the tool call with JSON-RPC `-32000`, `reason_code: policy_pending`, and an `action_id`, persists a SQL `pending_actions` row, and emits `tool_auth` with `effect: pending`. Operators list/approve/deny via `/admin-api/pending-actions`. Approve re-evaluates policy (hard deny refuses); otherwise the row becomes `approved` (one-shot capability). The agent's next matching `tools/call` for the same `(run_id, generation, connector, tool)` consumes that capability before Decide and is forwarded once; further calls go pending/deny again. Pending decisions are never cached. Without a SQL store (Mongo), pending cannot be persisted — the MCP path fails closed to deny (same `reason_code`, no `action_id`).

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
