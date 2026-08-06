# runkite-runner (Python)

Python runner for **[Runkite](https://github.com/getrunkite/runkite)** — the self-hosted
[Agent Protocol](https://github.com/langchain-ai/agent-protocol) control plane.

Connects to Runkite over gRPC, loads agents from `langgraph.json`, executes them
(LangGraph by default; CrewAI / LlamaIndex / AutoGen / plain LangChain via
in-repo adapters), and streams events, status, and cancels back to the plane.

| | |
|---|---|
| **Install** | `pip install runkite-runner` |
| **CLI** | `runkite-runner` or `python -m runkite_runner` |
| **Site** | https://getrunkite.github.io/runkite/ |
| **Docs** | [Runners](https://github.com/getrunkite/runkite/blob/main/docs/runners.md) · [Quick start](https://github.com/getrunkite/runkite/blob/main/docs/quickstart.md) |
| **License** | [BUSL-1.1](https://github.com/getrunkite/runkite/blob/main/LICENSE) |

## Quick start

You need a running control plane (`runkite serve` / `runkite dev`, or the
published Docker image). Then:

```bash
pip install runkite-runner

runkite-runner \
  --config path/to/langgraph.json \
  --grpc-address 127.0.0.1:50051 \
  --http-address http://127.0.0.1:2026
```

Common env vars (same as the TypeScript runner):

| Variable | Purpose |
|----------|---------|
| `POSTGRES_DSN` | Direct-mode LangGraph checkpoints + store (durable) |
| `RUNNER_TOKEN` / kind token | Authenticate to the control plane |
| `RUNKITE_CONCURRENCY` | Concurrent jobs per process (default `1`) |
| `OTEL_EXPORTER_OTLP_*` | Optional OpenTelemetry export |

## What you get

- **LangGraph** execution with streaming, cancel, and HITL interrupt/resume
- **Durable checkpoints** when `POSTGRES_DSN` is set (MemorySaver otherwise)
- **Store / vector** dual-mode against the control plane
- **OTel** `runkite.run` + LLM/tool child spans when OTLP is configured
- Shared **generic worker** used by framework adapters in the Runkite repo
  (`python/adapters/*` — install those from source; not published as separate wheels yet)

## Related packages & artifacts

| Artifact | Where |
|----------|--------|
| Control plane binary | [GitHub Releases](https://github.com/getrunkite/runkite/releases) |
| Control plane image | `ghcr.io/getrunkite/runkite` |
| TypeScript / LangGraph.js runner | [`runkite-runner` on npm](https://www.npmjs.com/package/runkite-runner) |
| Helm chart | [`deploy/helm/runkite`](https://github.com/getrunkite/runkite/tree/main/deploy/helm/runkite) |

## Links

- Homepage: https://getrunkite.github.io/runkite/
- Source: https://github.com/getrunkite/runkite/tree/main/python
- Issues: https://github.com/getrunkite/runkite/issues
- Changelog: https://github.com/getrunkite/runkite/blob/main/CHANGELOG.md
- Limitations (honest gaps): https://github.com/getrunkite/runkite/blob/main/docs/limitations.md
