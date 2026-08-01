# Runkite

**Self-hosted [Agent Protocol](https://github.com/langchain-ai/agent-protocol) control plane.**  
One Go binary. Framework-agnostic runners. Embedded Admin UI. Pluggable state & transport — Postgres + Redis for HA, with MySQL, MongoDB, NATS, Kafka, and more when you need them.

[![CI](https://github.com/getrunkite/runkite/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/getrunkite/runkite/actions/workflows/ci.yml?query=branch%3Amain)
[![Matrix](https://img.shields.io/badge/matrix-nightly-6F42C1)](https://github.com/getrunkite/runkite/actions/workflows/matrix.yml)
[![License: BUSL-1.1](https://img.shields.io/badge/license-BUSL--1.1-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)](go.mod)

<p align="center">
  <img src="docs/assets/admin-walkthrough.gif" alt="Runkite Admin UI walkthrough" width="920" />
</p>

<p align="center">
  <b>Admin UI</b> — overview, agents, threads, runs (live SSE), connectors, cron, webhooks<br/>
  Open at <code>http://localhost:2026/admin/</code> after <code>runkite serve</code> or <code>runkite dev</code>
</p>

<p align="center">
  <img src="docs/assets/ecosystem.svg" alt="Runkite ecosystem map: clients, multi-replica control plane, runners, and pluggable state/transport/vector backends" width="920" />
</p>

<p align="center">
  <b>Ecosystem map</b> — solid boxes are shipped in-tree; dashed <code>+ …</code> boxes are extension points<br/>
  State · transport · vectors · frameworks — swap via env, or bring another backend behind the same interfaces
</p>

<p align="center">
  <img src="docs/assets/lifecycle.gif" alt="Animated run lifecycle: client to control plane to runner to streamed events" width="920" />
</p>

<p align="center">
  <b>Run lifecycle</b> — create run → enqueue → GetJob / RunAssignment → StreamEvents → live HTTP/SSE/WebSocket back to the client
</p>

---

## Why Runkite

| | |
|---|---|
| **Agent Protocol, in Go** | HTTP / SSE / WebSocket, auth, streaming, job dispatch, persistence, connectors — one static binary |
| **Bring your framework** | LangGraph, CrewAI, LlamaIndex, AutoGen, LangChain, LangGraph.js over the same gRPC Runner Protocol |
| **Ops without a second deploy** | React Admin UI embedded via `embed.FS` — no Node runtime for end users |
| **Honest backends** | **Supported:** Postgres + Redis HA. **Also wired:** SQLite, MySQL, MongoDB, NATS, Kafka, pgvector / Qdrant / Weaviate / Pinecone — with documented tiers, not equal claims |

## Architecture

```mermaid
flowchart LR
  classDef client fill:#1e3a8a,stroke:#60a5fa,color:#e2e8f0
  classDef cp fill:#1d4ed8,stroke:#93c5fd,color:#f8fafc
  classDef runner fill:#0f766e,stroke:#5eead4,color:#ecfdf5
  classDef store fill:#334155,stroke:#94a3b8,color:#e2e8f0

  C[Clients<br/>HTTP · SSE · WS]:::client
  CP[Control plane<br/>1..N replicas]:::cp
  R[Runners<br/>Python · TypeScript]:::runner
  S[(State)]:::store
  T[(Transport)]:::store
  V[(Vectors)]:::store

  C -->|Agent Protocol| CP
  CP -->|gRPC Runner Protocol| R
  CP --- S
  CP --- T
  CP --- V
```

```mermaid
sequenceDiagram
  box rgb(30,58,138) Client
    participant C as Client
  end
  box rgb(29,78,216) Control plane
    participant CP as Control Plane
  end
  box rgb(15,118,110) Runner
    participant R as Runner
  end
  C->>CP: Create thread + run
  R->>CP: GetJob
  CP-->>R: RunAssignment
  R->>CP: StreamEvents
  CP-->>C: SSE / WebSocket
```

Full backend tiers and HA notes: [docs/architecture.md](docs/architecture.md). Visual map: [`docs/assets/ecosystem.svg`](docs/assets/ecosystem.svg).

## Quick start

```bash
git clone https://github.com/getrunkite/runkite && cd runkite
make build
```

**Prerequisites:** Go 1.25+, Python 3.12+ (for agents), Docker optional.

**1 — Control plane** (zero-deps: SQLite + in-memory transport)

```bash
./runkite dev --config examples/echo_agent/langgraph.json
```

```
HTTP API:    http://localhost:2026
gRPC bridge: localhost:50051
Admin UI:    http://localhost:2026/admin/
Health:      http://localhost:2026/health
```

**2 — Runner**

```bash
PYTHONPATH=python python -m runkite_runner --config examples/echo_agent/langgraph.json
```

TypeScript / LangGraph.js: see [docs/runners.md](docs/runners.md).

**3 — Create a run**

```python
from langgraph_sdk import get_client

client = get_client(url="http://localhost:2026")
thread = await client.threads.create()
async for event in client.runs.stream(
    thread["thread_id"],
    "echo_agent",
    input={"messages": [{"role": "user", "content": "hello"}]},
):
    print(event.event, event.data)
```

Full walkthrough: [docs/quickstart.md](docs/quickstart.md) · Client access: [docs/client-sdk.md](docs/client-sdk.md)

```
runkite dev             Dev mode (SQLite + in-memory)
runkite serve           Production mode
runkite db upgrade      Migrations
runkite agents list     Agents from config
runkite version         Version info
```

## Backend support tiers

| Tier | Meaning |
|------|---------|
| **Supported** | Multi-replica HA, Helm defaults, primary CI focus |
| **Compatible** | Same conformance suite; known gaps / thinner soak evidence |
| **Experimental** | Single-instance or incomplete multi-replica primitives |

| Concern | Supported | Compatible | Experimental |
|---------|-----------|------------|--------------|
| **State** | **Postgres** | SQLite, MySQL, MongoDB (replica set) | — |
| **Transport** | **Redis** | NATS/JetStream, in-process | Kafka queue without Redis |
| **Mixed** | — | Kafka queue **+ Redis** broker/cancel | — |
| **Vectors** | **pgvector** | Qdrant, Weaviate, Pinecone | — |

Production default: `POSTGRES_DSN` + `REDIS_URL` ([Helm](deploy/helm/runkite), `docker-compose.multi.yml`). Details: [docs/architecture.md](docs/architecture.md).

## Documentation

| | |
|---|---|
| [Quick start](docs/quickstart.md) · [Client SDK](docs/client-sdk.md) · [Configuration](docs/configuration.md) · [Auth / TLS / tenants](docs/auth.md) | Getting started |
| [Admin UI](docs/admin.md) · [Runners](docs/runners.md) · [Architecture](docs/architecture.md) · [API](docs/api.md) | Core |
| [Connectors](docs/connectors.md) · [A2A](docs/a2a.md) · [MCP](docs/mcp-server.md) · [Registry](docs/registry.md) · [Vectors](docs/vector-store.md) | Features |
| [Deployment](docs/deployment.md) · [Factory graphs](docs/factory-graphs.md) · [Limitations](docs/limitations.md) · [All docs](docs/README.md) | Ops |

## Examples

| Example | What it proves |
|---------|----------------|
| [`echo_agent`](examples/echo_agent/) | Minimal bridge |
| [`react_agent`](examples/react_agent/) | Tool calls (fake LLM) |
| [`approval_agent`](examples/approval_agent/) | HITL interrupt / resume |
| [`slow_agent`](examples/slow_agent/) | Streaming + cancel |
| [`a2a_agent`](examples/a2a_agent/) | Agent-to-agent delegation |
| [`echo_agent_ts`](examples/echo_agent_ts/) | TypeScript / LangGraph.js |
| [`vector_agent`](examples/vector_agent/) | Vector store |
| [`factory_agent`](examples/factory_agent/) | Per-request factory graphs |
| [`cron_agent`](examples/cron_agent/) · [`store_agent`](examples/store_agent/) · [`custom_routes_agent`](examples/custom_routes_agent/) | Cron, store, custom routes |

## Development

```bash
make test                    # SQLite + in-memory + protocol fixtures
make test-all                # All backends (needs infra-up)
make test-e2e                # Tier-0 black-box E2E
make test-matrix             # Framework × backend goldens (nightly CI)
make test-protocol-fixtures  # runner-protocol/examples invariants
make infra-up && make build && make lint
```

PR CI: unit / conformance / lint / Tier-0 e2e. Matrix: nightly + `workflow_dispatch`. Guide: [CONTRIBUTING.md](CONTRIBUTING.md) · [docs/deployment.md](docs/deployment.md).

## License

[Business Source License 1.1](LICENSE). Licensor: **Sharan Harsoor**.  
Use, modify, and self-host freely (including production). You may not offer Runkite as a hosted/managed service to third parties without a commercial license. Converts to Apache 2.0 four years after each release — see `LICENSE`.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: [SECURITY.md](SECURITY.md), not a public issue.
