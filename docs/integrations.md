# Integrations through existing boundaries

These integrations keep Runkite as a self-hosted operational plane. Other
projects retain their clients, agent loops and observability backends.

## Cline calls Runkite over MCP

[Cline](https://github.com/cline/cline) supports Streamable HTTP with custom
headers. Add a server entry using the operator's Runkite URL and client key:

```json
{
  "mcpServers": {
    "runkite": {
      "type": "streamableHttp",
      "url": "https://runkite.example/mcp",
      "headers": {"Authorization": "Bearer REPLACE_WITH_RUNKITE_CLIENT_KEY"},
      "disabled": false,
      "autoApprove": []
    }
  }
}
```

Configured agents appear as MCP tools. Calling one uses the same admitted
`createRunCtx` and runner dispatch path as an Agent Protocol client. Use the
tenant and permissions appropriate for that client. Follow the existing
[MCP session affinity guidance](mcp-server.md) for legacy session-based clients
behind multiple replicas. Client-side consent does not bypass plane policy.
No Cline runtime or approval database belongs in the Go binary.

Upstream configuration reference:
[Cline MCP](https://github.com/cline/cline/blob/main/docs/mcp/mcp-overview.mdx).

## Deep Agents uses the LangGraph runner

[The pinned offline example](../examples/deepagents_agent/README.md) exports the
compiled graph returned by upstream `create_deep_agent`. The existing graph
loader, runner protocol, checkpoint/store attachment and cancellation channel
remain the integration points. Native upstream subagents remain inside that
graph; they are not automatically separate plane runs.

## Vercel AI SDK: choose execution ownership

The [managed runner example](../examples/managed_ai_sdk/README.md) ships in
**v0.5.8** with `runkite-runner`. A per-run factory returns public AI SDK
settings; `--framework ai-sdk` executes the agent inside the TypeScript runner.
The app creates real Runkite Threads/Runs and consumes results through an
application-side `useChat` bridge. The model loop moves out of its HTTP handler.
This uses the existing Runner Protocol and checkpoint surface, not a Vercel fork
or a JavaScript runtime inside the Go plane.

[The server-side example](../examples/governance_ai_sdk/README.md) wraps a tool
with the small governance client. It persists a pending action ID in the app,
returns promptly, and resumes app-owned execution after an operator resolves the
action. The app owns tool idempotency and conversation state. This path creates
no Runkite thread or run, does not consume approval on GET, and does not apply
run kill switches, break-glass or spend caps. User consent in a chat UI is
different from an operator's Admin approval.

Upstream: [vercel/ai](https://github.com/vercel/ai). Helper package preparation
does not publish a package or imply that an upstream contribution was accepted.

Both paths support streaming chat with ordinary single-result tools. Streaming
tool outputs and native approval/resume are outside the initial managed adapter.
Neither an app-owned allow decision nor an operation `run_id` proves execution
or supplies idempotency. See [client integration guidance](client-sdk.md).

## Microsoft Agent Framework: function middleware

The [Python example](../examples/governance_agent_framework/README.md) attaches
the unpublished helper's optional `GovernanceFunctionMiddleware` to local
`FunctionTool` objects. It asks Decide once and stops on deny or pending. The
app saves the exact proposal and continues its own operation later; it does not
create a Runkite run or resume a managed Microsoft agent. The tested optional
core is pinned to `agent-framework-core==1.23.0`. A Microsoft managed runner is
not implemented.

Upstream: [microsoft/agent-framework](https://github.com/microsoft/agent-framework).
Use public function middleware APIs; no upstream fork, second approval service
or embedded framework server is needed.

## Phoenix receives existing OTLP traces

[Phoenix](https://github.com/Arize-ai/phoenix) accepts OTLP trace exports. Run its
server separately, then set the standard exporter variables on both the plane
and runners, for example with an external collector on the same private network:

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=http://phoenix:4317
export OTEL_EXPORTER_OTLP_PROTOCOL=grpc
export OTEL_SERVICE_NAME=runkite
```

Use a distinct service name such as `runkite-runner` for runner processes.
Configure exporter headers when the receiver requires authentication. Verify
one connected client/plane/runner trace before adding backend-specific semantic
mapping. Existing spans contain run and model/tool metadata; they do not capture
prompt/completion content. `LOG_FORMAT=json` controls stdout logs separately.
No Phoenix UI, storage engine or additional exporter is embedded in Runkite.

## GitHub MCP Server is an external connector

Deploy [GitHub MCP Server](https://github.com/github/github-mcp-server)
separately. For a `/mcp` endpoint, configure its documented HTTP `--base-path
/mcp`, and point the connector's `mcp.url` to that endpoint. Use
`mcp.transport: streamable_http` as described in [Connectors](connectors.md).
The Go SDK negotiates protocol/session headers and decodes POST SSE while the
plane retains grants, HITL, run binding and credential injection.

Start with read-only tools and a narrow service credential resolved through
`secret_ref`. Add writes only with explicit plane grants and the upstream
resource permissions they need. Upstream tools that require subscriptions or
interactive elicitation are outside the request-scoped adapter's supported
surface. Do not introduce a personal OAuth-token database, replicate GitHub
authorization logic, or return the raw credential to the runner.

The repository's connector tests exercise real Go SDK handlers over in-memory
HTTP with modern discovery, legacy sessions and SSE. They do not use a live
GitHub service or GitHub credentials. A combined deployment and upstream
maintainer acceptance remain separate validation steps.
