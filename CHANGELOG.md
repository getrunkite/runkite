# Changelog

All notable releases are documented here. Version source of truth: [`VERSION`](./VERSION).

## Unreleased

### Added
- An unreleased disposable multi-plane recovery harness checks concurrent cache retries, positive synthetic usage without replay billing, cross-replica cancel, process-loss reclaim and retained raw SSE. Reports distinguish the short local rehearsal from the 30-minute announce bar and cloud HA.
- The unreleased application UI bridge supports managed Microsoft, OpenAI Agents, Pydantic AI and Google ADK Python runs through the existing AI SDK UI transport. Server-selected profiles validate the current turn, durable framework and generation before finish; Pydantic final JSON uses a separate result part. Includes Next/Nuxt route recipes and authenticated application route hooks, retaining app-owned UI storage, reconnect and explicit Stop.
- Managed Google ADK Python profile (`python-google-adk`) for one native text agent, local function tools, session-local history, cancellation/usage and CAS receipt recovery. The source-built `google-adk` extra pins `google-adk==2.13.0`; its installed command is `runkite-runner-google-adk`. Shared app/user state, native confirmation/resume, workflows, delegation, memory, artifacts and live audio are outside the first profile. This is unreleased and absent from published PyPI `0.5.8`.
- Managed Pydantic AI Python profile (`python-pydantic-ai`) for native text and validated JSON results, conversation persistence, cancellation/usage and CAS committed-turn receipts. The source-built `pydantic-ai` extra pins `pydantic-ai-slim==2.63.0`; the installed CLI is `runkite-runner-pydantic-ai`. Provider packages remain application choices. This profile is unreleased and is absent from the published PyPI `0.5.8` artifact.
- Admin Run Detail displays the saved output already returned by its API, including structured agent results after the live stream has ended.
- Managed OpenAI Agents SDK Python profile (`python-openai-agents`), with a per-run native `Agent` factory, text streaming, local function tools, native history, usage/cancel and CAS committed-turn recovery. The optional `openai-agents` extra pins `openai-agents==0.24.0`; the installed CLI is `runkite-runner-openai-agents`. Includes an offline example and authenticated installed-wheel integration tests. This is unreleased source packaging; existing published PyPI `0.5.8` does not include this profile.
- Managed Microsoft Agent Framework Python runner (`python-agent-framework`): factories, text streaming, local tools, durable `AgentSession`, cancellation/usage/tenant isolation, and CAS committed-turn recovery. Offline example under `examples/managed_agent_framework`. Decision-only governance middleware remains separate.
- Optional Microsoft install profile in the same `runkite-runner` wheel and sdist: `agent-framework` extra pinned to `agent-framework-core==1.23.0`, installed `agent_framework_adapter` module and dedicated `runkite-runner-agent-framework` CLI. This is unreleased source packaging; package metadata remains `0.5.8`, and the published PyPI `0.5.8` artifact does not contain it. The four older adapters remain checkout-only; the stock Python image does not install this extra.
- The managed AI SDK application bridge can detach browser delivery and reattach an owned run through GET without creating another run. The pinned native transport consumes a full replay with `resumeStreamIsReplay: true`. The app retains authentication, UI-message storage and its chat/turn-to-run mapping.
- Explicit managed-run Stop helper, separate from stream disconnect. Create defaults to cancel; detach requires a saved run ID. Attach defaults to detach. The app persists the first assistant ID before display and supplies it as a reconnect precondition. Reconnection is limited to complete retained history from one attempt; missing start, replay gaps and expiration fail without fabricating a transcript or re-executing tools.

### Fixed
- Bundled multi-plane runners select HTTP proxy checkpoints so default cron thread IDs satisfy the native identity boundary. The soak sink port now reaches listener and fixture URLs consistently; loaders accept optional API-key auth for test overlays. Existing native history is not imported automatically.
- In-process Nack/reclaim retains the original lease and generation on full-buffer or cancelled ready-write failure. Handoffs do not block under the queue lock, and reclaim counts/advances only successful ready writes. Local memory still has no process-restart durability.
- Authenticated dispatch validates the actual dequeued assignment kind before durable attempt advancement or input delivery and generation-Nacks a misroute. NATS consumer creation reuses identical same-kind configuration but refuses conflicting filters from colliding derived durable names; existing names and no-token local behavior stay unchanged.
- NATS queue/event/cancel operations reject run IDs that could become wildcard filters or invalid JetStream KV keys. Run creation returns 400 before thread/run/cache mutation on this backend. Event and cancel consumers require exact subjects. Valid keys retain their layout; unsupported legacy queued assignments stay retained with delayed negative acknowledgement for operator reconciliation. This Compatible-backend boundary does not restrict Postgres/Redis IDs.
- Built-in cache hits commit admission, run output, merged thread state and checkpoint together. Failed writes no longer leave partial successful runs. Replay does not bill original tokens or refresh source TTL; matching concurrent retries reuse the committed quota winner. Custom stores without optional `CachedRunCreator` use ordinary dispatch.
- Run and checkpoint insertion verify exact parent tenant ownership inside the write transaction. Mongo thread deletion and tenant-scoped child cascades commit together, preventing delayed writes/deletes from affecting a replacement tenant. Historical malformed links are not repaired automatically.
- TypeScript pre-execution status checks include the assignment tenant, and both SDKs encode custom run IDs in that URL. Task-local identities remain exact. Unicode/legacy header-unsafe identities use additive base64url HTTP headers; upgrade the plane before runners. New run IDs with controls or outer whitespace return 400.
- Direct Postgres LangGraph now refuses every non-default tenant before factory/state access: even a delimiter-safe new named key can read older default-tenant history. Named LangGraph tenants must use HTTP proxy checkpoints and explicitly resolve prior native state. Safe default direct keys remain readable; fresh memory retains its delimiter guard. Default cron threads need proxy mode. No owner marker, automatic import or blank fallback is provided; managed opaque SDK profiles are unaffected.
- MySQL schema version 17 replaces insensitive identity/security-column comparison with binary NO PAD comparison, preventing case/accent tenant and resource collisions. Existing installations require a verified backup and a stopped-writer maintenance upgrade. Preflight rejects aliased references and inconsistent tenant ownership; interrupted DDL resumes, unsafe downgrade is refused, and historical overwritten data cannot be recovered automatically. See the [upgrade procedure](docs/ops-runbook.md#hardening-upgrade-unreleased).
- Unreleased approval hardening preserves exact numeric arguments and JSON types, rejects ambiguous MCP/decision input, versions argument hashes, removes legacy empty-digest approval wildcards, and prevents prior approval from bypassing current policy deny. Old pending/approved operations require fresh submission and review; see the operator upgrade guide.
- Negative acknowledgements now fence the dispatch generation on every queue. Redis returns jobs atomically without dropping a lease on write failure; in-process assignments no longer alias reclaimed generations; reclaim leader identities are unique per loop.
- Completion checkpoints contain merged thread state while run outputs remain per-run. Pending argument reads retain JSON numbers. Built-in stores use indexed dead-letter ID lookup so old records remain replayable.
- Connector helpers use one bounded operation, cancellation and immutable run/request snapshots. Automatic renewal requires an explicit pre-dispatch session rejection; upstream 401 is an ambiguous 502 and is not replayed. Heartbeat RPCs are bounded and stopped calls cannot deliver late supersession.
- Credential and decision transports refuse redirects, including Vault and vector clients. Webhook auth caches the full forwarded context, requires bounded complete successful responses and honors cancellation. Vault paths are validated after decoding. Policy/preflight reject truncated or failed response reads.
- Logout requires confirmed session revocation. Admin polling cannot overwrite newer state, SSE decoding preserves framing/whitespace with bounds, debug previews are capped, and resource IDs are encoded. New dot-only IDs are rejected; managed chat intake is bounded before dispatch.
- The unreleased managed Microsoft runner enforces the dispatched `allowed_tools` snapshot before native local function execution. An omitted list remains unrestricted, an empty list denies all tools, and forbidden calls fail the run with `tool_auth` denial while retaining observed usage and previous committed history. Custom clients without the supported invocation boundary reject a configured restriction before model execution; this does not add connector policy or native approval/resume.
- Application JSON numbers retain their numeric value through managed completion, thread state/history, store items, cached results and wait responses. Retry identity no longer treats neighboring large integers as equal. SQLite and Postgres preserve numbers without a Go float conversion; MySQL rejects values its native JSON conversion would change, and Mongo uses exact native int64/Decimal128 or rejects unsupported precision. This prevents future rounding and does not reconstruct previously rounded records. JavaScript clients still need their existing safe-number guard or a lossless parser.
- Run SSE, thread SSE and WebSocket streams confirm terminal events through the existing durable completion fence. A superseded terminal no longer closes a replacement attempt's stream; early broker closure uses retained replay and durable-state checks. Event IDs preserve attempt boundaries, and sequence cursors do not hide a replacement's restarted sequence. Raw nonterminal output remains advisory during reclaim and must not be merged across attempts.
- Managed Microsoft runs retain model identity with observed usage so the configured pricebook can estimate spend. Success, cancellation, errors and committed-turn replay preserve that attribution without double counting. Unknown or mixed model totals remain unpriced and retain token accounting.
- Managed Microsoft Responses streams accept final confirmations of already streamed text without appending them twice, including multiple tool rounds. Genuine replacement text remains unsupported and fails explicitly.
- Durable dispatch now marks runs `running` in the same transaction as their attempt generation. Active workers no longer remain `pending` in Admin/API records. Terminal and superseded attempts remain fenced.
- Generic Python worker events use compact UTF-8 JSON on the wire. Non-ASCII output that fits the managed event limit no longer expands past gRPC's receive cap through ASCII escapes, including committed-turn receipt replay.
- Managed Microsoft runner uses `RUNNER_TOKEN` consistently for gRPC and checkpoint HTTP, and the offline config routes to `python-agent-framework`. Invalid, foreign or unversioned native state fails before execution. Receipts validate their graph and output, replay with the current attempt generation, and enforce output bounds before commit. All accepted text messages reach the native agent; unsupported approval/resume fails explicitly. Cancellation and provider failures retain observed usage and the last committed session. Stream and checkpoint clients close on every path.
- Control-plane cancellation retains usage already delivered by the current attempt without committing unfinished conversation history. The transaction drops that usage if reclaim advances the attempt before cancellation commits. Usage not yet delivered when cancellation starts cannot be recovered.
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
