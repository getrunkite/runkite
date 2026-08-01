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

Runners call `POST /internal/connectors/{name}/session` to get ready-to-use credentials without implementing auth flows themselves.

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

User-defined HTTP endpoints mounted at `/custom/*` alongside the Agent Protocol API. From the control plane's side, both modes below are the exact same mechanism -- a reverse proxy to `custom_routes.url` in `langgraph.json`, with the `/custom` prefix stripped before forwarding (`/custom/webhook` reaches the target as `/webhook`). Unreachable target returns `502`; unconfigured returns `404`.

**Sidecar mode** (language-agnostic): run any HTTP service yourself, point `custom_routes.url` at it. Works for non-Python routes, or routes that need independent scaling/deployment from the runner.

**In-runner mode** (Python, simplest DX): the runner SDK hosts your ASGI app itself, via `uvicorn`, as a background task alongside its own gRPC poll loop -- "similar to dropping a file in your project":

```json
{
  "custom_routes": { "url": "http://127.0.0.1:8100" },
  "custom_app": { "module": "./app.py:app", "host": "127.0.0.1", "port": 8100 }
}
```

`custom_app.module` uses the same `path:symbol` convention as `graphs`. Works with FastAPI, Starlette, or any ASGI-callable -- `uvicorn` is the only SDK dependency, not a specific framework. WSGI frameworks (e.g. Flask) need an ASGI adapter (e.g. `a2wsgi.WSGIMiddleware`) first, since `uvicorn` only serves ASGI. Because it shares the runner's own process and event loop, a slow custom-route handler can, in principle, delay the runner's own async work -- use sidecar mode instead if a route needs isolation or independent scaling. See `examples/custom_routes_agent/` for a working FastAPI example.
