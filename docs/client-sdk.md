# Client access & SDK roadmap

How to call Runkite from application code today, and when (if ever) we ship a first-party downloadable client SDK.

This page is the durable decision record so the plan survives chat sessions.

For the unreleased hardening update, use canonical API URLs without redirects.
Connector helpers expose bounded operations and TypeScript caller cancellation;
the managed chat bridge also bounds stalled request-body intake before dispatch.
New literal `.` and `..` run/thread IDs are rejected. New run IDs also reject
controls and leading/trailing whitespace; Unicode and ordinary custom path
characters remain supported through encoded components. Legacy pending actions require fresh
submission and approval. See [upgrade guidance](ops-runbook.md#hardening-upgrade-unreleased).

Runner-side status checks now include the assignment tenant. Upgrade the plane
before runner helpers that use the new base64url identity headers. LangGraph
direct Postgres now requires named LangGraph tenants to use HTTP proxy state;
default cron threads need proxy mode too. Prior native history must be resolved
explicitly, and managed opaque SDK profiles are unaffected; see [the follow-up upgrade procedure](ops-runbook.md#follow-up-identity-and-completion-upgrade-unreleased).

## Choose who owns execution

Use **managed execution** when the caller needs Runkite Threads/Runs, runner
dispatch, managed-run admission, cancellation and usage accounting. The app
creates a run over Agent Protocol; a separate runner executes its framework.
The app keeps UI authentication and streams the result back to its user.

Use **app-owned execution** when the SDK loop must stay in the application's
backend. The governance helper asks about one tool call. Its decision and pending
approval can be audited on SQL, but no Runkite thread or run is created. Run kill
switches, break-glass and spend caps do not cover that path. The app owns pending
continuation, exact saved arguments and execution idempotency.

Terminology matters: framework SDKs execute agents; Agent Protocol clients call
the plane; the published `runkite-runner` packages execute assignments; the
unpublished governance helpers only request decisions. There is no general
first-party Runkite client SDK yet. The public
[integration guide](../site/support/sdk-integration.html) compares the paths.

### Managed Vercel AI SDK (v0.5.8)

The optional TypeScript backend uses `--framework ai-sdk`, runner kind
`typescript-ai-sdk`, and public `ToolLoopAgent` APIs. Export a per-run factory
returning supported `AiSdkAgentDefinition` settings; the plane dispatches it
through the existing Runner Protocol. See the
[managed example](../examples/managed_ai_sdk/README.md) for configuration and the
application-side `useChat` bridge. AI SDK's UI stream differs from Agent Protocol
SSE, so pointing an unchanged frontend directly at `/runs/stream` is insufficient.

The app must route these agents through Runkite instead of owning their model
loop in its HTTP handler. Native successful-turn history is stored by the runner
through the plane; frontend presentation history stays with the app. Initial
support is text and local JSON-result tools. Native approvals/resume, streaming
tool outputs, binary/client/provider tools, time travel and whole-run caching
are unsupported. The separate [Microsoft managed runner](#managed-microsoft-agent-framework-unreleased)
has its own Python factory and session profile.

This backend ships in tagged `v0.5.8` `runkite-runner` artifacts. Install the
tested optional `ai@7.0.155` peer with the runner. The app-side bridge needs a
server key that can create, read and cancel managed runs; `governance:decide`
alone is insufficient.

#### Unreleased: one UI bridge for managed Python agents

The existing [application bridge](../examples/managed_ai_sdk/README.md#use-the-same-frontend-with-a-python-runner)
also accepts server-selected `agent-framework`, `openai-agents`, `pydantic-ai`
and `google-adk` profiles. It converts their ordinary Agent Protocol assistant
text into the same native AI SDK UI transport. The default `ai-sdk` path retains
native tool UI. Python modes expose text and Pydantic final JSON, with no new
tool panels, reasoning display or native approval/resume.

The app still owns authentication, presentation history, the chat/turn mapping
and durable assistant-ID binding. Checked route hooks and Next/Nuxt recipes
reuse create, detach, attach and Stop. Attach expects a bridge-created turn with
one new user string message. Completion checks the exact current-turn suffix
against persisted run input, the framework/generation and streamed text; a
historical assistant cannot fill an empty typed-only turn. Pydantic's final
`data-result` part supports ordinary JSON including null, false and zero.
Result nesting is limited to 64 levels; integral values must fit JavaScript's
safe integer range. Larger exact identifiers should be strings.

This remains a checkout-only example, rather than a published client package.
No model loop moves into the app handler. Full retained replay and one-attempt
identity are required; the existing example payload limits still apply.

#### Unreleased: reconnecting the managed UI stream

The checkout's app-side bridge adds `disconnectPolicy: "detach"` for callers
whose run should continue after its browser leaves. Create requires an explicit
server-owned run ID in detach mode; persist that chat/turn-to-run mapping before
the request. The default create policy remains `"cancel"` for existing callers.

An authenticated application GET route calls `attachManagedChatResponse` with
the owned thread/run IDs. It joins the existing run and defaults to detach;
it does not create another run or resubmit input. Configure the pinned native
`DefaultChatTransport` with `resumeStreamIsReplay: true` and a reconnect GET
route, then use `chat.resumeStream()` after restoring the app's UI messages.
Full replay resets the streaming assistant instead of appending its partial
text/tool results twice. The app still owns authentication, message storage and
mapping checks on create, attach and Stop.

Persist the first assistant ID with the awaited `onMessageStart` callback before
displaying its start. The app transaction must accept an unset or matching ID
for that owned turn/run and reject a different stored ID. Pass it back as
`expectedMessageId` on create retries and attach. That precondition protects
against a new generation silently becoming another assistant after reconnect.
Without it, a caller must be a fresh observer with no unknown partial assistant
to merge. The callback is repeated during replay and must be idempotent; failure
ends delivery, while detach keeps the managed run alive. Use its supplied
`AbortSignal` and the store's normal deadline. The helper bounds its wait, but
cannot roll back a database commit already in progress after timeout.

Use `cancelManagedChatRun` from a separate authenticated application POST route
for explicit Stop. A local `chat.stop()` only disconnects a detach stream.
Cancellation is cooperative, may race success and cannot undo external writes.

This initial reconnect supports one attempt with retained complete native event
history. Missing start, replay gaps and expiration fail explicitly; the bridge
withholds finish and does not manufacture a transcript or retry model/tools.
It still refuses to combine generations after reclaim. See the
[example's reconnect and retention contract](../examples/managed_ai_sdk/README.md#reattach-to-an-existing-run).
These helpers are an unreleased example update, not another client SDK package.

### Managed Microsoft Agent Framework (unreleased)

For Microsoft agents that should dispatch through Runkite and appear in Admin
Threads/Runs, use the managed Python adapter (`python-agent-framework`) and the
[offline managed example](../examples/managed_agent_framework/README.md). The
adapter is included in source-built `runkite-runner` wheels and sdists, with the
optional `agent-framework` extra pinned to `agent-framework-core==1.23.0` and
the dedicated `runkite-runner-agent-framework` entrypoint. The installed module
is `python -m agent_framework_adapter`; no `PYTHONPATH` is required. This packaging
is unreleased and is not in the published PyPI `0.5.8` artifact. Install the
local source package as shown in the example until a release contains it. This
is a runner profile, separate from the unpublished governance helper's extra.

It uses public Agent streaming and session APIs, stores native session history in
opaque checkpoints with CAS, and recovers committed turns without repeating
model or tool work. Use the same `RUNNER_TOKEN` for gRPC and checkpoint HTTP,
and declare `runner_kind: python-agent-framework` in the plane's config. Supply
only the new user/system text messages for a turn, ending with a user message;
native session state owns prior history. Failed or cancelled turns retain the
previous committed history and report any observed usage. Workflows, .NET,
non-text inputs and native approval/resume are out of the initial scope.
Unsupported approval or resume fails explicitly. API cancellation can retain
only usage already delivered to the plane before cancellation commits. Keep the
governance middleware when the app owns execution and only needs tool decisions.

### Managed OpenAI Agents SDK (unreleased)

Python apps that already export an `agents.Agent` can dispatch it through runner
kind `python-openai-agents`. The source-built `runkite-runner` wheel includes
`openai_agents_adapter` and the dedicated `runkite-runner-openai-agents` CLI.
The optional `openai-agents` extra pins `openai-agents==0.24.0`. This profile is
not in the published PyPI `0.5.8` artifact. Follow the
[offline example](../examples/managed_openai_agents/README.md) for a clean local
installation until a release contains it.

The runner uses public native streaming and history APIs with Runkite's existing
dispatch, cancellation, usage and conditional opaque checkpoint writes. Only
successful turns commit history. Receipts let a reclaimed run replay without
another factory/model/tool cycle after a successful commit. Supply only this
turn's new user/system text, ending with a user message; the application keeps
UI authentication and presentation history. This is an Agent Protocol stream,
not an AI SDK UI transport.

Initial support is one text agent and local function tools. Native approvals,
resume, handoffs, agents-as-tools, hosted/MCP tools, structured final outputs and
non-text input fail explicitly. A local tool does not automatically receive
connector grants or HITL. Native SDK trace export is disabled; the generic
worker's OTLP run span remains available. See the
[runner boundaries](../python/adapters/openai_agents_adapter/README.md).

### Managed Pydantic AI (unreleased)

Python callers with a native `pydantic_ai.Agent` can use runner kind
`python-pydantic-ai` for real Threads/Runs, streaming text and validated
structured results. The source-built runner includes `pydantic_ai_adapter` and
the `runkite-runner-pydantic-ai` command. The optional `pydantic-ai` extra pins
`pydantic-ai-slim==2.63.0`; install any provider package your own agent needs.
This profile is absent from published PyPI `0.5.8`. Follow the
[source installation example](../examples/managed_pydantic_ai/README.md).

The SDK performs validation and retries. Only its fully completed final result
can commit a successful turn; streamed JSON fragments are not durable success.
Native conversation state uses the existing opaque checkpoint API with CAS.
The runner reports this run's usage rather than rebilling earlier conversation
turns. A committed receipt replays without calling its factory, model or tools.

The application keeps UI authentication and presentation history. New input is
text only; local tools need existing connector helpers for connector policy and
HITL. Native deferred approvals, output functions, multimodal input and external
workflow engines are outside the first profile. See the
[adapter contract](../python/adapters/pydantic_ai_adapter/README.md).

### Managed Google ADK (unreleased)

Callers with an ordinary native `LlmAgent` can dispatch it through
`python-google-adk`. The model loop stays in Python while the plane owns
Threads/Runs, streaming, cancellation, reported usage and durable storage.
The source-built `google-adk` extra pins `google-adk==2.13.0` and installs
`google_adk_adapter` plus `runkite-runner-google-adk`. Published PyPI `0.5.8`
does not include this profile; use the
[source installation example](../examples/managed_google_adk/README.md).

Send exactly one new user text message per run. The native session supplies
previous history. A run-local public `BaseSessionService` restores that session
from opaque CAS state; successful-turn receipts recover without repeated model
or tool work. The adapter drains the invocation and validates completion instead
of treating a final-response event as proof of success.

Session-local state is supported. ADK's cross-session `app:` and `user:` state
is rejected; `temp:` state lasts only for the invocation. Native confirmation,
workflows, delegation, memory, artifacts, live audio and unfinished-turn resume
are unsupported. Ordinary tools need existing connector helpers for connector
policy and HITL. See the [adapter contract](../python/adapters/google_adk_adapter/README.md).

## Use today (recommended)

Runkite’s public HTTP surface is [Agent Protocol](https://github.com/langchain-ai/agent-protocol) plus documented Runkite extensions. The machine-readable contract is:

- [`spec/openapi.json`](../spec/openapi.json) — **use this** for clients and codegen
- [`spec/openapi-admin.json`](../spec/openapi-admin.json) — Admin UI / operators
- [`spec/openapi-internal.json`](../spec/openapi-internal.json) — runners only (not for app clients)

### Option A — LangGraph SDK (Python)

Runkite implements the same Agent Protocol the LangGraph SDK expects (`/assistants/*` aliases included):

```bash
pip install langgraph-sdk
```

```python
import asyncio
from langgraph_sdk import get_client

async def main():
    client = get_client(url="http://localhost:2026")
    thread = await client.threads.create()
    async for event in client.runs.stream(
        thread["thread_id"],
        "echo_agent",
        input={"messages": [{"role": "user", "content": "hello"}]},
    ):
        print(event.event, event.data)

asyncio.run(main())
```

Longer walkthrough: [`docs/quickstart.md`](quickstart.md) §6.

### Option B — Any Agent Protocol / OpenAPI client

Point any HTTP client (curl, generated OpenAPI client, custom wrapper) at the control plane. Minimal create → run → stream:

```bash
# needs a running control plane + runner (see docs/quickstart.md)
BASE=http://localhost:2026

THREAD=$(curl -sf -X POST "$BASE/threads" -H 'Content-Type: application/json' -d '{}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["thread_id"])')

curl -sf -N -X POST "$BASE/threads/$THREAD/runs/stream" \
  -H 'Content-Type: application/json' \
  -d '{"agent_id":"echo_agent","input":{"messages":[{"role":"user","content":"hello"}]}}'
```

With auth enabled, send `Authorization: Bearer <key>` or `X-API-Key: <key>` (see [`spec/README.md`](../spec/README.md)).

### Option C — OpenAPI artifact on GitHub Releases

Tagged releases attach `openapi.json`, `openapi-admin.json`, and `openapi-internal.json` so you can download the contract without cloning the repo (workflow: `.github/workflows/release.yml`).

## Deliberately not built yet — first-party client SDK

**Decision (2026-08):** do **not** ship `pip install runkite` / `npm install @runkite/sdk` until there is clear traction that LangGraph SDK + raw OpenAPI are not enough.

If/when traction warrants it, build a **thin** first-party client (not a full mirror of admin/internal/MCP):

| Item | Intent |
|---|---|
| Packages | `runkite` (PyPI) and `@runkite/sdk` (npm) |
| Layout | `sdk/python/`, `sdk/typescript/` in this repo (or sibling repos) |
| Scope v1 | agents, threads, runs (create/stream/wait/cancel), store — happy path only |
| Source of truth | generate or hand-wrap from `spec/openapi.json` |
| Install story | PyPI/npm primary; GitHub Releases optional source tarballs |
| CI | smoke against `runkite serve` + echo agent |
| Out of v1 | admin-api, internal runner API, full MCP surface, 1:1 coverage of every OpenAPI op |

Until that client ships, use LangGraph SDK or another Agent Protocol client to
call the plane. The runner packages and governance helpers are separate APIs;
they are not substitutes for a full application client.

## Embedded governance helper

[`typescript/runkite-governance`](../typescript/runkite-governance) and [`python/runkite_governance`](../python/runkite_governance) call `POST /governance/decide` and `GET /governance/actions/{action_id}`. They are not the SDK above. They do not create threads or runs, and they are not published. The app still executes the tool after `allow`. `resolve()` polls until the pending row is approved (returned as allow) or rejected or consumed (returned as deny). Default wait is 60 seconds. Pass an `AbortSignal` (TypeScript) or a `threading.Event` (Python) and the poll stops when the caller aborts.

### Local package builds

The small helpers now include packaging metadata, compiled JavaScript and type
declarations, plus a standalone Python wheel with no runtime dependencies.
They remain unpublished until a maintainer releases them to npm and PyPI.

```sh
cd typescript/runkite-governance
npm ci
npm pack
cd ../..
python -m pip install ./python/runkite_governance
```

Install the npm tarball in the application that owns the model loop. See each
package README and the [AI SDK example](../examples/governance_ai_sdk/README.md).
The default 60-second timeout covers decide, polling HTTP headers/bodies and
waits. TypeScript composes a deadline with the caller's abort signal. Python
bounds the caller wait and the number of blocking stdlib transport workers;
those workers use the remaining socket timeout and close responses on unwind.
Cancellation can return before an operating-system DNS lookup finishes.

### Guard an application-owned tool

The TypeScript helper exports `createGovernedExecute` for single-result tool
functions, including AI SDK `tool.execute`. It snapshots and freezes JSON object
arguments, asks the plane once, and invokes the executor only after `allow`.
Denied and pending calls throw `GovernanceBlockedError` by default. Explicit
`onDenied` and `onPending` handlers can return application results instead;
the pending handler must persist the action ID and exact original operation
before returning. The guard never polls, resumes or retries execution.

An `AbortSignal` stops decision work and prevents starting callbacks after
cancellation. The application controls cancellation once its callback has
started. Streaming tool executors are outside this helper's scope. See the
[helper usage](../typescript/runkite-governance/README.md) and the example's
`tools.ts` for typed AI SDK integration. Run visibility, kill switches, spend
caps and execution idempotency remain outside this decision-only guard.

The AI SDK example also tests ordinary guarded tools inside `streamText`,
including text chunks, allow/deny/pending results, policy failure and cancellation.
Streaming chat output does not require streaming tool executors.

`createGovernedExecute` lives in the unpublished `runkite-governance` package,
not in published `runkite-runner`. Its `runId` is a stable application operation
identifier. Neither the helper nor the decision endpoint deduplicates tool
effects, creates managed runs, or proves an allowed tool actually executed.

### Microsoft Agent Framework function middleware

The Python helper has an optional `agent-framework` extra, pinned to the tested
framework core. `runkite_governance.agent_framework` exports
`GovernanceFunctionMiddleware` for local `FunctionTool` calls. It decides once,
executes only after allow, and stops the agent with a native middleware failure
on deny, pending or policy failure. The base client remains dependency-free.

This middleware ships in the unpublished Python `runkite_governance` package
(extra `agent-framework`, pinned to `agent-framework-core==1.23.0`). It does not
create Admin Threads/Runs by itself.

An async pending callback persists the action ID and exact frozen arguments.
The app continues its own operation on a later request; the middleware never
polls or resumes the model loop. See the [Microsoft example](../examples/governance_agent_framework/README.md)
for JSON argument constraints, durable continuation, native streaming tests and
`make test-governance-agent-framework`. This uses the same decision-only boundary
as the TypeScript guard, with no managed run, run controls or spend caps.

## Related

- Spec regeneration / drift CI: [`spec/README.md`](../spec/README.md) (`make openapi`, `make openapi-check`)
- Control-plane + runner setup: [`README.md`](../README.md), [`docs/quickstart.md`](quickstart.md)
