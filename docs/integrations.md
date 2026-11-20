# Integrations through existing boundaries

These integrations keep Runkite as a self-hosted operational plane. Other
projects retain their clients, agent loops and observability backends.

Start with the [public integrations guide](https://getrunkite.com/support/integrations.html)
to choose a task and follow its setup: evaluate agents, call a specialist,
connect a chat UI, export traces or route model requests. That guide links the
existing support pages and distinguishes published runtime features, checkout
examples, configuration recipes and experimental fixtures. This source guide
records the underlying contracts and qualification evidence.

<a id="unreleased-current-source"></a>

## v0.10.0 integrations

This guide maps the runtime additions in v0.10.0 and their separate application
examples. Use matching v0.10.0 plane and runner artifacts for the runtime
features. Caller recipes, UI bridges and qualification tools remain checkout
assets where their guides say so; upgrading a package does not install them.
Use the linked contracts and qualification commands before adopting a slice.
The [support feature map](../site/support/feature-map.html#current-source)
provides the same availability boundary for operators.

Discover and operate an existing agent:

- [Assignment identity protocol 1](internal/assignment-identity.md) requires
  matching planes and runners, exact run/record/generation echoes and a
  coordinated drain of older assignments and all writers, verified backups and
  Postgres/SQLite migration 23 or MySQL migration 24. Legacy unbound accounting,
  terminal-hook claims and same-ID receipts require explicit reconciliation;
  new intentional operations need fresh IDs. Events and cancellation use separate
  record namespaces. This is continuity fencing, not a secret capability, worker attestation or external-effect
  idempotency. Kafka is off unless its non-atomic compatibility mode is
  explicitly accepted, including when paired with Redis.

- [Tools-only connector profiles](connectors.md#tools-only-mcp-profile-unreleased)
  optionally narrow an outgoing connector proxy's inbound method surface and
  initialize capabilities. Existing filters, grants and approvals still apply.
  It does not inspect tool results, gate every SDK upstream message or affect
  the incoming public agent `/mcp` server. Upgrade all HTTP replicas before
  enabling the profile; opt-in binds managed targets as v4.
- [Static graph schema recovery](internal/schema-publication-recovery.md) keeps
  cached static Python LangGraph reports recoverable after a registration reset
  or failed startup publication. Its new endpoint atomically fills only existing
  exact bootstrap rows, preserving useful/partial reports. Matching plane and
  worker versions are required. Other SDK/factory/named-tenant introspection and
  code freshness are not added; existing MCP catalogs still need reconnecting.
- [Structured MCP input discovery](internal/mcp-input-schemas.md) enriches only
  the nested `input` envelope from a bounded usable report. Raw invocation,
  defaults and coercion stay unchanged. Missing or unsupported reports remain
  generic; freshness is unknown and an existing session keeps its catalog.
- [Configured-profile descriptors](internal/agent-capabilities.md) help Agent
  Detail and Try Agent explain continuity, resume and local-tool conditions.
  Unknown profiles and execution pools stay unknown. This is advisory metadata,
  not readiness, code attestation, effective permissions or a live runner probe.
- [Registry metadata and declared requirements](registry.md) keep definitions
  discoverable without installing them. Admin preserves loaded opaque metadata;
  the optional `runkite_definition` convention is publisher-supplied. The
  [numeric preservation fix](internal/opaque-json-preservation.md) qualifies
  SQLite/Postgres precision and states MySQL/Mongo limits. PUT still replaces
  submitted values and adds no concurrent-edit protection. Agent/Registry
  selectors retain exact tenant bytes. Existing registry records use an explicit
  edit route, so a record literally named `new` remains editable; changing record,
  tenant or create/edit mode resets the form and confirmation state.
- [Cron retained-run visibility](internal/cron-run-visibility.md) links a
  schedule's exact-tenant thread and latest retained run. Retained-run links open
  guarded, read-only snapshots with manual Refresh, without live SSE, cancel or
  child-run navigation. Empty history, unavailable reads and failures before run
  creation remain different cases. This adds no fire ledger, replay, next-fire
  prediction or scheduler behavior.
- [Cron calendar preview](internal/cron-next-fire-preview.md) separately projects
  one next calendar boundary using the scheduler's shared backend parser.
  Independent `include_preview=true` leaves definition-only/history-only callers
  compatible and adds no thread/run/claim lookups to preview-only requests.
  Timezone/DST behavior follows the pinned parser; relative `@every` preview is
  unavailable without an anchor. A projected boundary is not a dispatch promise,
  skip ledger or live countdown. Specific parser/interval guards are documented
  separately from the preview's read-only behavior.
- [Offline static policy fixtures](policy-fixtures.md) run `runkite policy test`
  over declared grants, predicates and mandatory review using the current
  engine. They contact no services and execute no tools. Live
  [simulation](sim.md) continues to create managed runs and can call connectors.
- [Exact policy identities](internal/exact-policy-identities.md) keep positive
  grants and tool allow lists tied to their supplied resource names. Restrictive
  predicates/HITL retain their conservative trim behavior, so ambiguous padded
  configuration can deny more. Upgrade all replicas with affected callers and
  continuations paused, then obtain fresh reviews. Previously returned app-owned
  decisions and saved app continuations are not revoked by a source upgrade;
  follow the documented discard/resubmit procedure. The Admin grant form retains
  exact loaded row/scope bytes and takes tool names as JSON string arrays,
  preserving names containing commas or spaces without splitting or trimming.

Choose the caller or presentation boundary:

- [Scoped specialist access](auth.md#scoped-specialist-callers-unreleased-source-checkout-only)
  separates `agents:<id>:run` from `agents:<id>:observe`. The former admits
  filtered MCP discovery and invocation; the latter permits exact public
  GET/wait observation with a saved SQL run reference. Observation is
  agent-wide, including retained runs, not requester-private ownership. It
  grants no stream, cancel, search or thread-history access.
- [Langflow MCP Tools](../examples/langflow_mcp/README.md) keeps the surrounding
  flow and model loop in Langflow. Its pinned native headless client was
  qualified separately with baseline read/write and run-only scoped authority
  against an integrated local Runkite plane and Python worker. Each profile
  produced two runs and two assignments with the same native dependencies.
  The visual editor, credential-variable UI and arbitrary agent loop remain
  deployment checks. This MCP-only proof does not qualify REST observation.
  The stock component is not a durable operation-receipt store.
- [promptfoo managed-run provider](../examples/promptfoo/README.md) evaluates
  ordinary governed runs with the pinned native loader, scheduler and CLI.
  Each intentional call has fresh IDs; one saved intent permits at most one
  create attempt, followed by read-only observation. Unknown delivery never
  silently dispatches replacement work. Broad read/write and scoped run/observe
  profiles passed separately, including reference-pinned waits and scoped
  denial probes. Local qualification uses a scripted worker and local assertions,
  not a real model or every evaluator strategy. The bundled `ready` assertion is
  a connection smoke test. Automated red-team strategies are not qualified, and
  an output assertion alone does not prove a connector denial or absence of a
  tool side effect; those need checks against the relevant evidence or a
  controlled tool backend.
- [AG-UI text transport](../examples/managed_ag_ui/README.md) presents an existing
  managed run through an application route and the official encoder/client.
  It reuses the shared bridge's ownership, durable finish, attempt, retained
  replay, detach and explicit Stop behavior. It supports plain text only,
  not tool, reasoning, interrupt or native approval events. The host app owns
  authentication, chat/turn mapping and assistant-ID binding; streaming and
  Stop still require the application's broader read/write authority.

Keep operation intent and the first acquired run reference where the caller
contract requires them. Neither an operation ID nor first reference acquisition
proves continuity before it was saved or after pruning. A response, approval,
cancel request or disconnected stream does not prove a remote business effect
or rollback. See [current-source limits](limitations.md#unreleased-current-source-boundaries)
and the [integration checklist](internal/ecosystem-integration-checklist.md).
These examples establish bounded interoperability, not upstream endorsement.

Qualify an external execution boundary:

- The v0.10.0 checkout [OpenShell effective-policy fixture](internal/openshell-effective-policy.md)
  adds an optional comparison to the existing disposable JSONL qualifier.
  Matching current/loaded policy snapshots are checked against the configured
  boundary by the official OpenShell 0.1.8 checker. The fresh provider-free
  local macOS fixture passed; inspect the separate Linux workflow's actual
  current result for that environment. This changes no production worker or
  authorization and proves neither atomic exec binding, continuous kernel enforcement nor hidden
  provider-state absence. The external profile remains experimental.

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

The v0.6.1 shared bridge can also present managed Microsoft, OpenAI Agents,
Pydantic AI and Google ADK Python output in the same AI SDK UI. Its server-selected
profiles translate ordinary text events and validate the current durable turn;
Pydantic JSON remains a separate final result. Reconnect and Stop keep the same
application ownership checks. See the
[Next/Nuxt route recipes](../examples/managed_ai_sdk/README.md#use-the-same-frontend-with-a-python-runner).

## vLLM Semantic Router routes the runner's model calls

The published walkthrough
[One Conversation, Multiple Models](https://vllm-sr.ai/blog/runkite-ai-sdk-semantic-router/)
connects a Runkite-managed AI SDK agent to Semantic Router through the runner's
OpenAI-compatible model client. Runkite admits and dispatches the run, the SDK
executes its loop, and Semantic Router selects among configured model backends.
The plane does not proxy model traffic or choose models.

Follow the walkthrough for its runnable setup, connector allow/deny exercise and
recorded verification scope. Its integration example is external to this
repository; it is not another shipped runner package. Keep Semantic Router
separate from Runkite's process, policy store and secrets. Publication describes
working interoperability, not production adoption or an endorsement.

## Microsoft Agent Framework: function middleware

The [Python example](../examples/governance_agent_framework/README.md) attaches
the separately published `runkite-governance` helper's optional
`GovernanceFunctionMiddleware` to local `FunctionTool` objects. It asks Decide once and stops on deny or pending. The
app saves the exact proposal and continues its own operation later; it does not
create a Runkite run or resume a managed Microsoft agent. The tested optional
core is pinned to `agent-framework-core==1.23.0`. This middleware is separate
from the [managed Microsoft runner](../examples/managed_agent_framework/README.md),
which creates real Threads/Runs and persists native session state. Its optional
`runkite-runner[agent-framework]` install profile ships in v0.6.1.

Upstream: [microsoft/agent-framework](https://github.com/microsoft/agent-framework).
Use public function middleware APIs; no upstream fork, second approval service
or embedded framework server is needed.

## OpenAI Agents SDK uses a managed Python runner

The [offline example](../examples/managed_openai_agents/README.md) exports a native
`agents.Agent` factory for `python-openai-agents`. Runkite dispatches it through
the generic worker and persists successful-turn native history and receipts
through the existing opaque HTTP API. The SDK retains its native model loop
inside the Python process. This integration uses the public
[SDK streaming](https://developers.openai.com/api/docs/guides/agents/running-agents)
and [history APIs](https://developers.openai.com/api/docs/guides/agents/results),
without an upstream fork or OpenAI-hosted Agents API session service.

The optional `openai-agents` extra pins `openai-agents==0.24.0`, and the installed
entrypoint is `runkite-runner-openai-agents`. This profile ships in published
PyPI `0.6.1`; see the install instructions in the example. Text and local function tools are the initial scope. Native approvals,
resume, handoffs, agents-as-tools, hosted/MCP tools and structured final outputs
are unsupported. Connector policy applies only to tools using the existing
connector helpers. Native SDK trace export is disabled; generic-worker OTLP run
tracing stays on the existing exporter path.

Upstream: [openai/openai-agents-python](https://github.com/openai/openai-agents-python).
The control plane, policy engine, secrets and durable run records remain in
Runkite. Local tool side effects before a state commit still need idempotency.

## Pydantic AI uses a managed Python runner

The [offline example](../examples/managed_pydantic_ai/README.md) uses the public
SDK inside `python-pydantic-ai`. Runkite dispatches the native agent and stores
validated final JSON alongside ordinary managed run data. The SDK owns its
validation and model loop. Runkite reuses its generic worker and opaque CAS
checkpoint surface, with successful-turn receipts for reclaim.

The optional `pydantic-ai` extra pins `pydantic-ai-slim==2.63.0` and provides
`runkite-runner-pydantic-ai`. This profile ships in published PyPI `0.6.1`;
application providers are installed separately.
Native conversation serialization preserves SDK metadata while per-run billing
excludes previously committed turns. Deferred approvals, output functions,
multimodal input and external workflow engines are not part of this profile.

Upstream: [pydantic/pydantic-ai](https://github.com/pydantic/pydantic-ai).
The boundary is a runner using public execution, validation and conversation
APIs. It does not embed an upstream server, UI or a second durability engine.

## Google ADK uses a managed Python runner

The [offline example](../examples/managed_google_adk/README.md) exports a fresh
ordinary `LlmAgent` for `python-google-adk`. Its public `Runner.run_async` executes
inside the Python worker. A run-local `BaseSessionService` restores native session
state/events through Runkite's opaque CAS API and commits successful receipts.
The plane retains dispatch, admission, cancel, spend and audit through existing
protocols. No upstream server, session database or hosted Agent Engine is added.

The source-built `google-adk` extra pins `google-adk==2.13.0` and provides
`runkite-runner-google-adk`. This profile ships in published PyPI `0.6.1`.
Start with one user text message per run and ordinary local tools.
Connector policy requires existing connector helpers. Shared `app:`/`user:` state,
native confirmation/resume, workflows/delegation, memory/artifacts, structured
output and live audio are outside the first profile.

Upstream: [google/adk-python](https://github.com/google/adk-python).
Native model/tool callbacks provide the execution boundary; completed invocation
validation, generation fencing and receipt recovery remain Runkite responsibilities.

## Langfuse receives existing OTLP traces

The [Langfuse deployment recipe](langfuse.md) uses the plane and runner exporters
already present in published v0.9.0. The guide and qualification are new
current-checkout work. Run Langfuse separately, configure its full OTLP traces
URL and project authorization on every emitting process, and select HTTP:
protobuf for Go/Python, JSON for the current TypeScript HTTP exporter.

Existing Runkite operational spans carry identity, model/tool names and status.
This path does not automatically capture prompt/completion bodies or supply
Langfuse generation measurements, user/session mapping or cost data. Logs and
metrics remain separate. Follow the guide's data-access and persistence checks
before using a production receiver. An optional collector can provide fan-out
or explicitly qualified mapping without a new exporter inside Runkite.

Upstream: [langfuse/langfuse](https://github.com/langfuse/langfuse).
See the [implementation and qualification boundary](internal/langfuse-otlp.md).

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
