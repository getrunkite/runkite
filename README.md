# Runkite

### Keep your agent framework. Share the control plane.

A self-hosted control plane for AI agents. Dispatch, connector access, human
approval, spend and audit in one Go binary, with an embedded Admin UI.

Run agents through Runkite, or keep the loop in your app and govern individual
tool calls. Your framework still owns the model loop.

[![CI](https://github.com/getrunkite/runkite/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/getrunkite/runkite/actions/workflows/ci.yml?query=branch%3Amain)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![PyPI](https://img.shields.io/pypi/v/runkite-runner)](https://pypi.org/project/runkite-runner/)
[![npm](https://img.shields.io/npm/v/runkite-runner)](https://www.npmjs.com/package/runkite-runner)
[![GHCR](https://img.shields.io/badge/GHCR-getrunkite%2Frunkite-blue)](https://github.com/getrunkite/runkite/pkgs/container/runkite)

Current source and v0.6.1+ downloads are Apache-2.0. Published v0.6.0 downloads retain BUSL-1.1.
See [license scope](#license).

**[Quick start](#quick-start)** · **[Choose your integration](#choose-an-integration-path)** · **[Documentation](https://getrunkite.com/support/)** · **[Demo guide](https://getrunkite.com/support/try.html)**

**[v0.10.0](https://github.com/getrunkite/runkite/releases/tag/v0.10.0) requires a coordinated upgrade:** back up and drain all writers, apply Postgres/SQLite migration 23 or MySQL migration 24, and deploy matching protocol-1 planes and runners. Reconcile legacy accounting, hook claims and ambiguous receipts before restoring traffic. Follow the [upgrade sequence](docs/internal/release-candidate-v0100.md#upgrade-sequence). Kafka requires an explicit non-atomic compatibility opt-in, even with Redis.

The [integration and operator guide](docs/integrations.md#unreleased-current-source) covers scoped specialist callers, advisory discovery, retained-run visibility and the Langflow, promptfoo and AG-UI examples. Runtime features ship in v0.10.0; application examples and qualification scripts remain checkout assets. See the [actual publication and qualification record](docs/internal/release-verification-v0100.md).

<p align="center">
  <img src="docs/assets/runkite-overview.svg" alt="Two ways to use one Runkite plane: managed agents create Threads and Runs through runners; app-owned loops request individual tool decisions without creating managed runs." width="1120" />
</p>

[Website](https://getrunkite.com/) · [Releases](https://github.com/getrunkite/runkite/releases) · [Examples](examples/) · [Architecture](#architecture) · [Development](#development) · [License](#license)

## Why Runkite

Each new agent brings the same operational questions: who can run it, which
connector it may call, which action needs approval, what it spent, and what
happened when a worker stopped. Runkite gives those operations a shared home
across frameworks.

| Need | What Runkite provides |
|---|---|
| **Run agents** | Authenticated dispatch, durable Threads/Runs, streaming, cooperative cancel and generation-fenced recovery through separate runners. |
| **Connect tools** | Run-bound connector sessions, secret resolution at session time, and credential injection on outgoing calls. |
| **Approve actions** | Connector grants, argument predicates and human approval before gated operations proceed. Tools must use the gate to acquire its policy. |
| **Track spend** | Reported usage, estimated USD, daily admission caps and optional reservations on SQL backends. See [accounting limits](docs/limitations.md). |
| **Operate together** | Embedded Admin, audit, agent delegation, Prometheus metrics and OTLP traces exported to your existing backend. |
| **Self-host** | One Go control-plane binary. Local SQLite/in-process mode for a first run; Postgres + Redis is the Supported HA profile. Framework runners keep their own Python or Node.js runtime. |

See [trust and governance](docs/trust-governance.md), [connectors](docs/connectors.md)
and the [backend support tiers](docs/architecture.md#backend-support-tiers) for
the enforcement and deployment contracts.

v0.7.0 introduced optional [required policy audit](docs/configuration.md#required-policy-audit)
that blocks access when its SQL decision record cannot be saved. Agent delegation
derives authority from the active parent assignment. See the
[hardening design and upgrade notes](docs/internal/action-authority-hardening.md).
The release also offers opt-in [connector action evidence](site/support/connector-actions.html):
record intent before managed MCP dispatch and inspect observed responses or
unknown outcomes in Admin. It also adds
[tenant-scoped review](docs/auth.md#tenant-review): reviewers can inspect evidence
or approve their own tenant's operations without global Admin access. These
settings and permissions are not in published v0.6.1 binaries. Managed MCP
approvals also [bind to the configured connector target](docs/connectors.md#target-bound-managed-approvals-unreleased),
so a changed target cannot consume an earlier approval. Credential values can
rotate without changing that binding.

<a id="security-completion-in-current-source-unreleased"></a>

### Security controls introduced in v0.7.0

v0.7.0 introduced three opt-in controls around existing managed connector
authority: [distinct-reviewer quorum and requester separation](docs/internal/approval-authority-and-quorum.md),
[durable action evidence export](docs/internal/action-delivery-outbox.md), and
[registered Vault database credential leases](docs/internal/credential-lifecycle.md).
Managed approvals bind the current configured policy and optional observed MCP
tool schema as well as arguments, target and run incarnation. Historical unbound
approvals require fresh review. These settings are not in published v0.6.1 packages.
Deploy matching plane and runner versions. Follow the coordinated SQL
upgrade before enabling new controls; do not mix older writers with the new
authority fields.

[Execution pools and an experimental external OpenShell bridge](docs/internal/constrained-execution.md)
add a separately credentialed fleet and explicit workload boundary. Existing SDK
workers are not automatically sandboxed; the bridge requires its JSONL workload
contract. Rendered network policy and configuration hashes do not prove runtime
or CNI enforcement. Disposable qualification has passed with an installed Python
runner, a real SQLite plane and OpenShell 0.1.8, plus a separate enforcing-CNI
profile. The [qualification record](docs/internal/constrained-execution.md#verification-and-remaining-qualification)
states the tested cases and remaining limits. Start with the [security operations guide](site/support/security-completion.html)
and [upgrade procedure](docs/ops-runbook.md#security-completion-upgrade).

The [v0.7.0 qualification record](docs/internal/security-completion-qualification.md)
collects that release's tested backends, installed SDK profiles, failure cases
and gates. Its JSONL profile did not report model/token usage or settle model
spend. v0.8.0 adds durably acknowledged cumulative usage reports for
the configured isolated profile on SQL, with terminal settlement including
cancellation. Partial reports settle only acknowledged usage; missing
measurements stay unknown, and a complete reported zero remains distinct.
These are workload claims, not
provider-verified bills or hard limits inside a sandbox. Native SDK usage
reporting remains a separate capability.

<a id="identity-and-distribution-in-v080-source"></a>

### Identity and distribution in v0.8.0

v0.8.0 includes an opt-in authenticated-workload ownership profile,
constrained delegated token exchange, external audit correlation and explicit
isolated-workload usage reports. Admin displays admission and worker identity,
Vault request IDs and unavailable/partial usage separately. These additions are
not in v0.7.0 artifacts. The TypeScript and Python governance helpers first ship
as independent `runkite-governance` packages in v0.8.0, with installed-artifact
tests and verified public downloads. See the
[v0.8.0 release](https://github.com/getrunkite/runkite/releases/tag/v0.8.0),
[npm](https://www.npmjs.com/package/runkite-governance) and
[PyPI](https://pypi.org/project/runkite-governance/).

Follow the [coordinated upgrade](docs/ops-runbook.md#identity-and-distribution-upgrade-v080),
[eight tracked RFEs](docs/internal/identity-and-distribution-plan.md),
[identity and usage guide](site/support/security-identity.html) and
[adversarial assessment handoff](docs/security-assessment.md). The external
runtime bridge remains experimental. Maintainer qualification is not an
independent assessment or a security ranking.

## Choose an integration path

v0.9.0 adds [retry-safe MCP and a clearer operator
workflow](docs/internal/mcp-retry-and-operator-flow.md): stable MCP operation IDs,
direct approval navigation, a plain-text final-answer preview and a scripted
governance walkthrough. These features require v0.9.0 or matching source;
v0.8.0 does not include them. See [Releases](https://github.com/getrunkite/runkite/releases)
for verified publication status.

The checkout-only [Trinity specialist example](examples/trinity_specialist_mcp/README.md)
keeps Trinity's native loop and invokes one Runkite-managed specialist. It
documents durable caller intent, read-only retry observation and the separate
configuration/protocol qualification boundaries. It is not a new runner kind.

| | Runkite owns execution | Your app owns execution |
|---|---|---|
| **Good fit** | Agents you want Runkite to dispatch and operate. | An existing Next, Nuxt or other backend that keeps its SDK loop. |
| **Entry point** | Agent Protocol over HTTP, SSE or WebSocket; Runner Protocol over gRPC. | `POST /governance/decide`, with approval status available for pending actions. |
| **Where the SDK runs** | In a separate Runkite runner. | In your application process. |
| **What Admin sees** | Managed Threads and Runs, outputs and reported usage. | Submitted policy decisions and pending actions on SQL backends. No managed Thread or Run is created. |
| **Controls** | Managed-run admission, cooperative cancel, usage accounting and recovery; connector policy where used. | Allow, deny or pending for the submitted tool call. Run kill switches, break-glass and spend caps do not apply. |

The original LangGraph client path remains available: use
`langgraph_sdk.get_client(url=...)` to create a thread and stream a run from
Runkite, while a separate LangGraph runner executes the graph. See the
[client-first walkthrough](docs/client-sdk.md#agent-protocol-clients-come-first).
Choosing another managed runner changes the executor, while Threads and Runs
still enter through the plane's public API.

For managed chat, your app creates a Runkite run and consumes its stream. The
[shared AI SDK UI bridge](examples/managed_ai_sdk/README.md#use-the-same-frontend-with-a-python-runner)
keeps UI authentication, saved chat ownership, reconnect and explicit Stop in
your application.

For app-owned execution, [AI SDK tool helpers](examples/governance_ai_sdk/README.md)
and [Microsoft function middleware](examples/governance_agent_framework/README.md)
use the independent `runkite-governance` helpers, separate from the runner
packages. Their [TypeScript](typescript/runkite-governance/README.md) and
[Python](python/runkite_governance/README.md) guides distinguish installation
from a verified registry release and from a checkout. The app executes after
allow and owns pending continuation.
A decision is not proof of execution, and its `run_id` is an operation correlation
value rather than execution idempotency.
The managed Node.js runner does not require the governance helper. See
[packages and downloads](https://getrunkite.com/#packages) or the
[complete installation map](site/support/install.html#packages) for binary,
registry and container choices.
For helper deployments, the [app-owned governance guide](docs/app-owned-governance.md)
walks through administrator configuration, application integration and review.

### Bring your framework

The v0.10.0 runners retain these profiles from earlier releases. Check the runner
registry links above for published versions, and deploy matching plane and runner
artifacts. Each profile supports its documented subset of native features.

| Managed framework | Install or select | Runnable example |
|---|---|---|
| **LangGraph** | `pip install runkite-runner` | [Python echo agent](examples/echo_agent/) |
| **LangGraph.js** | `npm install -g runkite-runner` | [TypeScript echo agent](examples/echo_agent_ts/) |
| **Vercel AI SDK** | TypeScript runner with `--framework ai-sdk`; requires the `ai` peer | [Managed agent + UI bridge](examples/managed_ai_sdk/) |
| **Microsoft Agent Framework** | `pip install 'runkite-runner[agent-framework]'` | [Managed Microsoft agent](examples/managed_agent_framework/) |
| **OpenAI Agents SDK** | `pip install 'runkite-runner[openai-agents]'` | [Managed OpenAI agent](examples/managed_openai_agents/) |
| **Pydantic AI** | `pip install 'runkite-runner[pydantic-ai]'` | [Text and structured results](examples/managed_pydantic_ai/) |
| **Google ADK** | `pip install 'runkite-runner[google-adk]'` | [Managed ADK agent](examples/managed_google_adk/) |

CrewAI, LlamaIndex, AutoGen and plain LangChain adapters remain **checkout-only**.
Install their framework dependencies and follow [Runners](docs/runners.md).
The stock Python image keeps its LangGraph default and does not install the four
optional SDK extras. See the [profile contracts](docs/limitations.md) before
moving agents that use native workflows, approvals, handoffs or multimodal input.

For a runnable model-routing integration, read
[One Conversation, Multiple Models: Runkite and vLLM Semantic Router](https://vllm-sr.ai/blog/runkite-ai-sdk-semantic-router/).
The managed AI SDK runner keeps its native loop, Runkite operates the run, and
Semantic Router selects among configured model backends.

## Quick start

Run the local LangGraph example below, or follow the
[demo guide](https://getrunkite.com/support/try.html). The public demo Admin is
currently offline; the website and recorded walkthrough remain available.
You operate your own control plane.

From a checkout, with Go and Python installed:

~~~bash
git clone https://github.com/getrunkite/runkite
cd runkite
make build

# Terminal 1: local control plane, SQLite + in-process transport
./runkite dev --config examples/echo_agent/langgraph.json

# Terminal 2: Python runner
pip install runkite-runner
runkite-runner --config examples/echo_agent/langgraph.json \
  --grpc-address 127.0.0.1:50051 --http-address http://127.0.0.1:2026
~~~

Open Admin at **http://localhost:2026/admin/**.

[Full quickstart](docs/quickstart.md) · [Client requests and streaming](docs/client-sdk.md) · [Binary releases](https://github.com/getrunkite/runkite/releases) · [Docker and Helm](docs/deployment.md)

For a supported production deployment, use **Postgres + Redis**, client auth,
runner tokens and TLS. [Deployment](docs/deployment.md) and the
[operator runbook](docs/ops-runbook.md) describe setup and upgrades.

### See the real Admin

<p align="center">
  <a href="https://getrunkite.com/admin/" title="Public demo Admin is currently offline. Open for availability and local setup.">
    <img src="docs/assets/admin-walkthrough.gif" alt="Recorded Runkite Admin walkthrough with sample agents, runs, connectors, approvals and audit." width="920" loading="lazy" />
  </a>
</p>

The Admin UI ships inside the control-plane binary. It needs no separate frontend
server. The [public Admin link](https://getrunkite.com/admin/) is retained while
the demo is offline. Start your own instance with the quickstart above or read
the [Admin guide](docs/admin.md).

## Architecture

Clients speak **Agent Protocol** to the plane. Workers pull assignments over
**Runner Protocol**, run their framework and report events and completion. Model
calls stay in those workers, using your provider or gateway.

The plane owns operational state and coordination. Postgres + Redis is the
Supported HA pair; optional backends have explicit
[support tiers](docs/architecture.md#backend-support-tiers).

<details>
<summary><b>Follow one run through the HA profile</b></summary>

~~~mermaid
sequenceDiagram
  autonumber
  participant C as Client / SDK
  participant LB as Load balancer
  participant CP as CP replica
  participant PG as Postgres
  participant RD as Redis
  participant R as Framework runner

  C->>LB: Create thread + run
  LB->>CP: Agent Protocol (HTTP)
  CP->>PG: Persist thread + run
  CP->>RD: Enqueue assignment
  R->>CP: GetJob (gRPC)
  CP->>RD: Claim assignment
  CP-->>R: RunAssignment + generation
  Note over R: Framework owns the model loop
  R->>CP: StreamEvents + ReportStatus
  CP->>PG: Commit output + checkpoint
  CP-->>C: SSE / WebSocket output
~~~

</details>

<details>
<summary><b>Agent-to-agent delegation</b></summary>

~~~mermaid
sequenceDiagram
  participant Coord as Coordinator agent
  participant CP as Control plane
  participant Work as Worker agent

  Note over Coord,CP: Parent run executes on a runner
  Coord->>CP: call_agent / callAgent
  CP->>CP: Check depth + parent/root bookkeeping
  CP->>Work: Dispatch child run
  Work-->>CP: Report child result
  CP-->>Coord: Return child output
~~~

</details>

[Architecture details](docs/architecture.md) · [Runner Protocol](runner-protocol/PROTOCOL.md) · [Agent delegation](docs/a2a.md) · [MCP server](docs/mcp-server.md) · [Integrations](docs/integrations.md)

## What Runkite is not

Runkite operates the infrastructure around an agent. Your framework keeps planning,
model calls and tool execution. Native tools that bypass connectors or decision
helpers do not automatically acquire Runkite policy. Managed cancellation is
cooperative and cannot undo completed side effects.

Model requests can go directly to your provider or existing gateway. Cost figures
are estimates based on reported usage and your pricebook, with the
[documented limits](docs/limitations.md). Self-hosting is the product; the public
demo is a sandbox.

## Documentation

| Topic | Links |
|---|---|
| **Start building** | [Quickstart](docs/quickstart.md) · [Client SDK](docs/client-sdk.md) · [Configuration](docs/configuration.md) · [Auth](docs/auth.md) |
| **Operate agents** | [Admin](docs/admin.md) · [Runners](docs/runners.md) · [Connectors](docs/connectors.md) · [Trust and governance](docs/trust-governance.md) |
| **Deploy and upgrade** | [Deployment](docs/deployment.md) · [Ops runbook](docs/ops-runbook.md) · [Limitations](docs/limitations.md) |
| **Understand the design** | [Architecture](docs/architecture.md) · [Engineering notebook](docs/internal/engineering-notebook/README.md) · [Historical walkthrough](docs/internal/architecture-deep-dive.md) · [Runner Protocol](runner-protocol/README.md) |

The [documentation site](https://getrunkite.com/support/) provides guided product
and integration walkthroughs. Additional reference: [API](docs/api.md),
[registry](docs/registry.md), [vectors](docs/vector-store.md) and
[fixture replay](docs/sim.md).

## Development

~~~bash
make test                 # SQLite + in-memory
make smoke-governance     # Governance checks on Postgres
make test-e2e             # Binary + runner end to end
~~~

[Contributing](CONTRIBUTING.md) includes the full development workflow.
[CI](https://github.com/getrunkite/runkite/actions/workflows/ci.yml) and the
[nightly framework matrix](https://github.com/getrunkite/runkite/actions/workflows/matrix.yml)
exercise separate validation scopes.

The [runtime hardening record](docs/internal/runtime-hardening.md),
[identity and atomic-result rationale](docs/internal/atomic-results-and-identities.md),
[HA recovery evidence](docs/internal/ha-recovery-validation.md) and
[upgrade guidance](docs/ops-runbook.md#hardening-upgrade) explain the v0.6.0
operational changes and their limits.

## License

[Apache License, Version 2.0](LICENSE). Copyright 2026 Sharan Harsoor.
See [NOTICE](NOTICE) for attribution.

This source tree permits commercial use, modification, redistribution and
hosting under Apache-2.0. Third-party components retain their own licenses;
the license does not grant general trademark rights. Runkite remains a
self-hosted product.

v0.6.1 and later releases ship under Apache-2.0. Already-published v0.6.0 and
earlier artifacts retain their shipped
[BUSL-1.1 terms](https://github.com/getrunkite/runkite/blob/v0.6.0/LICENSE).
For the scope and release checks, see the
[license transition record](docs/internal/apache-2.0-transition.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) to get started. Report security issues
privately following [SECURITY.md](SECURITY.md).
