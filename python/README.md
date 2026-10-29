# runkite-runner

**Python runner for [Runkite](https://github.com/getrunkite/runkite)**, a self-hosted
[Agent Protocol](https://github.com/langchain-ai/agent-protocol) control plane.

This package is the process that **does the work**: it connects to a Runkite
control plane over gRPC, claims jobs, loads your agent from `langgraph.json`,
executes LangGraph, and streams events, heartbeats, and terminal status back
so clients see live SSE / WebSocket progress in the Admin UI.

```text
  Client / SDK ──HTTP/SSE──► Runkite control plane (Go)
                                    │ gRPC Runner Protocol
                                    ▼
                           runkite-runner (this package)
                                    │
                              your LangGraph agent
```

| | |
|---|---|
| **PyPI** | `pip install runkite-runner` |
| **CLI** | `runkite-runner` · `python -m runkite_runner` |
| **Companion (TS)** | [`runkite-runner` on npm](https://www.npmjs.com/package/runkite-runner) |
| **Control plane** | [GitHub Releases](https://github.com/getrunkite/runkite/releases) · `ghcr.io/getrunkite/runkite` |
| **Docs** | [Runners](https://github.com/getrunkite/runkite/blob/main/docs/runners.md) · [Quick start](https://github.com/getrunkite/runkite/blob/main/docs/quickstart.md) · [Site](https://getrunkite.com/) |
| **License** | [BUSL-1.1](https://github.com/getrunkite/runkite/blob/main/LICENSE) |

---

## Why a separate runner?

Runkite keeps the **control plane** framework-agnostic (auth, threads, runs,
queues, Admin UI). Framework-specific execution lives in a **runner** so you can
scale workers independently, mix Python and TypeScript agents on one plane, and
upgrade LangGraph without redeploying the Go binary.

---

## Install

```bash
pip install runkite-runner
```

Requires **Python 3.11+**. Pulls in LangGraph, gRPC, checkpoint Postgres support,
httpx, and uvicorn (see `pyproject.toml`). Tracing is optional:

```bash
pip install 'runkite-runner[otel]'
```

Without the extra, the runner still starts; OTEL_* env vars log once and stay no-op until the packages are installed.

---

## Quick start

**1. Control plane** (pick one):

```bash
# Docker
docker pull ghcr.io/getrunkite/runkite:latest
docker run --rm -p 2026:2026 -p 50051:50051 \
  -e RUNKITE_ALLOW_INSECURE_SERVE=1 \
  ghcr.io/getrunkite/runkite:latest \
  dev --config /app/examples/echo_agent/langgraph.json

# or binary from https://github.com/getrunkite/runkite/releases
```

**2. Runner** (this package):

```bash
runkite-runner \
  --config path/to/langgraph.json \
  --grpc-address 127.0.0.1:50051 \
  --http-address http://127.0.0.1:2026
```

**3. Admin UI:** http://127.0.0.1:2026/admin/

Minimal `langgraph.json`:

```json
{
  "graphs": {
    "echo_agent": "./graph.py:graph"
  },
  "dependencies": ["."]
}
```

Examples live in the [Runkite repo](https://github.com/getrunkite/runkite/tree/main/examples)
(`echo_agent`, `react_agent`, `approval_agent`, …).

---

## Features

- **LangGraph** graphs with streaming chunks, cancel, and HITL interrupt/resume
- **Durable checkpoints** via `AsyncPostgresSaver` when `POSTGRES_DSN` points at the control plane's Postgres database; otherwise via the HTTP checkpoint proxy when an HTTP address is configured. `MemorySaver` is the ephemeral fallback when neither is available. An explicitly blank `RUNKITE_HTTP_URL` selects that local fallback.
- **Store / vector** dual-mode against the control plane's APIs
- **Concurrency:** `--concurrency N` / `RUNKITE_CONCURRENCY` for overlapping I/O-bound jobs
- **OpenTelemetry:** `runkite.run` span under the control plane `traceparent`, plus thin `runkite.llm` / `runkite.tool` children when OTLP env is set
- **Production auth:** runner kind tokens (`RUNNER_TOKEN` / `RUNNER_TOKEN_*`) matching control-plane config

### Framework adapters from a checkout

The published Python package and its CLI run LangGraph. CrewAI, LlamaIndex,
AutoGen and plain LangChain adapters live under `python/adapters/` in the source
tree and share `runkite_runner.generic_worker`. They are not included by
`pip install runkite-runner` and are not separate PyPI packages. Install the
chosen adapter from a clone with its isolated dependencies. See
[docs/runners.md](https://github.com/getrunkite/runkite/blob/main/docs/runners.md).

The managed Vercel AI SDK runner in the working tree is a TypeScript backend,
not a Python CLI mode, and requires an updated checkout build until released.
Microsoft Agent Framework currently has checkout-only function governance
middleware in `python/runkite_governance`, not a managed runner. That helper
keeps the model loop and pending continuation in the app and creates no Runkite
thread or run. See the
[SDK integration guide](https://getrunkite.com/support/sdk-integration.html).

---

## Configuration

| Flag / env | Purpose |
|------------|---------|
| `--config` | Path to `langgraph.json` |
| `--grpc-address` | Control plane gRPC (default from env / localhost) |
| `--http-address` / `RUNKITE_HTTP_URL` | Control plane HTTP for checkpoint/store/connector proxy helpers |
| `--concurrency` / `RUNKITE_CONCURRENCY` | Max in-flight jobs (default `1`) |
| `POSTGRES_DSN` | Direct-mode checkpoints + store pool |
| `RUNNER_TOKEN` | Shared secret presented on gRPC / `/internal/*` |
| `OTEL_EXPORTER_OTLP_*` | Enable tracing export (same contract as the control plane) |
| `LOG_LEVEL` / `LOG_FORMAT` | Logging (`info`, `debug`, … · `text` / `json`) |

---

## Related artifacts

| Artifact | Install |
|----------|---------|
| Control plane binary | [GitHub Releases](https://github.com/getrunkite/runkite/releases) |
| Control plane image | `docker pull ghcr.io/getrunkite/runkite:latest` |
| Python runner image | `docker pull ghcr.io/getrunkite/runkite-runner:latest` |
| TypeScript runner | `npm install -g runkite-runner` |
| Helm | [`deploy/helm/runkite`](https://github.com/getrunkite/runkite/tree/main/deploy/helm/runkite) |

---

## Links

- Homepage: https://getrunkite.com/
- Source: https://github.com/getrunkite/runkite/tree/main/python
- Issues: https://github.com/getrunkite/runkite/issues
- Changelog: https://github.com/getrunkite/runkite/blob/main/CHANGELOG.md
- Known limitations: https://github.com/getrunkite/runkite/blob/main/docs/limitations.md
- Security: https://github.com/getrunkite/runkite/blob/main/SECURITY.md
