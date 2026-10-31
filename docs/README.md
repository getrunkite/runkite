# Runkite documentation

Focused guides extracted from the root README. Start with the [root README](../README.md) for a short overview, then dive in here.

| Doc | Topic |
|-----|-------|
| [Quick Start](quickstart.md) | Longer getting-started walkthrough |
| [Client access and SDK integrations](client-sdk.md) | Choose managed execution or app-owned tool decisions; package status and AI SDK/Microsoft examples |
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
| [Runners](runners.md) | Published LangGraph runners, checkout adapters and the managed AI SDK backend (v0.5.8+) |
| [Runner Protocol](../runner-protocol/README.md) ([public mirror](https://github.com/getrunkite/runner-protocol)) | Worker contract (gRPC + opaque checkpoints) |
| [Deployment](deployment.md) | Docker, Helm/kind (Compatible), Compose soak |
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

The unreleased [runtime hardening design record](internal/runtime-hardening.md)
explains the failure cases, invariants, tests and upgrade requirements for current
authorization, recovery, connectors and Admin changes. The follow-up
[atomic results and exact runtime identities](internal/atomic-results-and-identities.md)
record covers transactional cache results, child ownership and SDK identity boundaries.

Feature availability follows [CHANGELOG](../CHANGELOG.md), not the presence of a
README or packaging metadata. Checkout-only adapters and unpublished
governance helpers are labeled separately from tagged runner packages.
