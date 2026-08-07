# Connectors

> Deep dive moved from the root README. For a 60-second overview see the [root README](../README.md).

Pre-authenticated OAuth/MCP sessions for runners. Configured in YAML, referenced from `langgraph.json`:

```yaml
# connectors/salesforce.yaml
auth:
  type: oauth2_client_credentials
  token_url: https://login.salesforce.com/services/oauth2/token
  client_id: ${SF_CLIENT_ID}
  client_secret: ${SF_CLIENT_SECRET}
mcp:
  url: https://salesforce-mcp.internal/sse
errors:
  INSUFFICIENT_ACCESS: "You don't have permission for this Salesforce object."
tools:
  allow: [soqlQuery, getObjectSchema]
  deny: [deleteRecord, bulkUpdate]
```

Supported auth types: `oauth2_client_credentials`, `oauth2_token_exchange`, `api_key`, `bearer`.

### Calling connectors from agent code (run-bound)

`POST /internal/connectors/{name}/session` and `POST /internal/connectors/{name}/mcp` require an **in-flight run**: runners must send `X-Runkite-Run-Id` + `X-Runkite-Generation`. The control plane derives tenant/agent from that assignment — a bare kind-token call without those headers gets `401` with `reason_code: run_binding_required`. See [Trust & governance](trust-governance.md).

Do **not** call those URLs with only a runner token from graph code. Use the SDK helpers (they read `configurable.run_id` / `configurable.generation` set by the runner for every job):

```python
# Python (inside a LangGraph node)
from runkite_runner.connectors import get_connector_session, proxy_connector_mcp

async def my_node(state, config):
    sess = await get_connector_session(config, "salesforce")
    # or: result = await proxy_connector_mcp(config, "salesforce", {"jsonrpc":"2.0", ...})
```

```typescript
// TypeScript
import { getConnectorSession, proxyConnectorMcp } from "runkite-runner";

const sess = await getConnectorSession(config, "salesforce");
```

Store/vector proxy clients already attach the same headers via `tenant_headers()` / `tenantHeaders()`; connectors need these helpers because agent-authored code makes the HTTP call.

### Tool allow/deny enforcement (MCP connectors)

The `tools.allow`/`tools.deny` filter above is a real, enforced gate, not just an advisory hint. `GetSession`'s `mcp.url` points at this control plane's own MCP proxy (`POST /internal/connectors/{name}/mcp`), not the connector's raw downstream MCP server -- so a `tools/call` request passes through here first, where a denied tool is rejected with a JSON-RPC error and **never reaches the downstream server at all**. `tools/list` responses are filtered against the same allow/deny list using the downstream server's own real tool list -- correct even for a deny-only filter (no allow list), which can't be represented in a static preview without knowing the full tool universe the downstream server exposes. An empty/missing tool name is always rejected too, independent of the filter shape (never a legitimate request regardless of what's allowed or denied). Every other JSON-RPC method (`initialize`, `resources/*`, etc.) is forwarded transparently. The connector's existing circuit breaker (guarding token fetches) also guards the proxied MCP call.

**What this does and doesn't cover, precisely**: neither `GetSession` nor `GET /internal/connectors[/{name}]` ever hand out the connector's raw downstream credentials or raw MCP URL when MCP is configured (found and fixed after two of those leaks slipped through review) -- so there's no *Runkite-issued* way for an agent to reach the real server directly instead of through the proxy. What this can't and doesn't claim to cover: if an agent author independently has their own valid credentials or URL for that same downstream service (hardcoded, from an environment variable, from anywhere outside this system), nothing here can stop code using them directly -- that's a fundamentally different threat this feature was never positioned to solve, since Runkite can only mediate access to things it hands out itself.

### Circuit breakers

Every OAuth2 connector (`oauth2_client_credentials`, `oauth2_token_exchange`) gets a per-connector circuit breaker guarding its actual token-fetch network call -- always on, with tunable thresholds:

```yaml
circuit_breaker:
  failure_threshold: 5    # consecutive failures before opening (default: 5)
  cooldown_seconds: 30    # how long to fail fast before a trial call (default: 30)
```

Standard closed/open/half-open state machine: closed passes calls through and counts consecutive failures; opening it makes every call fail fast (`503` + `Retry-After`, no network attempt) until the cooldown elapses; half-open lets exactly one trial call through and closes on success or reopens on failure. `api_key`/`bearer` auth never touches a breaker -- they make no network calls to break on. A cached, still-valid `client_credentials` token keeps being served even while the breaker is open on a since-broken refresh endpoint. Current state is visible at `GET /internal/connectors/{name}` (`circuit_breaker_state`).

## Custom Routes

User-defined HTTP endpoints mounted alongside the Agent Protocol API so a product can add feedback, favourites, OAuth callbacks, etc. **without forking the control plane**.

From the control plane's side, both modes below are the same mechanism: a reverse proxy to `custom_routes.url`, with the configured **mount** prefix stripped before forwarding (`/custom/webhook` → `{url}/webhook`, or `/sales-assistant/v1/x` → `{url}/v1/x`). Unreachable target returns `502`; unconfigured returns `404`.

```json
{
  "custom_routes": {
    "url": "http://127.0.0.1:8100",
    "mount": "/custom"
  },
  "custom_app": { "module": "./app.py:app", "host": "127.0.0.1", "port": 8100 }
}
```

`mount` defaults to `/custom`. Use a product prefix (e.g. `/sales-assistant`) when the frontend already calls bare product paths — no shim that rewrites `/v1/...` → `/custom/v1/...` is required. Mounts that collide with Agent Protocol reserved paths (`/threads`, `/admin`, `/internal`, …) are rejected at startup.

**Sidecar mode** (language-agnostic): run any HTTP service yourself, point `custom_routes.url` at it.

**In-runner mode** (Python or TypeScript): the runner hosts your ASGI / Node `RequestListener` beside its poll loop. `custom_app.module` uses the same `path:symbol` convention as `graphs`. WSGI needs an ASGI adapter. A slow handler can delay runner work — prefer sidecar for isolation.

### Identity headers (portable auth)

Custom routes go through the same client auth middleware as the Agent Protocol API. After auth succeeds, the proxy **strips** any inbound `X-Runkite-*` identity headers (anti-spoof) and **injects**:

| Header | Meaning |
|--------|---------|
| `X-Runkite-Identity` | Authenticated identity |
| `X-Runkite-Tenant-Id` | Resolved tenant (or `default`) |
| `X-Runkite-Permissions` | Comma-separated permissions |
| `X-Runkite-Display-Name` | Optional display name |
| `X-Runkite-User` | JSON object with the above fields |

`Authorization` is still forwarded if present. Custom apps should treat the `X-Runkite-*` headers as the trust boundary for "who is this?" when traffic arrives via the control plane. Python helpers: `runkite_runner.custom_auth.user_from_request` and `runkite_runner.custom_helpers.ControlPlaneClient` (store/thread/run HTTP calls with the caller's Bearer). See `examples/custom_routes_agent/` (`/ping`, `/whoami`).
