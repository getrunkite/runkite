# Runkite

**A self-hosted control plane for AI agents. One Go binary handles auth, dispatch, connectors, human approval, spend and audit while your agent keeps its framework and model loop.**

For managed execution, the plane dispatches agents to separate framework runners.
Apps that keep execution in their own backend can ask the same plane for an
individual tool decision. These paths have different visibility and controls;
see [Choose an integration path](#choose-an-integration-path).

[![CI](https://github.com/getrunkite/runkite/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/getrunkite/runkite/actions/workflows/ci.yml?query=branch%3Amain)
[![Matrix](https://img.shields.io/badge/matrix-nightly-6F42C1)](https://github.com/getrunkite/runkite/actions/workflows/matrix.yml)
[![License: BUSL-1.1](https://img.shields.io/badge/license-BUSL--1.1-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](go.mod)
[![PyPI](https://img.shields.io/pypi/v/runkite-runner)](https://pypi.org/project/runkite-runner/)
[![npm](https://img.shields.io/npm/v/runkite-runner)](https://www.npmjs.com/package/runkite-runner)
[![GHCR](https://img.shields.io/badge/GHCR-getrunkite%2Frunkite-blue)](https://github.com/getrunkite/runkite/pkgs/container/runkite)
[![Site](https://img.shields.io/badge/site-getrunkite.com-0c1210)](https://getrunkite.com/)

**Website:** [getrunkite.com](https://getrunkite.com/) · **Live Admin:** [getrunkite.com/admin](https://getrunkite.com/admin/) · **Releases:** [GitHub Releases](https://github.com/getrunkite/runkite/releases) · **Python runner:** [PyPI](https://pypi.org/project/runkite-runner/) · **TypeScript runner:** [npm](https://www.npmjs.com/package/runkite-runner)

**Try it live, no signup:** [getrunkite.com](https://getrunkite.com/). Open Admin with the shared key on that page. Fake agents. Your graphs stay on your infra.

<p align="center">
  <img src="docs/assets/admin-walkthrough.gif" alt="Runkite Admin UI walkthrough" width="920" />
</p>

<p align="center">
  <b>Admin UI</b> — ops + SQL governance (grants, HITL, kill, audit), embedded in the binary<br/>
  <code>http://localhost:2026/admin/</code> after <code>runkite serve</code> or <code>runkite dev</code>
</p>

---

### Contents

[Why Runkite](#why-runkite) · [What it is not](#what-runkite-is-not) · [Quick start](#quick-start) · [Architecture](#architecture) · [Documentation](#documentation) · [Development](#development) · [License](#license) · [Contributing](#contributing)

---

## Why Runkite

The problem Runkite exists for: one agent, one `.env`, one deploy is easy. A second agent that needs the same GitHub token, a third that needs a human to approve a write before it happens, and someone asking "who called Salesforce last Tuesday" is where most teams end up rebuilding the same operational plumbing — credentials, admission control, crash recovery, kill switches, spend caps, audit — separately, per agent, per framework. Runkite is that plumbing, built once, on a plane your agent code talks to instead of reimplementing.

| Capability | What it means in practice |
|---|---|
| **Agent Protocol, in Go** | HTTP / SSE / WebSocket, auth, streaming, job dispatch, persistence, connectors — one static binary, no runtime dependencies for the control plane itself |
| **Bring your framework** | Existing LangGraph, LangGraph.js, CrewAI, LlamaIndex, AutoGen and LangChain runners use one gRPC Runner Protocol. The optional AI SDK managed runner ships in `runkite-runner` with `--framework ai-sdk`. Framework support and package availability are listed in [Runners](docs/runners.md). |
| **Governance lives on the plane, not just in the agent** | Fail-closed connector grants, optional argument predicates on MCP `tools/call`, durable policy audit, connector human-in-the-loop approval, kill/pause switches, and time-boxed break-glass bypass — all with their own Admin UI pages, all provable with `make smoke-governance`. Details: [Trust & governance](docs/trust-governance.md) · [Admin UI](docs/admin.md) |
| **Agent-to-agent delegation** | One agent calls another mid-run (`call_agent` / `callAgent`), with depth limits and parent/root bookkeeping enforced by the plane, not left to each agent to get right — [docs/a2a.md](docs/a2a.md) |
| **Ops without a second deploy** | Admin is a static React app embedded with Go's `embed.FS`. The control plane needs no separate frontend server or Node.js runtime. Framework runners retain their own Python or Node.js runtime. |
| **Spend governance, not a billing platform** | Managed-run admission checks tenant and agent daily caps against recorded usage and configured reservations. Estimated USD, concurrent spending and cancellation accounting have explicit [limits](docs/limitations.md). |
| **Honest about backend maturity** | **Supported:** Postgres + Redis HA — this is the profile to run in production today. Also wired and tested, at varying maturity: SQLite, MySQL, MongoDB, NATS, Kafka, and several vector stores. See the real [tier breakdown](docs/architecture.md#backend-support-tiers) rather than a marketing claim that everything is equally production-ready |

## What Runkite is not

Being clear about scope matters as much as being clear about capability:

- **Not another agent framework.** Runkite doesn't tell your agent how to think, plan, or call tools — LangGraph, CrewAI, and the rest still do that. Runkite governs what happens around the agent: dispatch, credentials, approval, spend, audit.
- **Not a managed LLM gateway.** Your agent still calls its model provider directly (or through whatever gateway you already use). Runkite doesn't sit in that request path by default.
- **Not a billing company.** FinOps here means budget caps and an honestly-labeled cost *estimate* enforced on the plane — not invoicing, payment processing, or a Stripe integration.
- **Self-hosted only.** You operate the control plane and its state and transport backends on your own infrastructure.

## Quick start

The fastest way to see it, with nothing installed: [getrunkite.com](https://getrunkite.com/) (live Admin, shared key on the page). To run the same path on your own infra:

| Piece | Get it |
|---|---|
| Control plane | [GitHub Releases](https://github.com/getrunkite/runkite/releases) · `docker pull ghcr.io/getrunkite/runkite:latest` · or `make build` |
| Python runner | `pip install runkite-runner` |
| TypeScript runner | `npm install -g runkite-runner` |
| Helm chart | [`deploy/helm/runkite`](deploy/helm/runkite) |

```bash
git clone https://github.com/getrunkite/runkite && cd runkite && make build

# terminal 1 — zero dependencies (SQLite + in-process transport)
./runkite dev --config examples/echo_agent/langgraph.json

# terminal 2
pip install runkite-runner
runkite-runner --config examples/echo_agent/langgraph.json \
  --grpc-address 127.0.0.1:50051 --http-address http://127.0.0.1:2026
```

Admin: http://localhost:2026/admin/ · Health: http://localhost:2026/health

The full walkthrough — streaming with the client SDK, running against Postgres + Redis instead of the zero-dependency profile, and turning on auth — is in [docs/quickstart.md](docs/quickstart.md) and [docs/client-sdk.md](docs/client-sdk.md). More runnable examples live under [`examples/`](examples/) (`echo_agent`, `pending_agent`, `approval_agent`, `a2a_agent`, `policy_webhook`, `sim`, and more).

## Choose an integration path

**Runkite owns execution:** an Agent Protocol client creates a thread and run.
The plane dispatches a runner, records its output and streams results to the app.
These executions appear in Admin Threads and Runs and use managed-run admission,
cancel and usage accounting. Your UI and user authentication can stay in the app.
Model calls execute in the runner, not inside the Go binary.

**Your app owns execution:** a tool calls `POST /governance/decide`, then the app
executes only after allow or continues its own saved operation after approval.
This creates no Runkite thread or run. Run kill switches, break-glass and spend
caps do not apply. `run_id` is an app operation correlation value, not execution
idempotency or proof that the tool ran.

The [AI SDK managed example](examples/managed_ai_sdk/README.md) and `useChat`
bridge ship in **v0.5.8** with `runkite-runner` (`--framework ai-sdk`, peer
`ai@7.0.155`). The app-owned
[AI SDK guard](examples/governance_ai_sdk/README.md) and
[Microsoft function middleware](examples/governance_agent_framework/README.md)
use the unpublished `runkite-governance` helpers (install from the repository).
A [Microsoft managed runner](examples/managed_agent_framework/README.md) is
available from a source-built Python wheel with the optional `agent-framework`
extra and `runkite-runner-agent-framework` CLI. This install path is
**unreleased** and is not in the published PyPI `0.5.8` artifact.
A managed [OpenAI Agents SDK profile](examples/managed_openai_agents/README.md)
is also available from source-built wheels through the optional `openai-agents`
extra and `runkite-runner-openai-agents` CLI. This profile is unreleased too.
A managed [Pydantic AI profile](examples/managed_pydantic_ai/README.md) adds
native text agents and validated structured results through `python-pydantic-ai`.
Its source-built `pydantic-ai` extra and `runkite-runner-pydantic-ai` CLI are
also unreleased.
The source-built [Google ADK profile](examples/managed_google_adk/README.md)
adds `python-google-adk` through the optional `google-adk` extra and
`runkite-runner-google-adk` command. It is also unreleased and starts with
one text agent, ordinary local tools and session-local history.
See [CHANGELOG](CHANGELOG.md),
[client integration guidance](docs/client-sdk.md), and the
[support guide](site/support/sdk-integration.html).

## Architecture

<p align="center">
  <img src="docs/assets/ecosystem.png" alt="How Runkite fits together: clients to control plane to runners, with pluggable state, transport, and vector backends" width="920" />
</p>

<p align="center"><b>Three processes, two protocols</b> — clients speak Agent Protocol to the plane; runners speak Runner Protocol to the plane; the plane owns state, transport, and (optionally) vectors underneath both</p>

The two sequence diagrams below show the same architecture from two different angles: a single run traveling through the high-availability profile, and one agent delegating work to another. Skip this section entirely if you just want to try it — [docs/architecture.md](docs/architecture.md) covers the same ground with the full protocol table and backend tier details.

<details>
<summary><b>One run on the HA profile</b></summary>

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

</details>

<details>
<summary><b>Agent-to-agent delegation</b></summary>

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

</details>

## Documentation

| Topic | Links |
|---|---|
| Getting started | [Quick start](docs/quickstart.md) · [Client SDK](docs/client-sdk.md) · [Configuration](docs/configuration.md) · [Auth](docs/auth.md) |
| Core | [Admin UI](docs/admin.md) · [Runners](docs/runners.md) · [Architecture](docs/architecture.md) · [API](docs/api.md) |
| Worker contract (gRPC + opaque checkpoints) | [Runner Protocol](runner-protocol/README.md) · [PROTOCOL.md](runner-protocol/PROTOCOL.md) · [public mirror](https://github.com/getrunkite/runner-protocol) |
| Features | [Connectors](docs/connectors.md) (MCP proxy, optional payload shrink) · [A2A](docs/a2a.md) · [MCP](docs/mcp-server.md) · [Integrations](docs/integrations.md) · [Registry](docs/registry.md) · [Vectors](docs/vector-store.md) |
| Operations | [Deployment](docs/deployment.md) · [Ops runbook](docs/ops-runbook.md) · [Trust & governance](docs/trust-governance.md) · [Fixture replay](docs/sim.md) · [Limitations](docs/limitations.md) · [All docs](docs/README.md) |

The [documentation site](https://getrunkite.com/support/) covers the same material with a guided, page-by-page walkthrough — including a full [Admin UI guide](https://getrunkite.com/support/admin-guide.html) and an occasional [blog post](https://getrunkite.com/support/blog/) — if you'd rather read in that format than jump between Markdown files.

## Development

```bash
make test                 # SQLite + in-memory
make smoke-governance     # governance announce bar (Postgres)
make test-e2e             # black-box binary + runner
```

Full target list, kind/Helm smoke tests, and the nightly matrix: [CONTRIBUTING.md](CONTRIBUTING.md) · [docs/deployment.md](docs/deployment.md).

## License

[Business Source License 1.1](LICENSE). Licensor: **Sharan Harsoor**.

Use, modify, and self-host freely, including in production. You may not offer Runkite as a hosted or managed service to third parties without a commercial license. Each release converts to Apache 2.0 four years after it ships — see `LICENSE` for the exact terms.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for how to get set up and what a good PR looks like. Found a security issue? Report it privately per [SECURITY.md](SECURITY.md) — not as a public issue.
