# Runkite documentation

Focused guides extracted from the root README. Start with the [root README](../README.md) for a short overview, then dive in here.

For additions after published v0.9.0, start with the **[Unreleased current-source guide](integrations.md#unreleased-current-source)**. It links advisory discovery, specialist authority, operator visibility and the bounded caller/UI examples without changing package availability. Contributors should also use the [integration and qualification checklist](internal/ecosystem-integration-checklist.md).

| Doc | Topic |
|-----|-------|
| [Quick Start](quickstart.md) | Longer getting-started walkthrough |
| [Client access and SDK integrations](client-sdk.md) | Managed AI SDK and Python profiles, shared chat/reconnect, app-owned tool decisions and package availability |
| [Configuration](configuration.md) | `langgraph.json` and environment variables, including opt-in `payload_shrink` |
| [Auth](auth.md) | Auth, TLS/mTLS, multi-tenancy |
| [Trust & governance](trust-governance.md) | Trust boundary, run-binding, non-goals; `make smoke-governance` |
| [Fixture replay](sim.md) | `runkite sim`: YAML against a live plane; spend/daily skip, policy kept |
| [Admin UI](admin.md) | Embedded ops + SQL governance pages (grants, HITL, kill, audit, …) |
| [Factory Graphs](factory-graphs.md) | LangGraph SDK factory-graph compatibility |
| [Vector Store](vector-store.md) | Vector store backends and APIs |
| [Connectors](connectors.md) | Connectors, MCP proxy, optional payload shrink |
| [A2A](a2a.md) | Agent-to-Agent protocol |
| [MCP Server](mcp-server.md) | MCP server surface |
| [Registry](registry.md) | Agent marketplace / registry |
| [Architecture](architecture.md) | Control plane, backends, dual modes |
| [Runners](runners.md) | Published LangGraph, AI SDK, Microsoft, OpenAI Agents, Pydantic AI and Google ADK profiles; checkout-only adapters |
| [Integrations](integrations.md) | Existing MCP clients, framework runners, Semantic Router and OTLP backends |
| [Runner Protocol](../runner-protocol/README.md) ([public mirror](https://github.com/getrunkite/runner-protocol)) | Worker contract (gRPC + opaque checkpoints) |
| [Deployment](deployment.md) | Docker, Helm/kind (Compatible), Compose soak |
| [Public website hosting](site-hosting.md) | GitHub Pages custom domain, offline public Admin and future backend restoration |
| [Ops runbook](ops-runbook.md) | Supported install, reclaim, kill, secrets |
| [API](api.md) | OpenAPI and HTTP API reference |
| [Known Limitations](limitations.md) | Release summary + full caveats (link from GitHub Releases) |
| [Test plan crosswalk](test-plan-crosswalk.md) | Internal test-plan IDs → done / mixed / gap (honesty map) |
| [Agent skills](../skills/README.md) | Cursor skill: scaffold a new Runkite agent |
| [Releasing](releasing.md) | Tag → GitHub Release, GHCR, PyPI, npm |

Public HTML walkthroughs live in [site/support](../site/support/), including the
[SDK integration guide](../site/support/sdk-integration.html). Engineering
[study notes](internal/architecture-deep-dive.md) include a current-contract
correction map; the [embedded governance design record](internal/embedded-governance-next-release.md)
is historical and points to the implemented API.

The v0.8.0 [identity and distribution milestone](internal/identity-and-distribution-plan.md)
maps RFEs #45 through #52 to code, qualification and availability gates. Read
[workload ownership](internal/workload-identity.md),
[delegation and audit correlation](internal/delegation-and-audit-correlation.md),
[isolated usage](internal/isolated-usage.md) and
[governance package distribution](internal/governance-distribution.md) for the
design rationale. The [assessment handoff](security-assessment.md) distinguishes
automated qualification from pending independent review.

The v0.6.0 [runtime hardening design record](internal/runtime-hardening.md)
explains the failure cases, invariants, tests and upgrade requirements for current
authorization, recovery, connectors and Admin changes. The follow-up
[atomic results and exact runtime identities](internal/atomic-results-and-identities.md)
record covers transactional cache results, child ownership and SDK identity boundaries.

The v0.6.0 [local HA validation record](internal/ha-recovery-validation.md)
separates steady load from bounded cache, cancel, reclaim and retained-stream
recovery checks, with fixture setup and measured rehearsal limits.

The [Apache-2.0 transition record](internal/apache-2.0-transition.md) explains
current-source licensing, the unchanged terms of published v0.6.0 artifacts,
third-party notices and the packaging checks retained for release archives.

Feature availability follows [CHANGELOG](../CHANGELOG.md), not the presence of a
README or packaging metadata. Checkout-only adapters, independent governance
helpers and managed runner packages have separate installation contracts.

[AAIF Sandbox application materials](foundation/README.md) describe the project's
intended users, current stewardship, dependency review scope and community
roadmap. This is application preparation, not a foundation admission claim.

- [Identity and distribution qualification](internal/identity-distribution-qualification.md): executed maintainer checks and remaining external gates for v0.8.0.
- [Node fetch and TLS compatibility](internal/node-fetch-tls-compatibility.md): the Node 22 dispatcher failure found by hosted qualification, its dependency adapter and regression coverage.
- [v0.8.0 package recovery](internal/release-recovery-v080.md): release-client assumptions, public-index visibility and immutable source/controller recovery.
- [v0.9.0 publication verification](internal/release-verification-v090.md): immutable source, archive and package checks, six image variants, actual publishing authority and qualification limits.
