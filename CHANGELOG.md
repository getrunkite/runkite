# Changelog

All notable releases are documented here. Version source of truth: [`VERSION`](./VERSION).

## Unreleased

### Fixed
- Run-bound opaque checkpoint writes and deletes check the durable attempt and terminal status in the same transaction as the mutation. An upload authorized before reclaim or cancellation can no longer replace or remove a newer attempt's saved history. Blob ETag checks remain separate.
- Python and TypeScript workers isolate supersession cancellation per attempt. An older generation cannot cancel its replacement or remove its cancellation registration when both execute in the same worker. Genuine run cancellation still reaches all attempts and racing registrations.

## [0.5.8] - 2026-10-29

Managed AI SDK runs, opaque checkpoint fail-closed behavior, and docs/Admin copy
aligned to the current ownership model. Preview (BUSL), not a 1.0 production
claim, and **not** a hosted control plane.

### Added
- TypeScript `createGovernedExecute` guards a single-result app-owned tool with one decide call. The AI SDK example uses it, including `streamText` coverage for ordinary tools. Pending continuation stays in the app. The helper remains unpublished on npm.
- Optional Python `GovernanceFunctionMiddleware` for Microsoft Agent Framework local function tools. Same decide API, app-owned pending continuation, and an offline example. Install via the `agent-framework` extra. The helper remains unpublished on PyPI.
- Managed AI SDK TypeScript runner, selected with `--framework ai-sdk`, using public `ToolLoopAgent` APIs and per-run factories. Native history and committed-turn receipts use run-bound opaque checkpoints with CAS. Managed executions appear in Admin Threads and Runs.
- Application-side AI SDK UI stream bridge and an offline example in `examples/managed_ai_sdk`. The bridge confirms durable completion before UI finish and keeps Runkite credentials and chat ownership in the app.
- Small TypeScript execution interface shared by the existing LangGraph backend and optional AI SDK backend. The SDK peer is pinned to the tested `ai@7.0.155`.

### Fixed
- Non-LangGraph Python adapters with opaque persistence stop on failed state reads. Completion and HITL-ready events follow a confirmed state save. Failed writes leave the assignment recoverable; rejected `run_not_inflight` writes no longer pretend to succeed.
- Malformed or unsupported adapter message checkpoints fail before execution instead of silently discarding history.

### Notes
- Managed AI SDK ships in this release's `runkite-runner` npm package and TypeScript runner image. Install optional peer `ai@7.0.155`. Initial support is new text turns and local JSON-result tools. Native approval/resume, binary/client/provider tools, streaming tool outputs, time travel and whole-run caching are unsupported. See [Runners](docs/runners.md) and [Limitations](docs/limitations.md).
- The Release workflow still publishes only `runkite-runner`. `runkite-governance` stays unpublished; install from the repository or a local pack/wheel.
- SQLite/in-process and single-plane Postgres/Redis integration checks cover restart and crash-after-commit receipt recovery. They do not establish a new multi-replica HA soak or exactly-once external writes before state commit.
- **Self-host only.** A managed/hosted control plane is not in this release.

[0.5.8]: https://github.com/getrunkite/runkite/releases/tag/v0.5.8

## [0.5.7] - 2026-10-29

The control plane image and Go binaries use Go 1.26.10. Preview (BUSL), not a 1.0 production claim, and **not** a hosted control plane.

### Fixed
- Admin UI uses Vite 8.2.4. Vite 8.2.2 has a high advisory.
- The control plane builds with Go 1.26.10. Go 1.26.9 is affected by CVE-2026-111943 (`crypto/x509`).
- golangci-lint passes on the v0.5.6 completion code. Deferred SQL rollbacks and best-effort run cleanup no longer leave errors unchecked. Test nil checks return after `t.Fatal`, which staticcheck does not treat as an exit on Go 1.26.10.

### Notes
- No runner behavior change.
- The governance helpers are still unpublished.
- **Self-host only.** A managed/hosted control plane is not in this release.

[0.5.7]: https://github.com/getrunkite/runkite/releases/tag/v0.5.7

## [0.5.6] - 2026-10-29

Runner identity, completion, and approval fixes from the v0.5.5 review. Preview (BUSL), not a 1.0 production claim, and **not** a hosted control plane.

### Fixed
- A runner token is bound to its kind. That kind cannot dequeue, watch cancels, heartbeat, stream, or complete another kind's run. Local mode with no runner tokens is unchanged.
- A run result is stored before its queue lease is acknowledged. A failed write leaves the job reclaimable. Events and stored output stay on the attempt that produced them. Generation 0 cannot renew, acknowledge, or complete a later attempt.
- An assignment that has used its reclaim retries stays leased until completion and finalization succeed.
- An open approval is reused only for the same tenant, agent, and call. Approving an expired embedded row is rejected in the same database write.
- MCP allow and deny use one strict JSON reading, and that reading is what is forwarded. Duplicate or differently spelled method and tool names are rejected. Request and response bodies are size-capped.
- Governance helper deadlines cover the blocked HTTP call. Python and TypeScript workers report their own runner kind. Helm `runner.extraEnv` accepts `value` or `valueFrom`. Admin Run Detail reloads the stored run after the live stream ends.

### Added
- Connectors can set `mcp.transport` to `streamable_http`. The default remains a single JSON POST. Each call uses its own upstream session, and a failed response is not retried as another tool call.
- `examples/deepagents_agent` and `examples/governance_ai_sdk` show an offline Deep Agents graph and an app that keeps its own model loop.

### Notes
- Runners must send the generation from the assignment. Omitting it no longer matches a newer attempt.
- The governance helpers are still unpublished.
- **Self-host only.** A managed/hosted control plane is not in this release.
- **Supported HA:** Postgres + Redis. Kubernetes/Helm is Compatible (kind + EKS smoke, not a multi-hour cloud HA soak). This cut did not add a new multi-replica soak.
- Limitations: [`docs/limitations.md`](docs/limitations.md)

[0.5.6]: https://github.com/getrunkite/runkite/releases/tag/v0.5.6

## [0.5.5] - 2026-10-16

The TypeScript runner image can be built again. Preview (BUSL), not a 1.0 production claim, and **not** a hosted control plane.

### Fixed
- `Dockerfile.runner-ts` can see `package-lock.json`. The control-plane image still drops example lockfiles after copy, so its vulnerability scan stays clean.

### Notes
- No runner behavior change.
- **Self-host only.** A managed/hosted control plane is not in this release.

[0.5.5]: https://github.com/getrunkite/runkite/releases/tag/v0.5.5

## [0.5.4] - 2026-10-16

Patch so the control-plane image and the TypeScript runner clear the current vulnerability scan. Preview (BUSL), not a 1.0 production claim, and **not** a hosted control plane.

### Fixed
- Control plane builds with Go 1.26.9. `golang.org/x/net` is v0.60.0 and OpenTelemetry is v1.45.0. Example lockfiles are no longer copied into the image.
- The custom-route proxy uses `Rewrite` instead of the deprecated `Director`.
- Admin UI lockfile resolves `source-map-js` 1.2.2.
- TypeScript runner lockfile resolves `@grpc/grpc-js` 1.14.6 and `undici` 8.12.2.

### Notes
- No runner behavior change beyond those dependency patches. The governance helpers are unchanged.
- **Self-host only.** A managed/hosted control plane is not in this release.
- Limitations: [`docs/limitations.md`](docs/limitations.md)

[0.5.4]: https://github.com/getrunkite/runkite/releases/tag/v0.5.4

## [0.5.3] - 2026-10-16

In-process tool governance for an app that keeps its own model loop. Preview (BUSL) -- not a 1.0 production claim, and **not** a hosted control plane.

### Added
- `POST /governance/decide`: allow, deny, or pending for one in-process tool call. Permission `governance:decide` (not implied by `read`, `write`, or `agents:<id>:run`). Pending returns `action_id` and requires `run_id`. `GET /governance/actions/{action_id}` polls it. Admin approve and deny use the existing pending row. Embedded rows still pending after 24h are rejected. Connector HITL rows are not expired. Kill switches, break-glass, and spend caps are not applied. Admin Threads and Runs do not list that caller.
- TypeScript helper `typescript/runkite-governance`: `decide`, then poll until approved or rejected. Not the full client SDK. Not published.
- Python helper `python/runkite_governance`: the same decide-then-poll calls. Stdlib only. Not the full client SDK. Not published.
- Governance helpers stop polling when the caller aborts. TypeScript takes an `AbortSignal`. Python takes a `threading.Event`.

### Notes
- No Python or TypeScript runner package logic change -- version lockstep only. The helpers live in the repo and are not the published runner packages.
- **Self-host only.** A managed/hosted control plane is not in this release.
- **Supported HA:** Postgres + Redis. Kubernetes/Helm is Compatible (kind + EKS smoke, not a multi-hour cloud HA soak).
- Governance + FinOps durability remain SQL-only; Mongo returns `501` / fail-closed on those routes.
- Runner Protocol stays independently versioned (Draft 0.1.0). Agent Protocol OpenAPI remains 0.1.6.
- Limitations: [`docs/limitations.md`](docs/limitations.md)

[0.5.3]: https://github.com/getrunkite/runkite/releases/tag/v0.5.3

## [0.5.2] — 2026-09-29

Try Agent hop: agent → plane → tool, live from connector `tool_auth`. Preview (BUSL) -- not a 1.0 production claim, and **not** a hosted control plane.

### Added
- Read-only hop strip on Try Agent (SSE `tool_auth`; amber hold clicks through to Pending)
- Pending Actions reads `run_id` / `tenant_id` query params so that deep link filters the queue

### Fixed
- Delayed hop particle stays invisible until `animateMotion` begin (same origin-flash as the landing diagram)

### Notes
- No Python or TypeScript runner package logic change -- version lockstep only.
- **Self-host only.** A managed/hosted control plane is not in this release.
- **Supported HA:** Postgres + Redis. Kubernetes/Helm is Compatible (kind + EKS smoke, not a multi-hour cloud HA soak).
- Governance + FinOps durability remain SQL-only; Mongo returns `501` / fail-closed on those routes.
- Runner Protocol stays independently versioned (Draft 0.1.0). Agent Protocol OpenAPI remains 0.1.6.
- Limitations: [`docs/limitations.md`](docs/limitations.md)

[0.5.2]: https://github.com/getrunkite/runkite/releases/tag/v0.5.2

## [0.5.1] — 2026-09-26

First-hour Try loop: connector Pending on a fake bank refund. Preview (BUSL) -- not a 1.0 production claim, and **not** a hosted control plane.

### Added
- `pending_agent` example: no-LLM graph calls connector MCP `refund`; predicate `amount >= 100` holds in Admin Pending; digest-bound retry after approve
- Fake bank MCP (`examples/pending_agent/mcp_server.py`) and `pending-mcp` in `docker-compose*.yml`
- Try Agent defaults to `pending_agent` and links connector pending to Admin Pending
- Public Try-it / admin-guide / feature-map copy for the refund hold

### Notes
- No Python or TypeScript runner package logic change -- version lockstep only. Policy pending stays JSON-RPC `-32000` in the MCP body (HTTP 200).
- **Self-host only.** A managed/hosted control plane is not in this release.
- **Supported HA:** Postgres + Redis. Kubernetes/Helm is Compatible (kind + EKS smoke, not a multi-hour cloud HA soak).
- Governance + FinOps durability remain SQL-only; Mongo returns `501` / fail-closed on those routes.
- Runner Protocol stays independently versioned (Draft 0.1.0). Agent Protocol OpenAPI remains 0.1.6.
- Limitations: [`docs/limitations.md`](docs/limitations.md)

[0.5.1]: https://github.com/getrunkite/runkite/releases/tag/v0.5.1

## [0.5.0] — 2026-09-24

Fixture replay (`runkite sim`) and opt-in connector MCP payload shrink. Preview (BUSL) -- not a 1.0 production claim, and **not** a hosted control plane.

### Added
- Fixture replay: `runkite sim -f fixtures.yaml` creates runs with `X-Runkite-Simulation` (admin-gated). Spend holds, usage ingest, and UTC-day run caps skip; concurrent admission, rate limits, kill, and connector policy stay. Admin `sim` badge. YAML matcher for deny/pending audit rows (SQL). See [docs/sim.md](docs/sim.md).
- Opt-in connector MCP payload shrink (`payload_shrink` in `langgraph.json`, default off). Over-cap `tools/call` results stash in Redis; the runner sees a preview plus `runkite_retrieve_payload`. Not a token SLA; audit keeps the preview, not the original body.

### Notes
- No Python or TypeScript runner logic change -- version lockstep only.
- Fixture replay is in this tag. CI uses an admin key on purpose (same blast radius as Admin).
- Payload shrink is off by default. Enabling it means durable audit keeps the preview plus ref, not the original MCP body. No Redis = no shrink.
- **Self-host only.** A managed/hosted control plane is not in this release.
- **Supported HA:** Postgres + Redis. Kubernetes/Helm is Compatible (kind + EKS smoke, not a multi-hour cloud HA soak).
- Governance + FinOps durability remain SQL-only; Mongo returns `501` / fail-closed on those routes.
- Runner Protocol stays independently versioned (Draft 0.1.0). Agent Protocol OpenAPI remains 0.1.6.
- Limitations: [`docs/limitations.md`](docs/limitations.md)

[0.5.0]: https://github.com/getrunkite/runkite/releases/tag/v0.5.0

## [0.4.1] — 2026-09-15

Security patch: connector HITL one-shot is bound to this `tools/call`'s argument digest. Preview (BUSL) — not a 1.0 production claim, and **not** a hosted control plane.

### Added
- `pending_actions.args_digest` / `args` / `decided_by` (schema 14); consume and find match the digest
- Admin Pending **Args** column; approve records `decided_by` when Admin auth carries an identity

### Fixed
- Approving a $500 transfer no longer forwards a $5,000,000 retry on the same tool (consume used to match `(run_id, generation, connector, tool)` only)
- `google.golang.org/grpc` 1.83.1 → 1.83.2 (already on `main` before this tag)

### Notes
- No Python or TypeScript runner logic change — version lockstep only.
- Consume still skips Decide so mandatory HITL cannot re-pending the approved retry. Digest match *is* the retry check.
- Rows created before this binding (`args_digest` empty) still consume once on the old tuple.
- Display map omits secret-looking keys; approve re-eval uses that map. Predicates must not key on `password|token|secret|authorization|api_key`.
- Fixture replay (`runkite sim`) is not in this tag.
- **Self-host only.** A managed/hosted control plane is not in this release.
- **Supported HA:** Postgres + Redis. Kubernetes/Helm is Compatible (kind + EKS smoke, not a multi-hour cloud HA soak).
- Governance + FinOps durability remain SQL-only; Mongo returns `501` / fail-closed on those routes.
- Runner Protocol stays independently versioned (Draft 0.1.0). Agent Protocol OpenAPI remains 0.1.6.
- Limitations: [`docs/limitations.md`](docs/limitations.md)

[0.4.1]: https://github.com/getrunkite/runkite/releases/tag/v0.4.1

## [0.4.0] — 2026-09-14

Argument-aware connector policy: MCP `tools/call` arguments reach Decide, config predicates, webhook, and audit. Preview (BUSL) — not a 1.0 production claim, and **not** a hosted control plane.

### Added
- Connector MCP `tools/call` arguments bound into policy Decide (SHA-256 digest for cache / webhook / audit; size-capped, secret-stripped display map)
- Config `policy.predicates` — deny or pending from argument values after a matching grant and before the sync webhook (never allow; restart to reload; no Admin CRUD)
- Bounded expirable LRU for policy decisions (max 10_000); cache key includes `ArgsDigest`; `pending` is not cached
- Admin Audit **Args** column; Grants / Pending copy points operators at `langgraph.json` for predicates
- Operator how-to: [Grants & HITL](site/support/grants.html#argument-predicates)

### Changed
- Policy webhook `data` includes `args_digest` plus capped `args` on `tool.call`
- `tool_auth` and SIEM carry the digest only

### Notes
- Connector MCP, pre-call only — not in-graph `AuthorizeTool`, not tool-result inspection. Pending rows still store connector / tool / reason; the triggering arguments are on the audit event.
- Fixture replay (`runkite sim`) is not in this tag.
- **Self-host only.** A managed/hosted control plane is not in this release.
- **Supported HA:** Postgres + Redis. Kubernetes/Helm is Compatible (kind + EKS smoke, not a multi-hour cloud HA soak).
- Governance + FinOps durability remain SQL-only; Mongo returns `501` / fail-closed on those routes.
- Runner Protocol stays independently versioned (Draft 0.1.0). Agent Protocol OpenAPI remains 0.1.6.
- Limitations: [`docs/limitations.md`](docs/limitations.md)

[0.4.0]: https://github.com/getrunkite/runkite/releases/tag/v0.4.0

## [0.3.0] — 2026-09-05

Self-host preview: FinOps, opaque adapter checkpoints, plane-side secrets/RLS/`allowed_tools`, Admin playground + Spend, docs rewrite. Preview (BUSL) — not a 1.0 production claim, and **not** a hosted control plane.

### Added
- FinOps continuum on SQL: pricebook, daily USD/token/run caps, reservation holds, routing aliases, alerts/export, cancel-on-hard-breach, Admin Spend + live overlay editor
- Universal opaque checkpoints for non-LangGraph adapters (`adapter-state`); proxy HITL/CAS still for LangGraph
- Per-graph `allowed_tools`; connector `auth.secret_ref` (env/file/Vault); Postgres RLS fail-closed
- Run manifest snapshot at dispatch; Admin Try-agent playground, registry editor, DocsLink
- Kind K0–K3 ops proofs plus a named EKS smoke/reclaim
- Docs site rewrite, Admin UI 15-screen guide, Gemini dogfood QA hub
- Admin login rate limit (TCP peer, not `X-Forwarded-For`); GitHub Actions SHA-pinned

### Fixed
- Store `ListNamespaces` suffix on every backend; Python store `batch()` fail-fast on the store loop
- Proxy checkpoint lock map capped; RunDetail SSE list capped
- Alpine runtime image digest-pinned; Docker `GIT_COMMIT` is a release build-arg

### Notes
- **Self-host only.** A managed/hosted control plane is not in this release.
- **Supported HA:** Postgres + Redis. Kubernetes/Helm is Compatible (kind + EKS smoke, not a multi-hour cloud HA soak).
- Governance + FinOps durability remain SQL-only; Mongo returns `501` / fail-closed on those routes.
- Runner Protocol stays independently versioned (Draft 0.1.0). Agent Protocol OpenAPI remains 0.1.6.
- Limitations: [`docs/limitations.md`](docs/limitations.md)

[0.3.0]: https://github.com/getrunkite/runkite/releases/tag/v0.3.0

## [0.2.0] — 2026-08-10

Governance-preview cut: plane policy + Admin governance on SQL backends, announce-bar proof, packaging smokes. Preview (BUSL) — not a 1.0 production claim.

### Added
- Fail-closed connector policy (grants, sync webhook PDP, durable SQL audit)
- Connector HITL (`pending` → Admin approve → one-shot agent tool retry)
- Mandatory HITL overlays, kill/pause switches, time-bounded break-glass
- Run admission (agent-scoped authz, concurrent/daily `admission_limits`)
- Admin UI pages: grants, mandatory HITL, pending, kill, break-glass, audit
- `make smoke-governance` / CI announce-bar coverage on Postgres
- Kind Helm packaging smokes (`kind-helm-smoke` … `kind-helm-net`)
- MCP connector session tokens; Redis-shared Admin sessions; Helm pod TLS wiring
- Trust notes: [`docs/trust-governance.md`](docs/trust-governance.md)

### Fixed
- Policy decision cache key includes `principal` (no Alice→Bob allow leak when `cache_ttl_ms` > 0)

### Notes
- **Supported HA:** Postgres + Redis (Compose soak). Kubernetes/Helm remains **Compatible** (kind install/ops smokes; paid EKS deferred).
- Connector HITL approve is Admin-only; agent must retry the tool after approve (not Agent Protocol interrupt/resume).
- Mongo is not equal for durable governance (audit / grants / pending → `501` / fail-closed).
- Limitations: [`docs/limitations.md`](docs/limitations.md)

[0.2.0]: https://github.com/getrunkite/runkite/releases/tag/v0.2.0

## [0.1.1] — 2026-08-06

### Changed
- Richer PyPI / npm package READMEs and clearer root README links
- GitHub Release notes lead with download links (assets still listed by GitHub below)
- Helm default image tags use `:latest` for floating pulls

[0.1.1]: https://github.com/getrunkite/runkite/releases/tag/v0.1.1

## [0.1.0] — 2026-08-06

First public preview release of Runkite.

### Added
- Go control-plane binaries via GitHub Releases (linux/darwin, amd64/arm64)
- Container images on GHCR: `ghcr.io/getrunkite/runkite`, `runkite-runner`, `runkite-runner-ts`
- PyPI package `runkite-runner` (Python LangGraph runner)
- npm package `runkite-runner` (TypeScript / LangGraph.js runner)
- OpenAPI specs attached to the GitHub Release
- Helm chart defaults pointed at GHCR images

### Notes
- Preview quality: use the Supported profile (Postgres + Redis + auth) for anything serious.
- Honest gaps: [docs/limitations.md](./docs/limitations.md) release summary.
- Site: https://getrunkite.github.io/runkite/

[0.1.0]: https://github.com/getrunkite/runkite/releases/tag/v0.1.0
