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
  url: https://salesforce-mcp.internal/mcp
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

The managed AI SDK factory (published from v0.5.8) receives the same binding as `context.runkiteConfig`. Pass it to these TypeScript helpers inside a server tool's `execute` function. Registering a local AI SDK tool does not automatically route that tool through a connector or apply connector grants.

Apps using only `/governance/decide` have no in-flight assignment and cannot use these internal proxy routes with that client credential. They keep execution and pending continuation in the app. See the [SDK integration guide](../site/support/sdk-integration.html).

Store/vector proxy clients already attach the same headers via `tenant_headers()` / `tenantHeaders()`; connectors need these helpers because agent-authored code makes the HTTP call.

### Policy grants (fail-closed when configured)

With a `policy` section in `langgraph.json`, including `"policy": {}`, session mint and MCP `tools/call` require a matching grant for `(tenant_id, agent_id, connector)` derived from the in-flight run. See [Trust & governance](trust-governance.md). Optional `policy.predicates` additionally deny or hold a `tools/call` based on argument values before the upstream call. These checks cover connector MCP calls, not every local tool. MCP connectors stay **proxy-only**: the control plane never returns the raw downstream MCP URL or credentials when MCP is configured, so their tool grants and argument predicates remain on the execution path.

### Deadlines, cancellation and retry safety (v0.6.0)

Session helpers default to one 30-second operation deadline; MCP helpers default
to 60 seconds. The budget includes mint, request, response body and optional
renewal. Python `timeout` is in seconds; TypeScript `timeoutMs` is in milliseconds.
Pass the managed AI SDK factory's `context.signal` through the TypeScript helper's
`signal` option. Python task cancellation closes the in-flight client. Increase an
explicit deadline for a legitimately slow operation; zero/nonfinite values do not
create an unbounded call.

Only HTTP 401 `connector_session_invalid` permits one automatic capability
renewal. An arbitrary 401, timeout or transport failure is not proof that the
upstream tool had no effects. The plane maps upstream 401 to HTTP 502
`connector_upstream_unauthorized`, and helpers do not replay it. Upgrade the plane
before helpers for automatic renewal against the new reason code.

Runkite-owned clients refuse redirects. Configure the final plane, OAuth, MCP and
Vault URLs directly. MCP arguments must be an object when present; duplicate
members and invalid Unicode are rejected before dispatch. Literal application key
case is retained. Approval digests and numeric predicates are exact; old approvals
require fresh submission and review. See [upgrade guidance](ops-runbook.md#hardening-upgrade).

### Tool allow/deny enforcement (MCP connectors)

The `tools.allow`/`tools.deny` filter above is a real, enforced gate, not just an advisory hint. `GetSession`'s `mcp.url` points at this control plane's own MCP proxy (`POST /internal/connectors/{name}/mcp`), not the connector's raw downstream MCP server -- so a `tools/call` request passes through here first, where a denied tool is rejected with a JSON-RPC error and **never reaches the downstream server at all**. `tools/list` responses are filtered against the same allow/deny list using the downstream server's own real tool list -- correct even for a deny-only filter (no allow list), which can't be represented in a static preview without knowing the full tool universe the downstream server exposes. An empty/missing tool name is always rejected too, independent of the filter shape (never a legitimate request regardless of what's allowed or denied). In the default JSON transport, other JSON-RPC methods (`initialize`, `resources/*`, etc.) are forwarded transparently. The opt-in negotiated transport supports the methods listed below. The connector's existing circuit breaker (guarding token fetches) also guards the proxied MCP call.

**Short-lived MCP session tokens:** for MCP connectors, `GetSession` returns a
`session_token` with a 15-minute absolute expiry, independent of downstream OAuth
TTL. Every `POST .../mcp` sends `X-Runkite-Connector-Session: <token>` plus runner
auth and run binding. The token matches tenant, agent, run, generation and
connector. Tokens are shared in Redis when `REDIS_URL` is set (`rk:conn:sess:*`),
otherwise process-local. Missing, expired or mismatched tokens fail closed.
Pre-warm does not embed these short-lived capabilities or downstream credentials
in the assignment. SDK connector helpers mint them at use time.

**Non-MCP credentials:** `api_key` / `bearer` GetSession still returns raw `credentials.access_token`. `expires_at` is a **1-hour advisory remint hint** (not revocation of the underlying secret). OAuth connectors use the IdP's `expires_in` (default 1h). Pre-warm may call GetSession to warm OAuth caches and log early failures, but never embeds credentials into the assignment.

**What this does and doesn't cover, precisely**: neither `GetSession` nor `GET /internal/connectors[/{name}]` ever hand out the connector's raw downstream credentials or raw MCP URL when MCP is configured (found and fixed after two of those leaks slipped through review) -- so there's no *Runkite-issued* way for an agent to reach the real server directly instead of through the proxy. What this can't and doesn't claim to cover: if an agent author independently has their own valid credentials or URL for that same downstream service (hardcoded, from an environment variable, from anywhere outside this system), nothing here can stop code using them directly -- that's a fundamentally different threat this feature was never positioned to solve, since Runkite can only mediate access to things it hands out itself.

### Negotiated upstream MCP transport

The existing default (`mcp.transport: json`, or omitted) forwards a single JSON
POST. Set `transport: streamable_http` for an upstream that requires MCP
negotiation, session headers, or SSE responses:

```yaml
auth:
  type: bearer
  secret_ref: env:GITHUB_SERVICE_TOKEN
mcp:
  url: http://github-mcp:8082/mcp  # upstream started with --base-path /mcp
  transport: streamable_http
tools:
  allow: [get_file_contents, list_issues]
```

The control plane uses the existing Go MCP SDK to negotiate with the upstream.
Each proxy operation gets a fresh upstream session, performs the operation, then
closes the session. This works across control-plane replicas without sticky
routing or a shared upstream session store. Upstream session state does not
persist across separate tool calls. Connector capability tokens remain bound to
the live run and generation as before; grants, argument policy, HITL and tool
filters still run before dispatch. Credentials remain on the plane.

Supported methods are `initialize`, `notifications/initialized`, `ping`,
`tools/list`, `tools/call`, `resources/list`, `resources/templates/list`,
`resources/read`, `prompts/list` and `prompts/get`. Unknown methods return
JSON-RPC method-not-found. Standalone server pushes, subscriptions, sampling and
elicitation are not exposed by this request-scoped adapter. The runner-facing
reply remains JSON, so existing Python and TypeScript connector helpers work
unchanged. The SDK handles upstream protocol headers, legacy session IDs and
POST SSE decoding. Tool schemas are fetched before execution so negotiated
parameter headers can be derived from the advertised schema.

Upstream connection and operation share a 30-second deadline. Automatic
transport reconnect retries are disabled: an uncertain tool response must not
cause a write tool to execute twice. Operators should use narrowly scoped
service credentials and upstream resource permissions as well as Runkite grants.

### Connector body limits

Raw MCP requests are limited to 1 MiB before parsing or forwarding. MCP responses
(including streamed upstream responses) are limited to 8 MiB, and OAuth token
responses to 1 MiB. Oversized requests return `413`; oversized upstream results
return an explicit `502` with `reason_code: connector_response_too_large`.
The tool may already have executed when its result is rejected. Do not
automatically retry it. Payload shrink runs after the upstream size check and
does not replace a memory bound.

The proxy rejects duplicate or case-conflicting protocol keys and forwards the
same canonical envelope inspected by policy. Argument keys retain their case.

### Payload shrink (opt-in)

After a successful connector MCP `tools/call`, the plane can stash the original result in Redis and return a preview plus a retrieve ref. In-graph tool results never arrive here. Off by default; see [Configuration](configuration.md#payload-shrink).

- Requires `REDIS_URL`. Without Redis, the original body is forwarded.
- Retrieve is `runkite_retrieve_payload` on that connector's `tools/list`, served by the plane (run+generation bound). It does not need a connector grant. A reclaimed run (new generation) cannot read the old keys.
- Token spend may drop if the runner then sends fewer bytes to the model. That effect is **inferred** from whatever the runner already reports. No percent, no invoice, no FinOps SLA. Shrink does not write `usage_events`.
- **Audit sees the preview plus ref, not the original body.** Grants, pending, HITL, and kill stay fully audited. Operators who need the full MCP result on the audit path leave shrink off. Retrieve exists for the agent during TTL, not for after-the-fact forensics.

<a id="target-bound-managed-approvals-unreleased"></a>

### Target-bound managed approvals (v0.7.0)

v0.7.0 binds SQL managed MCP approvals to a `target-v1-`
revision as well as tenant, agent, run, generation, immutable run incarnation,
connector, tool and exact argument digest. Published v0.6.1 does not implement
this binding. It applies with `policy.action_evidence` either on or off; it is
not another capture flag.

A request snapshots its configured endpoint, transport, auth configuration and
filter before policy or approval lookup, and uses that snapshot through dispatch.
Admin and Review refuse approval when the current target differs from the row.
An earlier approval cannot authorize a changed target. When the current policy
still requires review, submit the operation again for a new pending row and
fresh review. Approval still does not execute the tool, resume a framework or
make an uncertain write safe to repeat.

The digest covers the exact configured MCP URL, transport (omitted means `json`),
optional `mcp.revision`, auth type, issuer/token endpoint, client ID, `secret_ref`
locator, scopes and tool allow/deny sets. Set ordering and duplicates do not
change the digest. Keep endpoint spelling and target configuration consistent
across replicas. Credential values (`api_key`, `bearer_token`, `client_secret`
and values resolved through a reference) are excluded. Rotating a value behind
an unchanged reference keeps the binding; changing the reference, client ID or
scopes changes it. Existing token-cache and provider-revocation rules still apply.

Use nonsecret endpoint and locator fields. Put credentials in auth fields or
`secret_ref`, never URL userinfo or query parameters. Nonempty MCP, OAuth issuer
and token endpoint URLs must be HTTP(S), with no userinfo or recognized
credential query fields. These checks are not an arbitrary-secret detector;
operators remain responsible for other locator content. Failures use bounded
errors without echoing URL text. Managed tool requests fail
closed when the target descriptor is unavailable or invalid. A hash does not
make secret-bearing configuration safe to publish.

For a remote change that keeps the same URL, the operator can advance a contract
label:

```yaml
mcp:
  url: https://files-mcp.example/mcp
  transport: streamable_http
  revision: files-contract-2
```

`revision` is optional, nonsecret, at most 128 UTF-8 bytes and contains no control
characters. Changing it invalidates earlier managed approvals. It is a manual
label, not automatic schema discovery, a signed release identity or proof of the
remote tool's implementation. Coordinate it with the upstream deployment;
unchanged endpoint/label configuration cannot detect changed remote implementation.
Optional `mcp.pin_tools` now detects the observed tool-contract changes described
[below](#managed-policy-and-tool-contracts-unreleased); neither mechanism proves
which remote code executed.

Historical managed approvals without a target revision remain records but cannot
be approved or consumed. Do not fill that column by guessing today's target.
Standalone `/governance/decide` approvals retain their app-owned expiring contract
and do not acquire a managed target binding. Connector session capabilities still
identify the live run; they do not replace an exact tool approval.

See the [upgrade procedure](ops-runbook.md#connector-target-revision-upgrade) before
using this source with retained approvals, and the
[engineering rationale](internal/target-bound-approvals.md) for dispatch and
storage invariants.

<a id="managed-tool-attempt-evidence-unreleased"></a>

### Managed tool attempt evidence (v0.7.0)

v0.7.0 can record managed MCP `tools/call` with
`policy.action_evidence: true` and required SQL audit. Both JSON and streamable
HTTP call the synchronous intent gate immediately before the protected request.
Protocol initialization/discovery can happen earlier. Persisting intent and
consuming an approval are one SQL transaction. A failed pre-dispatch write blocks
the tool; a missing response after intent remains unknown and never causes replay.

Each request gets `X-Runkite-Connector-Action-Id`; approval IDs are separate.
Admin records response metadata without raw arguments, bodies or credentials.
HTTP/RPC errors and tool errors are distinct observations, not proof about a
business effect. Original generation, run incarnation and configured target revision remain
attached after reclaim or run pruning. Local tools and `/governance/decide` are outside this
history. See [Connector actions](../site/support/connector-actions.html) for
configuration, retention and unknown-outcome investigation.

### Circuit breakers

v0.7.0 counts an OAuth-backed MCP request once. Its half-open
probe covers token preparation and the protected request, so refresh cannot
reject its own probe or close it before the MCP result. A local evidence refusal
does not count as a target failure. Standalone OAuth session minting retains its
own breaker admission.

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

## Credential-safe diagnostics and rotation

OAuth acquisition errors expose HTTP status and recognized standard OAuth error codes, without provider descriptions, response bodies or raw endpoint URLs. Connector transport failures retain safe classifications such as cancellation, deadlines and connection errors. Custom `errors` mappings that relied on arbitrary body text must use the recognized safe codes instead. Tool responses remain upstream content; transport sanitization is not a general secret scanner.

The plane rereads a configured Vault token file during resolution. OAuth client-credential access tokens retain their existing cache until the expiry buffer; external client-secret rotation does not revoke already-issued tokens. A Vault lookup failure still blocks resolution instead of silently using stale authority. The ordinary KV resolver does not add dynamic leases. The separately registered `vault_database` consumer path in current source now coordinates bounded issuance and provider-confirmed cleanup; see [credential lifecycle](internal/credential-lifecycle.md). See [secrets support](../site/support/secrets.html#rotation-and-diagnostics) and the [engineering note](internal/action-authority-hardening.md).

<a id="managed-policy-and-tool-contracts-unreleased"></a>

## Managed policy and tool contracts (v0.7.0)

v0.7.0 extends target-bound approvals with the configured
`policy-v1-` revision and optional `tool-v1-` schema revision. `policy.webhook.revision`
represents operator-declared remote PDP changes, not verified remote execution.
Both capture-on and capture-off dispatch require exact matching revisions.
Historical approvals missing managed policy or target identity need new submission
and review. Reusing the same arguments does not repair missing authority.

Set `mcp.pin_tools: true` to discover the requested tool's actual MCP schema via
`tools/list` and recheck it at dispatch. Discovery failure, a missing tool or a
changed contract blocks the send rather than borrowing an old approval. Schema
pinning does not prove remote implementation identity. Keep the separate optional
`mcp.revision` label for operator-managed remote contract changes. New pinning or
credential-binding options produce `target-v2-`; existing configurations retain
the original `target-v1-` descriptor. Credential values remain outside both.

`auth.type: vault_database` plus an operator-registered `credential_binding`
provides a different path from KV `secret_ref`: a trusted external MCP executor
receives a short-lived PostgreSQL username/password through private HTTP Basic.
Initialization and schema listing acquire no database lease. Only an authorized
managed tool dispatch may start issuance. Binding/provider revisions are immutable
and included in target-v2. Keep retired providers registered for old cleanup.
See [configuration](configuration.md#registered-vault-database-credentials-unreleased),
the [separate executor example](../examples/vault_database_executor/README.md), and
[lease lifecycle rationale](internal/credential-lifecycle.md).
