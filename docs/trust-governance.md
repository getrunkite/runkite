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
- Equal security proofs on Experimental state/transport backends

## Connector policy (Phase 1)

When `langgraph.json` has a non-empty `policy` section (grants and/or webhook), connector access is **fail-closed**:

| Stage | Gate |
|-------|------|
| `POST .../session` | Grant must match `(tenant_id, agent_id, connector)` |
| `POST .../mcp` `tools/call` | Same grant + optional per-grant tool allow/deny |

Static grants evaluate in-process. An optional sync webhook (HMAC, same shape as `preflight_hooks`) can add an external deny — first deny wins, and a static allow does **not** bypass the webhook. Policy denials use JSON-RPC `-32000` with `data.reason_code` on MCP, or HTTP `403` + `reason_code` on session. **Denials do not trip the connector circuit breaker.**

When grants and a webhook are both configured, grants act as a first-pass allowlist: a request with no matching grant is denied immediately and the webhook is never called. `default_effect: allow` is only honored when there is **no** webhook (grants-only setups). With a webhook and no grants, the webhook decides; `default_effect` is the fallback if nothing else returns.

Every decision is written to `audit_events` on **Postgres (Supported)** when `policy.audit` is true (default). Compatible backends skip durable audit in this release — do not claim equal audit proofs there.

**Admin search (Phase 2):** `GET /admin-api/audit-events` lists decisions newest-first with cursor paging (`X-Next-Cursor`) and filters (`tenant_id`, `decision`, `action`, `run_id`, `agent_id`, `connector`, `tool`, `since`/`until` as RFC3339). Example — denials for tenant `acme` in the last 7 days:

```
GET /admin-api/audit-events?tenant_id=acme&decision=deny&since=<RFC3339>
```

Returns `501` when the state backend is not Postgres. The Admin UI **Audit** page (`/admin/audit`) lists the same data with tenant/decision/connector filters.

**Observability:** every Decide adds an OTel span event `policy.decide` (effect, reason_code, run/agent/connector/tool) on the active HTTP span when OTLP is configured. Optional async SIEM export:

```json
"policy": {
  "grants": [ ... ],
  "siem": { "url": "https://siem.example/hooks/runkite", "secret": "..." }
}
```

That registers a `policy_decision` webhook sink (HMAC + retries + dead letters) — it does **not** block connector/MCP paths. HITL `pending` and Compatible-backend audit tables follow in the rest of Phase 2.

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
