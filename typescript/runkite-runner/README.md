# runkite-runner

**TypeScript runner for [Runkite](https://github.com/getrunkite/runkite)** supports
LangGraph.js and optional AI SDK agents through the self-hosted
[Agent Protocol](https://github.com/langchain-ai/agent-protocol) control plane.

Same Runner Protocol as the [Python package on PyPI](https://pypi.org/project/runkite-runner/):
connect over gRPC, load graphs from `langgraph.json`, stream events and cancels,
optional Postgres checkpoints. `runner_kind`: `typescript-langgraphjs`.

| | |
|---|---|
| **Install** | `npm install -g runkite-runner` |
| **CLI** | `runkite-runner` |
| **Control plane** | [GitHub Releases](https://github.com/getrunkite/runkite/releases) · `ghcr.io/getrunkite/runkite` |
| **Docs** | [Runners](https://github.com/getrunkite/runkite/blob/main/docs/runners.md) · [Site](https://getrunkite.com/) |
| **License** | [BUSL-1.1](./LICENSE) |

## Quick start

```bash
npm install -g runkite-runner

runkite-runner \
  --config path/to/langgraph.json \
  --grpc-address 127.0.0.1:50051 \
  --http-address http://127.0.0.1:2026
```

From a clone: `npm ci && npm run build` then `npx runkite-runner --config …`.

| Variable | Purpose |
|----------|---------|
| `POSTGRES_DSN` | Direct-mode LangGraph.js checkpoints + store |
| `RUNNER_TOKEN` | Authenticate to the control plane |
| `RUNKITE_CONCURRENCY` | Concurrent jobs per process (default `1`) |
| `OTEL_EXPORTER_OTLP_*` | Optional OpenTelemetry export |

## Features

- Dynamic `.ts` graph loading via `tsx` (no separate build step for agent code)
- Streaming, cancel (`WatchCancels`), interrupt / resume
- Postgres checkpointer when `POSTGRES_DSN` is set
- Thin OTel under the control plane’s `traceparent`

## Managed Vercel AI SDK agents

This checkout adds an optional AI SDK backend alongside LangGraph.js. Install
`ai@7.0.155` in the same application as the updated runner package. Select it
with `--framework ai-sdk`; the default runner kind is `typescript-ai-sdk`.
Existing published releases need a maintainer release containing this backend.

```sh
# From this checkout
npm ci
npm run build
node dist/cli.js --framework ai-sdk --config path/to/langgraph.json \
  --grpc-address localhost:50051 --http-address http://localhost:2026
```

The config uses `runner_kind: "typescript-ai-sdk"` and the existing `graphs`
mapping. Each export is a per-run factory returning `AiSdkAgentDefinition`
settings, imported from `runkite-runner/ai-sdk`. The runner constructs the native
`ToolLoopAgent`; the Go plane dispatches it through the existing Runner Protocol.
It records real managed threads/runs and reports native provider token usage.

Native model/tool message history persists through the plane's run-bound opaque
checkpoint API, using TLS, tenant/kind credentials, generation headers and CAS.
Success and UI finish follow the confirmed state commit. A committed turn can be
replayed after reclaim without invoking its model or tools again. A crash before
that commit can still repeat an external side effect; use downstream idempotency.

Initial support is new text user turns and local tools with JSON results.
Native approval/resume, provider/client-executed tools, streaming tool outputs,
binary attachments, time travel and whole-run caching are unsupported. A fresh
turn uses the last successfully committed history; interrupted partial turns
are not resumable. On cancellation the adapter emits known completed-call usage
or an explicit unmetered marker. The plane can settle cancellation before those
trailing events arrive, so complete canceled-run accounting is not guaranteed.
Cancellation remains cooperative after a tool starts. Native tool execution uses `allowed_tools`
when provided, but connector grants still require the actual connector path.

The native checkpoint is bounded by the plane's 16 MiB limit. This backend also
limits the per-turn UI receipt to 8 MiB and final output to 3 MiB so it fits the
existing gRPC message envelope. Oversized turns fail instead of committing an
output that cannot be delivered or replayed.

See the [managed AI SDK example](../../examples/managed_ai_sdk/README.md) for a
working offline factory and an application-owned `useChat` stream bridge. That
bridge keeps browser authentication and server credentials in your app.

## Links

- Homepage: https://getrunkite.com/
- Source: https://github.com/getrunkite/runkite/tree/main/typescript/runkite-runner
- Python twin: https://pypi.org/project/runkite-runner/
- Changelog: https://github.com/getrunkite/runkite/blob/main/CHANGELOG.md
- Limitations: https://github.com/getrunkite/runkite/blob/main/docs/limitations.md
