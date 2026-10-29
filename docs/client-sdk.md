# Client access & SDK roadmap

How to call Runkite from application code today, and when (if ever) we ship a first-party downloadable client SDK.

This page is the durable decision record so the plan survives chat sessions.

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

Until that ships, “SDK” in docs means **LangGraph SDK** (or any Agent Protocol client), not a Runkite-branded package.

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

### Microsoft Agent Framework function middleware

The Python helper has an optional `agent-framework` extra, pinned to the tested
framework core. `runkite_governance.agent_framework` exports
`GovernanceFunctionMiddleware` for local `FunctionTool` calls. It decides once,
executes only after allow, and stops the agent with a native middleware failure
on deny, pending or policy failure. The base client remains dependency-free.

An async pending callback persists the action ID and exact frozen arguments.
The app continues its own operation on a later request; the middleware never
polls or resumes the model loop. See the [Microsoft example](../examples/governance_agent_framework/README.md)
for JSON argument constraints, durable continuation, native streaming tests and
`make test-governance-agent-framework`. This uses the same decision-only boundary
as the TypeScript guard, with no managed run, run controls or spend caps.

## Related

- Spec regeneration / drift CI: [`spec/README.md`](../spec/README.md) (`make openapi`, `make openapi-check`)
- Control-plane + runner setup: [`README.md`](../README.md), [`docs/quickstart.md`](quickstart.md)
