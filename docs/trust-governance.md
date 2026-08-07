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

Static grants evaluate in-process. An optional sync webhook (HMAC, same shape as `preflight_hooks`) can add an external deny. Policy denials use JSON-RPC `-32000` with `data.reason_code` on MCP, or HTTP `403` + `reason_code` on session. **Denials do not trip the connector circuit breaker.**

Every decision is written to `audit_events` on **Postgres (Supported)** when `policy.audit` is true (default). Compatible backends skip durable audit in this release — do not claim equal audit proofs there. Admin search/HITL for `pending` are Phase 2.

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
