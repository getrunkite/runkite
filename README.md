# Runkite

**Self-hosted [Agent Protocol](https://github.com/langchain-ai/agent-protocol) control plane.**  
One Go binary. Framework-agnostic runners. Embedded Admin UI. Pluggable state & transport — Postgres + Redis for HA, with MySQL, MongoDB, NATS, Kafka, and more when you need them.

**Website:** [getrunkite.github.io/runkite](https://getrunkite.github.io/runkite/)

[![CI](https://github.com/getrunkite/runkite/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/getrunkite/runkite/actions/workflows/ci.yml?query=branch%3Amain)
[![Matrix](https://img.shields.io/badge/matrix-nightly-6F42C1)](https://github.com/getrunkite/runkite/actions/workflows/matrix.yml)
[![License: BUSL-1.1](https://img.shields.io/badge/license-BUSL--1.1-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)](go.mod)
[![Site](https://img.shields.io/badge/site-getrunkite.github.io-0c1210)](https://getrunkite.github.io/runkite/)

<p align="center">
  <img src="docs/assets/admin-walkthrough.gif" alt="Runkite Admin UI walkthrough" width="920" />
</p>

<p align="center">
  <b>Admin UI</b> — overview, agents, threads, runs (live SSE), connectors, cron, webhooks<br/>
  Open at <code>http://localhost:2026/admin/</code> after <code>runkite serve</code> or <code>runkite dev</code>
</p>

---

## Why Runkite

| | |
|---|---|
| **Agent Protocol, in Go** | HTTP / SSE / WebSocket, auth, streaming, job dispatch, persistence, connectors — one static binary |
| **Bring your framework** | LangGraph, CrewAI, LlamaIndex, AutoGen, LangChain, LangGraph.js over the same gRPC Runner Protocol |
| **Agent-to-agent delegation** | One agent calls another mid-run (`call_agent` / `callAgent`) — same Agent Protocol path, with depth limits, cancel cascade, and cost rollup |
| **Ops without a second deploy** | React Admin UI embedded via `embed.FS` — no Node runtime for end users |
| **Honest backends** | **Supported:** Postgres + Redis HA. **Also wired:** SQLite, MySQL, MongoDB, NATS, Kafka, pgvector / Qdrant / Weaviate / Pinecone — with documented tiers, not equal claims |

## Architecture

```mermaid
flowchart LR
  classDef client fill:#DBEAFE,stroke:#2563EB,color:#1E3A8A
  classDef cp fill:#BFDBFE,stroke:#1D4ED8,color:#1E3A8A
  classDef runner fill:#CCFBF1,stroke:#0F766E,color:#134E4A
  classDef store fill:#F1F5F9,stroke:#64748B,color:#334155

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

<p align="center">
  <img src="docs/assets/ecosystem.png" alt="How Runkite fits together: clients to control plane to runners, with pluggable state, transport, and vector backends" width="920" />
</p>

<p align="center"><b>Catalog of parts</b> — request path on top; state / transport / vectors plug in under the plane</p>

### One run on the HA profile

```mermaid
sequenceDiagram
  autonumber
  participant C as Client / SDK
  participant LB as Load balancer
  participant CP as CP replica
  participant PG as Postgres
  participant RD as Redis
  participant R as LangGraph runner

  Note over C,CP: Agent Protocol (HTTP / SSE / WebSocket)
  C->>LB: POST create thread + run
  LB->>CP: route to a replica
  CP->>PG: persist thread + run
  CP->>RD: enqueue job

  Note over CP,R: Runner Protocol (gRPC) — Runkite-defined
  R->>CP: GetJob
  CP->>RD: dequeue
  CP-->>R: RunAssignment (graph_id, input, config…)
  Note over R: Runner loads the graph and runs the agent / LLM turn
  R->>CP: StreamEvents
  CP->>RD: publish events (multi-replica fan-out)

  Note over C,CP: Agent Protocol again
  CP-->>C: SSE / WebSocket live output
```

### Agent Protocol vs Runner Protocol

**Creating threads, posting runs, streaming results** are [Agent Protocol](https://github.com/langchain-ai/agent-protocol) — a public, framework-agnostic client API. The control plane implements that surface.

**Runner Protocol is not that standard.** It is Runkite’s own internal contract ([`runner-protocol/PROTOCOL.md`](runner-protocol/PROTOCOL.md), gRPC in [`proto/runner.proto`](proto/runner.proto)): how the plane hands work to a worker process and how the worker streams events back. Clients never see it. Rough shape:

| Step | Who → whom | What |
|------|------------|------|
| `GetJob` | Runner → plane | Long-poll for the next assignment |
| `RunAssignment` | Plane → runner | `run_id`, `thread_id`, `graph_id`, `input`, `config`, auth context… |
| Execute | Runner only | Load LangGraph / CrewAI / … and run the agent (LLM calls live here) |
| `StreamEvents` | Runner → plane | Progress / messages / values / end |
| Cancel / HITL | Plane ↔ runner | Side channels on the same protocol |

So: Agent Protocol = “what the product client speaks.” Runner Protocol = “what our workers speak.” The plane translates between them, owns Postgres/Redis, and never imports LangGraph itself.

### Agent-to-agent delegation

An agent can call another agent as a sub-task mid-execution — not a separate wire protocol, the same create-run + wait path, reached from inside the runner via `POST /internal/a2a/runs`. Python `call_agent` and TypeScript `callAgent` forward the parent’s auth context; the plane enforces recursion depth, cascades cancel to children, and can roll up cost across the tree.

```mermaid
sequenceDiagram
  participant Coord as Coordinator agent
  participant CP as Control plane
  participant Work as Worker agent

  Note over Coord,CP: Parent run already executing on a runner
  Coord->>CP: call_agent / callAgent (internal A2A)
  CP->>CP: depth check · parent/root bookkeeping
  CP->>Work: enqueue child run (Runner Protocol)
  Work-->>CP: result
  CP-->>Coord: child output (wait=True)
```

Example: [`examples/a2a_agent/`](examples/a2a_agent/). Details: [docs/a2a.md](docs/a2a.md).

Backend tiers and HA notes: [docs/architecture.md](docs/architecture.md).

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
| [Deployment](docs/deployment.md) · [Factory graphs](docs/factory-graphs.md) · [Limitations](docs/limitations.md) (release summary + deep dive) · [Site](https://getrunkite.github.io/runkite/) · [All docs](docs/README.md) | Ops |

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
| [`gemini/`](examples/gemini/) | Real Gemini agents for every claimed runner (needs `.env.llm`) |

## Development

```bash
make test                    # SQLite + in-memory + protocol fixtures
make test-all                # All backends (needs infra-up)
make test-e2e                # Tier-0 black-box E2E
make test-matrix             # Framework × backend goldens (nightly CI)
make test-protocol-fixtures  # runner-protocol/examples schema + lifecycle
make test-protocol-execute   # execute_run → expected_events goldens (Python)
make test-llm-matrix         # live Gemini N×N (requires gitignored .env.llm)
make infra-up && make build && make lint
```

PR CI: unit / conformance / lint / Tier-0 e2e. Matrix: nightly + `workflow_dispatch`. Guide: [CONTRIBUTING.md](CONTRIBUTING.md) · [docs/deployment.md](docs/deployment.md).

## License

[Business Source License 1.1](LICENSE). Licensor: **Sharan Harsoor**.  
Use, modify, and self-host freely (including production). You may not offer Runkite as a hosted/managed service to third parties without a commercial license. Converts to Apache 2.0 four years after each release — see `LICENSE`.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: [SECURITY.md](SECURITY.md), not a public issue.
