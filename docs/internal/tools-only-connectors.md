# An optional tools-only surface for connector MCP

Status: Unreleased source after v0.9.0. This is an opt-in restriction on the
existing run-bound connector proxy. It adds no MCP server, credential store,
policy engine, transport, Runner Protocol message or database migration.

## Existing controls and the caller

`Registry.GetSession` already points MCP callers at the plane and keeps raw
downstream credentials there. `Registry.ProxyMCPRequest` already checks tool
allow/deny lists and filters `tools/list`; managed calls also use existing
grants, argument policy, approvals, target binding and action evidence. Those
tool controls do not restrict every other MCP method. The default JSON proxy
can forward non-tool methods, while the negotiated transport exposes its
existing supported request-scoped operations.

The caller is an operator who needs only tools from an upstream MCP server that
also exposes resources, prompts or other protocol surfaces. Turning on
`MCPConfig.ToolsOnly` limits the runner-facing method surface without replacing
tool policy or changing the default contract of other connectors.

## Configuration and inbound gate

Set `mcp.tools_only: true` in the existing connector YAML or JSON:

```yaml
mcp:
  url: https://files-mcp.example/mcp
  transport: streamable_http
  tools_only: true
tools:
  allow: [get_file_contents, list_issues]
```

Keep the connector's existing auth/`secret_ref` configuration. The profile is
independent of `json` versus `streamable_http`; omitted or false keeps existing
behavior. Tool names still need the configured filter and normal managed
authority. The setting is configured in the connector file, not an Admin edit
form.

After ordinary runner/session authentication, the proxy permits only these
inbound methods for this profile:

- `initialize`
- `notifications/initialized`
- `ping`
- `tools/list`
- `tools/call`

An unsupported request gets a local HTTP-200 JSON-RPC `-32601` method-not-found
response. An unsupported notification receives HTTP 202 with no response body.
This includes an inbound `server/discover` request; it is not an escape hatch
or a request to ask the upstream for fallback capabilities. Rejection happens
before the proxy operation's circuit breaker, tool-revision discovery,
credential resolution and upstream forwarding.

That ordering is scoped to this proxy operation. `GetSession` can already have
acquired or refreshed credentials separately before the caller submits an
unsupported method. This profile does not mean credentials are never resolved,
nor does it revoke a previously issued credential or stop other code that
independently possesses upstream access.

## Initialization and transport boundary

For JSON transport, the profile clears client capabilities in the forwarded
initialize request and strictly parses the initialize response before filtering
its capabilities. Malformed or ambiguous JSON initialize messages fail closed.
For negotiated Streamable HTTP, the existing Go MCP SDK negotiates and validates
the upstream result; Runkite filters the reconstructed result. There is no new
raw-SSE ambiguity validator or separate SSE parser in this feature.

In either transport, the runner-facing initialize result advertises only
`tools: {}` when the upstream advertised tools. It does not invent tools support,
retain `listChanged`, or advertise resources, prompts, tasks, logging,
completions, sampling or elicitation. Initialization is capability narrowing,
not an output-sanitization contract.

The allowlist gates runner-to-proxy methods, not every HTTP or protocol message
the SDK itself may exchange upstream. There is no outbound method gate. SDK
negotiation can still use upstream discovery, initialization and session
cleanup. Existing per-operation sessions and response-size limits remain.
There is no new cross-request cancellation, subscription or background session
API. Ordinary tool execution still has its own uncertainty and side-effect
semantics.

## Bind the profile into managed approval targets

Opting in wraps the existing completed target descriptor with the profile flag
and emits a `target-v4-` fingerprint. The wrapper preserves the existing
v1/v2/v3 descriptor semantics, including
transport, tool filters, tool-schema pinning, credential bindings and delegated
exchange/correlation options. Omitted or false keeps the prior fingerprint
unchanged. No stored revision is reinterpreted as v4.

Changing the profile changes the configured target identity. A retained managed
approval for a different target cannot authorize that changed operation; submit
for fresh review when policy requires it. Disabling the profile restores the
default target contract, so changing configuration is not a permanent approval
revocation mechanism. Standalone app-owned decisions gain no managed binding.

Existing run-bound connector session tokens are not target-revision-bound.
Holding an older token does not bypass the current profile check on a new proxy
request, but the token is not invalidated merely by toggling this setting.
Requests already admitted under a target snapshot are not retroactively
re-authorized. Use the rollout procedure below when changing the boundary.

## Rollout and operator checks

1. Inventory callers that use resources, prompts, completions, logging, tasks,
   cancellation or other non-tool methods. Keep the broader profile where those
   operations are required, or change the caller before opting in.
2. Upgrade every HTTP replica to matching source with the setting still false.
   Older binaries can ignore the unknown boolean and leave the wider surface
   available. Older target validators also do not understand v4. This is not a
   safe mixed-version enforcement rollout.
3. Pause affected connector callers and drain or reconcile in-flight operations.
   Apply the same opt-in configuration on every serving replica and restart
   them through the deployment's normal procedure. No DDL is required.
4. Inspect the configured `mcp_tools_only` projection and **Tools only** badge
   in Admin Connectors. These describe configuration, not a remote permission
   scan or proof that every replica is upgraded. Verify each serving replica.
5. With a synthetic run, confirm narrowed initialize capabilities, allowed tool
   discovery/call, and local rejection of a non-tool request and notification.
   Inspect counters on an owned upstream to distinguish local rejection from an
   upstream method-not-found response. Keep existing grant/HITL checks enabled.
6. Check retained managed approvals against the current target and obtain fresh
   review for changed targets before resuming callers. Do not retry a tool merely
   because its response or transport failed; external effects may already exist.

Local qualification passed with the pinned Go MCP SDK's real
`StreamableClientTransport` against an owned loopback proxy, once for a simple
JSON upstream and once for an actual SDK `StreamableHTTPHandler`. The native
caller test exercises local discovery rejection and legacy initialize fallback,
narrowed capabilities, filtered discovery, a real tool call and resource/prompt
rejection. It uses no model provider or production credentials.

Separate API tests use the actual mux, connector sessions, policy and approvals
to verify existing-session enforcement, both profile-toggle directions and
rejection before issuer/upstream work. JSON ambiguity and metadata preservation,
default/v4 fingerprint compatibility and Admin/Review revision handling have
their own regressions. These checks do not qualify a published binary, external
provider, every MCP client or a mixed-version deployment. Hosted CI remains a
separate merge gate.

## Deliberate limits

Tool results can still contain arbitrary text, embedded resources, resource
links, URLs and sensitive data. This feature does not inspect, redact, classify
or restrict that content, recursively govern a tool's internal operations, or
sandbox the upstream tool. A tools-only method surface is not a guarantee of
tools-only data or effects.

The public incoming Runkite `/mcp` agent server and app-owned `/governance/decide`
are separate boundaries. This connector option does not change them. Keep least
privilege at the upstream credential/resource server as well as Runkite's tool
grants, approvals and existing containment controls. See the
[connector guide](../connectors.md#tools-only-mcp-profile-unreleased),
[rollout runbook](../ops-runbook.md#tools-only-connector-rollout-unreleased)
and [target-binding design](target-bound-approvals.md).
