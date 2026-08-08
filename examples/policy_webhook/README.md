# Bring-your-own policy webhook (reference PDP)

Proves Runkite’s **control-plane** sync policy contract: you run any decision service that speaks this HTTP shape; the control plane enforces allow / deny / pending on connector session + MCP `tools/call` (and optional `run.create`).

This is **not** agent-framework / in-graph tool sandboxing. Tools that never go through a Runkite connector are outside this example’s scope.

## What it proves

| Path | PDP response | Control plane behavior |
|------|----------------|-------------------------|
| `tool.call` + `delete_repo` | `effect: deny` | MCP/session refuse; audit row |
| `tool.call` + `transfer_funds` | `effect: pending` | HITL queue (SQL); Admin approve → one-shot retry |
| Everything else | `effect: allow` | Proceed |

If this reference PDP works, **your** OPA/Cedar/internal ABAC service can sit behind the same JSON + HMAC.

## Quick check (no control plane)

```bash
cd examples/policy_webhook
SECRET=dev-policy-secret python3 pdp.py &
SECRET=dev-policy-secret python3 self_check.py   # unit + live HTTP
```

## Wire into Runkite

Add to the first `langgraph.json` the control plane loads (webhook-only = webhook decides; if you also set `grants`, a matching grant is required before the webhook is called):

```json
"policy": {
  "fail_closed": true,
  "webhook": {
    "url": "http://127.0.0.1:8099/decide",
    "secret": "dev-policy-secret",
    "timeout_ms": 2000
  }
}
```

1. Start the PDP: `SECRET=dev-policy-secret python3 pdp.py`
2. Start Runkite with a SQL state backend (needed for `pending` + audit).
3. Drive a connector MCP `tools/call` for `delete_repo` → deny; `transfer_funds` → pending in Admin **Pending** (`/admin/pending`).
4. Optional: point `policy.siem.url` at your log sink to receive async `policy_decision` events.

## Contract (copy into your real PDP)

**Request** (POST, `Content-Type: application/json`):

```json
{
  "type": "policy.decide",
  "stage": "tool.call",
  "tenant_id": "acme",
  "agent_id": "ops",
  "run_id": "...",
  "generation": 1,
  "connector": "github",
  "tool": "delete_repo",
  "timestamp": "...",
  "data": { "connector": "github", "tool": "delete_repo", "identity": "..." }
}
```

**HMAC** (when `secret` is set): header `X-Runkite-Signature: sha256=<hex(HMAC-SHA256(secret, raw body))>`.

**Response** (2xx + JSON): `{"effect":"allow"|"deny"|"pending","reason":"...","reason_code":"...","rule_id":"..."}`  
Legacy `{"allow": true|false}` also works. Non-2xx / bad JSON / timeout → fail-closed deny.

## Customize

| Env | Meaning |
|-----|---------|
| `PORT` | Listen port (default `8099`) |
| `SECRET` | Shared HMAC (must match `policy.webhook.secret`) |
| `DENY_TOOLS` | Comma list (default `delete_repo`) |
| `PENDING_TOOLS` | Comma list (default `transfer_funds`) |

Replace `decide()` in `pdp.py` with a call to OPA (`POST /v1/data/...`) or your service — keep the HTTP envelope identical.

See [Trust & governance](../../docs/trust-governance.md).
