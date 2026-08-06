# Test plan crosswalk

Honesty map of IDs in the internal `plans/test_plan.md` (gitignored)
to v1 launch status. Not a CI gate — a shipping checklist for humans.

Statuses:

| Status | Meaning |
| --- | --- |
| `done` | Covered by unit, integration, e2e, matrix, or soak that we run |
| `mixed` | Core path covered; some IDs thin or polish left |
| `gap` | Known incomplete for v1; do not claim in marketing |
| `deferred` | Explicitly post-v1 |

## By prefix

| Prefix | Count | Status | Notes |
| --- | ---: | --- | --- |
| `AD` | 6 | `mixed` | Admin UI — functional; empty-state polish shipping |
| `AG` | 5 | `done` | Adapters (CrewAI/LI/AutoGen/…) — matrix happy paths |
| `AP` | 62 | `done` | Agent Protocol surface — go tests + protocol fixtures |
| `AU` | 16 | `done` | AuthN/Z — unit + e2e auth paths |
| `AV` | 4 | `done` | Availability / health |
| `CB` | 3 | `done` | Circuit breakers / backpressure where implemented |
| `CF` | 9 | `done` | Config surface |
| `CH` | 6 | `done` | Checkpoints — MemorySaver + Postgres dual-mode |
| `CL` | 7 | `mixed` | Client SDKs — core; packaging release is next |
| `CN` | 10 | `done` | Connectors — API + admin + e2e where applicable |
| `CR` | 6 | `done` | Cron — unit/integration |
| `CS` | 7 | `done` | Cursor/streaming |
| `CU` | 8 | `done` | Cancel/HITL |
| `DX` | 7 | `mixed` | DX docs/CLI — mostly done; polish ongoing |
| `EB` | 9 | `mixed` | Event bus — redis path covered; edge cases thin |
| `EH` | 5 | `done` | Error handling |
| `FA` | 8 | `done` | Failure/reclaim paths — unit + e2e cancel |
| `FG` | 6 | `gap` | FinOps / cost — partial; deep FinOps post-v1 |
| `GO` | 7 | `done` | Go control plane core — unit |
| `IR` | 5 | `mixed` | Idempotency / retries |
| `JQ` | 9 | `done` | Job queue / lease |
| `LC` | 6 | `done` | Lifecycle hooks |
| `LD` | 9 | `done` | Load/durability benches — soak + load scripts |
| `MT` | 5 | `done` | Multi-tenant — unit + e2e isolation |
| `NM` | 19 | `mixed` | NATS/multi-broker — redis primary; optional backends vary |
| `OB` | 6 | `done` | OTel — go/python/ts instrumentation tests |
| `PM` | 5 | `mixed` | Permissions model — core done; claim richness varies |
| `RL` | 4 | `done` | Rate limits — unit |
| `RP` | 25 | `done` | Runner protocol / gRPC — unit + e2e + matrix |
| `SC` | 9 | `mixed` | Security hardening — v1 baseline; v2 design tracked separately |
| `SO` | 6 | `done` | Soak / stability — overnight multi-CP soak green |
| `SS` | 16 | `mixed` | Store/search — core done; some edge IDs thin |
| `TM` | 5 | `done` | Time/timeouts |
| `TR` | 21 | `done` | Thread/run lifecycle — API tests + e2e |
| `TS` | 12 | `done` | TypeScript runner — unit + e2e matrix |
| `TSR` | 7 | `done` | TS runner specifics |
| `VG` | 4 | `done` | Validation graphs / examples |
| `VS` | 10 | `done` | Vector store / pgvector migrations + tests |
| `WH` | 5 | `done` | Webhooks — unit + delivery tests |

**Total IDs enumerated:** 379

## Notable gaps (do not overclaim)

| Area | Status | Reality |
| --- | --- | --- |
| FinOps depth (`FG-*`) | `gap` / deferred | Basic usage signals only; full cost attribution post-v1 |
| Security v2 (`SC-*` stretch) | deferred | v1 = tokens + TLS guidance; fine-grained ABAC later |
| Package release (`CL-*` publish) | mixed | Code ready; PyPI/npm/Go release is the next workstream |
| Admin redesign | deferred | Functional Admin ships; empty-state polish only for v1 |
| Langfuse-in-CP | deferred | Bring your own OTel/Langfuse; not embedded |
| Multi-million events on laptop | deferred | Soak proves durability, not laptop analytics UX |

## Section inventory

### 1.1 RunAssignment Serialization

- IDs in section: 8
- Dominant status: `done`
- Prefixes: `RP`×8

### 1.2 RunEvent Serialization

- IDs in section: 7
- Dominant status: `done`
- Prefixes: `RP`×7

### 1.3 Golden Test Fixtures

- IDs in section: 10
- Dominant status: `done`
- Prefixes: `RP`×10

### 2.1 Job Delivery

- IDs in section: 6
- Dominant status: `done`
- Prefixes: `TR`×6

### 2.2 Event Delivery

- IDs in section: 6
- Dominant status: `done`
- Prefixes: `TR`×6

### 2.3 Cancellation

- IDs in section: 4
- Dominant status: `done`
- Prefixes: `TR`×4

### 2.4 Crash Recovery

- IDs in section: 5
- Dominant status: `done`
- Prefixes: `TR`×5

### 3.1 Agents

- IDs in section: 7
- Dominant status: `done`
- Prefixes: `AP`×7

### 3.2 Threads

- IDs in section: 17
- Dominant status: `done`
- Prefixes: `AP`×17

### 3.3 Runs (Background)

- IDs in section: 13
- Dominant status: `done`
- Prefixes: `AP`×13

### 3.4 Stateless Runs

- IDs in section: 4
- Dominant status: `done`
- Prefixes: `AP`×4

### 3.5 Streaming (Thread-Scoped, Agent Protocol v2)

- IDs in section: 9
- Dominant status: `done`
- Prefixes: `AP`×9

### 3.6 Store

- IDs in section: 12
- Dominant status: `done`
- Prefixes: `AP`×12

### 4.1 State Store (Metadata)

- IDs in section: 16
- Dominant status: `mixed`
- Prefixes: `SS`×16

### 4.2 Checkpoint Store

- IDs in section: 7
- Dominant status: `done`
- Prefixes: `CS`×7

### 4.3 Job Queue

- IDs in section: 9
- Dominant status: `done`
- Prefixes: `JQ`×9

### 4.4 Event Broker

- IDs in section: 9
- Dominant status: `mixed`
- Prefixes: `EB`×9

### 4.5 Vector Store

- IDs in section: 10
- Dominant status: `done`
- Prefixes: `VS`×10

### 6. Connector / MCP Registry

- IDs in section: 10
- Dominant status: `done`
- Prefixes: `CN`×10

### 7. Auth

- IDs in section: 16
- Dominant status: `done`
- Prefixes: `AU`×16

### 8.1 Multi-Tenancy

- IDs in section: 5
- Dominant status: `done`
- Prefixes: `MT`×5

### 8.2 Webhooks

- IDs in section: 5
- Dominant status: `done`
- Prefixes: `WH`×5

### 8.3 Rate Limiting

- IDs in section: 4
- Dominant status: `done`
- Prefixes: `RL`×4

### 8.4 Cron Scheduler

- IDs in section: 6
- Dominant status: `done`
- Prefixes: `CR`×6

### 8.5 Event Hooks

- IDs in section: 5
- Dominant status: `done`
- Prefixes: `EH`×5

### 8.6 Circuit Breakers

- IDs in section: 3
- Dominant status: `done`
- Prefixes: `CB`×3

### 9. Load / Performance

- IDs in section: 9
- Dominant status: `done`
- Prefixes: `LD`×9

### 10. Golden Output Tests (LangGraph Platform Compatibility)

- IDs in section: 7
- Dominant status: `done`
- Prefixes: `GO`×7

### 11. DX / Ergonomics (Manual Verification)

- IDs in section: 7
- Dominant status: `mixed`
- Prefixes: `DX`×7

### 12. Security

- IDs in section: 9
- Dominant status: `mixed`
- Prefixes: `SC`×9

### 13. Validation Gate (CI-Blocking, Must Pass Before Merge to Main)

- IDs in section: 4
- Dominant status: `done`
- Prefixes: `VG`×4

### 14.1 Message Formats

- IDs in section: 6
- Dominant status: `mixed`
- Prefixes: `NM`×6

### 14.2 Interrupt Handling

- IDs in section: 4
- Dominant status: `mixed`
- Prefixes: `NM`×4

### 14.3 Namespace / Subgraph

- IDs in section: 4
- Dominant status: `mixed`
- Prefixes: `NM`×4

### 14.4 Edge Cases

- IDs in section: 5
- Dominant status: `mixed`
- Prefixes: `NM`×5

### 15. Thread-Scoped Runs API

- IDs in section: 12
- Dominant status: `done`
- Prefixes: `TS`×12

### 16. Agents/Assistants CRUD

- IDs in section: 5
- Dominant status: `done`
- Prefixes: `AG`×5

### 17. Dual-Mode x Topology Matrix

- IDs in section: 5
- Dominant status: `done`
- Prefixes: `TM`×5

### 18. Store Schema Ownership Verification

- IDs in section: 6
- Dominant status: `done`
- Prefixes: `SO`×6

### 19. Custom Routes

- IDs in section: 8
- Dominant status: `done`
- Prefixes: `CU`×8

### 20. Framework Adapters (Beyond LangGraph)

- IDs in section: 8
- Dominant status: `done`
- Prefixes: `FA`×8

### 21. Factory / Per-Request Graph API

- IDs in section: 6
- Dominant status: `gap`
- Prefixes: `FG`×6
- Watch: `FG-001`, `FG-002`, `FG-003`, `FG-004`, `FG-005`, `FG-006`

### 22. Observability (OTel Continuity)

- IDs in section: 6
- Dominant status: `done`
- Prefixes: `OB`×6

### 23. Config and Migrations

- IDs in section: 9
- Dominant status: `done`
- Prefixes: `CF`×9

### 24. TypeScript Runner

- IDs in section: 7
- Dominant status: `done`
- Prefixes: `TSR`×7

### 25.1 LLM Response Caching

- IDs in section: 6
- Dominant status: `done`
- Prefixes: `LC`×6

### 25.2 Agent Versioning (Basic)

- IDs in section: 4
- Dominant status: `done`
- Prefixes: `AV`×4

### 25.3 Admin API

- IDs in section: 6
- Dominant status: `mixed`
- Prefixes: `AD`×6

### 25.4 Prometheus Metrics

- IDs in section: 5
- Dominant status: `mixed`
- Prefixes: `PM`×5

### 26. Chaos / Ops

- IDs in section: 6
- Dominant status: `done`
- Prefixes: `CH`×6

### 27. Idempotency and Races

- IDs in section: 5
- Dominant status: `mixed`
- Prefixes: `IR`×5

### 28. CLI Subcommands

- IDs in section: 7
- Dominant status: `mixed`
- Prefixes: `CL`×7

## How this was produced

IDs scraped from table rows in `plans/test_plan.md`. Prefix status is
engineering judgment against the current Makefile targets (`test`,
`test-all`, e2e, matrix, llm-matrix, soak) as of the v1 launch prep.
Re-run the scraper in this repo if the plan grows.
