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
# needs a running control plane + runner (see README Quick Start)
BASE=http://localhost:2026

THREAD=$(curl -sf -X POST "$BASE/threads" -H 'Content-Type: application/json' -d '{}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["thread_id"])')

curl -sf -N -X POST "$BASE/threads/$THREAD/runs/stream" \
  -H 'Content-Type: application/json' \
  -d '{"agent_id":"echo_agent","input":{"messages":[{"role":"user","content":"hello"}]}}'
```

With auth enabled, send `Authorization: Bearer <key>` or `X-API-Key: <key>` (see [`spec/README.md`](../spec/README.md)).

### Option C — OpenAPI artifact on GitHub Releases

Tagged releases attach `openapi.json`, `openapi-admin.json`, and `openapi-internal.json` so you can download the contract without cloning the repo (workflow: `.github/workflows/release-openapi.yml`).

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

## Related

- Spec regeneration / drift CI: [`spec/README.md`](../spec/README.md) (`make openapi`, `make openapi-check`)
- Control-plane + runner setup: [`README.md`](../README.md), [`docs/quickstart.md`](quickstart.md)
