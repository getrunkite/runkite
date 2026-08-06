# runkite-runner (TypeScript)

TypeScript / **LangGraph.js** runner for **[Runkite](https://github.com/getrunkite/runkite)** —
the self-hosted [Agent Protocol](https://github.com/langchain-ai/agent-protocol) control plane.

Same Runner Protocol as the Python package: connect over gRPC, load graphs from
`langgraph.json`, stream events and cancels, optional Postgres checkpoints.

| | |
|---|---|
| **Install** | `npm install -g runkite-runner` |
| **CLI** | `runkite-runner` |
| **Site** | https://getrunkite.github.io/runkite/ |
| **Docs** | [Runners](https://github.com/getrunkite/runkite/blob/main/docs/runners.md) · [Quick start](https://github.com/getrunkite/runkite/blob/main/docs/quickstart.md) |
| **License** | [BUSL-1.1](./LICENSE) |

## Quick start

You need a running control plane (`runkite serve` / `runkite dev`, or Docker). Then:

```bash
npm install -g runkite-runner

runkite-runner \
  --config path/to/langgraph.json \
  --grpc-address 127.0.0.1:50051 \
  --http-address http://127.0.0.1:2026
```

Local / from a clone (no global install):

```bash
cd typescript/runkite-runner && npm ci && npm run build
npx runkite-runner --config ../../examples/echo_agent_ts/langgraph.json \
  --grpc-address 127.0.0.1:50051
```

| Variable | Purpose |
|----------|---------|
| `POSTGRES_DSN` | Direct-mode LangGraph.js checkpoints + store |
| `RUNNER_TOKEN` / kind token | Authenticate to the control plane |
| `RUNKITE_CONCURRENCY` | Concurrent jobs per process (default `1`) |
| `OTEL_EXPORTER_OTLP_*` | Optional OpenTelemetry export |

`runner_kind` for this package: `typescript-langgraphjs`.

## What you get

- Dynamic `.ts` graph loading via `tsx` (no separate build step for agent code)
- Streaming, cancel (`WatchCancels`), interrupt / resume
- Postgres checkpointer when `POSTGRES_DSN` is set
- Thin OTel wiring under the control plane’s `traceparent`

## Related packages & artifacts

| Artifact | Where |
|----------|--------|
| Control plane binary | [GitHub Releases](https://github.com/getrunkite/runkite/releases) |
| Control plane image | `ghcr.io/getrunkite/runkite` |
| Python / LangGraph runner | [`runkite-runner` on PyPI](https://pypi.org/project/runkite-runner/) |
| Helm chart | [`deploy/helm/runkite`](https://github.com/getrunkite/runkite/tree/main/deploy/helm/runkite) |

## Links

- Homepage: https://getrunkite.github.io/runkite/
- Source: https://github.com/getrunkite/runkite/tree/main/typescript/runkite-runner
- Issues: https://github.com/getrunkite/runkite/issues
- Changelog: https://github.com/getrunkite/runkite/blob/main/CHANGELOG.md
- Limitations: https://github.com/getrunkite/runkite/blob/main/docs/limitations.md
