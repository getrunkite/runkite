# runkite-runner

**Python runner for [Runkite](https://github.com/getrunkite/runkite)**, a self-hosted
[Agent Protocol](https://github.com/langchain-ai/agent-protocol) control plane.

This package is the process that **does the work**: it connects to a Runkite
control plane over gRPC, claims jobs, loads your agent from `langgraph.json`,
executes its framework, and streams events, heartbeats, and terminal status back
so clients see live SSE / WebSocket progress in the Admin UI.

The default CLI runs LangGraph. An optional Microsoft Agent Framework profile
is included in source-built wheels from this checkout. That packaging change
is **unreleased**; the published PyPI `0.5.8` package does not contain it.

```text
  Client / SDK ──HTTP/SSE──► Runkite control plane (Go)
                                    │ gRPC Runner Protocol
                                    ▼
                           runkite-runner (this package)
                                    │
                              your framework agent
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

### Google ADK (unreleased)

The source-built optional `google-adk` extra pins `google-adk==2.13.0` and
installs `google_adk_adapter` plus the `runkite-runner-google-adk` command:

```bash
python3.12 -m venv .venv-google-adk
.venv-google-adk/bin/python -m pip install './python[google-adk]'
.venv-google-adk/bin/runkite-runner-google-adk \
  --config examples/managed_google_adk/langgraph.json \
  --grpc-address 127.0.0.1:50051 \
  --http-address http://127.0.0.1:2026
```

The installed module alternative is `python -m google_adk_adapter`. It runs one
native text `LlmAgent`, ordinary local tools and session-local state, with
successful-turn CAS receipts. Cross-session state, workflows and native
confirmation/resume are outside this profile. Published PyPI `0.5.8` lacks it;
the default CLI remains LangGraph and the stock image does not install this SDK.
See the [example](https://github.com/getrunkite/runkite/tree/main/examples/managed_google_adk)
for authentication, input and tests.

### Pydantic AI (unreleased)

The source-built optional `pydantic-ai` extra pins `pydantic-ai-slim==2.63.0`.
It installs `pydantic_ai_adapter` and the `runkite-runner-pydantic-ai` command;
the module alternative is `python -m pydantic_ai_adapter`. Install your chosen
model provider separately. From the repository root:

```bash
python3.12 -m venv .venv-pydantic-ai
.venv-pydantic-ai/bin/python -m pip install './python[pydantic-ai]'
.venv-pydantic-ai/bin/runkite-runner-pydantic-ai \
  --config examples/managed_pydantic_ai/langgraph.json \
  --grpc-address 127.0.0.1:50051 \
  --http-address http://127.0.0.1:2026
```

The profile supports native text agents, validated JSON results, conversation
history and committed-turn recovery. Published PyPI `0.5.8` does not include it.
The stock image does not install this SDK, and the default CLI remains LangGraph.
See the [example](https://github.com/getrunkite/runkite/tree/main/examples/managed_pydantic_ai)
for the factory contract and test commands.

### Microsoft Agent Framework (unreleased)

The optional profile ships in the same `runkite-runner` wheel and sdist, with a
separate entrypoint. Its `agent-framework` extra installs the tested
`agent-framework-core==1.23.0`; the ordinary install does not pull in that SDK.
Until a release containing this change is published, build and install from
the repository root in a clean environment:

```bash
python3.12 -m venv .venv-agent-framework
.venv-agent-framework/bin/python -m pip install './python[agent-framework]'
.venv-agent-framework/bin/runkite-runner-agent-framework \
  --config examples/managed_agent_framework/langgraph.json \
  --grpc-address 127.0.0.1:50051 \
  --http-address http://127.0.0.1:2026
```

Python 3.11+ remains supported; the example uses Python 3.12. Once a release
containing this change is available, `pip install 'runkite-runner[agent-framework]'`
will provide the same profile from PyPI. Do not use that command with the older
published `0.5.8` artifact.

`python -m agent_framework_adapter` is the installed module alternative.
`runkite-runner` and `python -m runkite_runner` retain their LangGraph defaults.
The plane must load the same `runner_kind: python-agent-framework` config.
Microsoft sessions require the plane's HTTP opaque checkpoint API and the same
`RUNNER_TOKEN` as gRPC. See the
[adapter README](https://github.com/getrunkite/runkite/blob/main/python/adapters/agent_framework_adapter/README.md) for native factory,
input and recovery boundaries. The stock Python runner image installs `[otel]`
and starts LangGraph; use your own environment or image with the Microsoft extra
and dedicated entrypoint.

### OpenAI Agents SDK (unreleased)

The same source-built wheel includes `openai_agents_adapter` and the dedicated
`runkite-runner-openai-agents` command. The optional `openai-agents` extra pins
`openai-agents==0.24.0`; the base installation does not install that SDK.
Until a release contains it, install the local package in a clean environment:

```bash
python3.12 -m venv .venv-openai-agents
.venv-openai-agents/bin/python -m pip install './python[openai-agents]'
.venv-openai-agents/bin/runkite-runner-openai-agents \
  --config examples/managed_openai_agents/langgraph.json \
  --grpc-address localhost:50051 \
  --http-address http://localhost:2026
```

`python -m openai_agents_adapter` is the installed module alternative. No source
`PYTHONPATH` is required. The published PyPI `0.5.8` artifact does not contain
this profile. After a release includes it, use
`pip install 'runkite-runner[openai-agents]'`. Python 3.11+ remains supported;
the example uses 3.12. The ordinary CLI and stock image keep their LangGraph
defaults; the image does not install this optional SDK.

Factories return a native `agents.Agent`. Successful-turn history, reasoning
items and committed receipts persist through the plane's HTTP checkpoint API.
The first profile supports text and local function tools, with explicit rejection
of unsupported native approval/resume, handoffs, hosted tools and structured final
outputs. Native SDK trace export is disabled; Runkite's existing OTLP run tracing
remains available. See the
[offline example](https://github.com/getrunkite/runkite/tree/main/examples/managed_openai_agents)
and [adapter contract](https://github.com/getrunkite/runkite/blob/main/python/adapters/openai_agents_adapter/README.md).

### Framework adapters from a checkout

CrewAI, LlamaIndex, AutoGen and plain LangChain adapters live under
`python/adapters/` in the source tree and share `runkite_runner.generic_worker`.
They are not included by `pip install runkite-runner` and are not separate PyPI
packages. Install the
chosen adapter from a clone with its isolated dependencies. See
[docs/runners.md](https://github.com/getrunkite/runkite/blob/main/docs/runners.md).

The managed Vercel AI SDK runner is a TypeScript backend, not a Python CLI mode,
and ships in the npm `runkite-runner` package from `v0.5.8`.
Microsoft Agent Framework also has checkout-only function governance
middleware in `python/runkite_governance`. That separate helper
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
| `POSTGRES_DSN` | LangGraph direct-mode checkpoints + store pool; Microsoft sessions use HTTP opaque checkpoints |
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
