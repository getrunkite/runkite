# runkite-runner

**TypeScript / LangGraph.js runner for [Runkite](https://github.com/getrunkite/runkite)** — the
self-hosted [Agent Protocol](https://github.com/langchain-ai/agent-protocol) control plane.

Same Runner Protocol as the [Python package on PyPI](https://pypi.org/project/runkite-runner/):
connect over gRPC, load graphs from `langgraph.json`, stream events and cancels,
optional Postgres checkpoints. `runner_kind`: `typescript-langgraphjs`.

| | |
|---|---|
| **Install** | `npm install -g runkite-runner` |
| **CLI** | `runkite-runner` |
| **Control plane** | [GitHub Releases](https://github.com/getrunkite/runkite/releases) · `ghcr.io/getrunkite/runkite` |
| **Docs** | [Runners](https://github.com/getrunkite/runkite/blob/main/docs/runners.md) · [Site](https://getrunkite.github.io/runkite/) |
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

## Links

- Homepage: https://getrunkite.github.io/runkite/
- Source: https://github.com/getrunkite/runkite/tree/main/typescript/runkite-runner
- Python twin: https://pypi.org/project/runkite-runner/
- Changelog: https://github.com/getrunkite/runkite/blob/main/CHANGELOG.md
- Limitations: https://github.com/getrunkite/runkite/blob/main/docs/limitations.md
