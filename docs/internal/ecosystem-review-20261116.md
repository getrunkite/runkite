# Public agent projects and the next Runkite improvements

Research date: November 16, 2026. Baseline: v0.9.0 runtime `9c7afb97aa69977385275bce8f5970e34f68f419`, with documentation at `a068c6b89d0cae5337881b73248f6fe865d43745`.

Runkite's strongest next step is to make its existing execution and governance contracts safer and easier for other products to use. Start with narrowly scoped specialist credentials, useful MCP input discovery, honest capability information and schedule visibility. Add a qualified external caller or evaluation adapter after those contracts are clear.

This is a curated survey of 17 public projects, not an exhaustive census or a claim that every upstream feature was tested. The recommendations below record the initial research proposals. Subsequent implementation and release gates are tracked in the [implementation plan](ecosystem-implementation.md).

## What Runkite already has

Existing code already supplies managed dispatch and recovery, versioned agents and registry entries, tenant scoping, connector grants and argument predicates, human review/quorums, run budgets, action evidence, mTLS worker ownership, constrained delegation, Vault-backed credential lifecycle and OTLP export. These are not new roadmap items.

Read [limitations](../limitations.md), [integrations](../integrations.md), [app-owned governance](../app-owned-governance.md) and [runners](../runners.md) before extending them. In particular, app-owned Decide creates no managed run; native local tools do not automatically traverse connector policy; registry publication does not install code; and the external OpenShell profile remains experimental.

## Priority 0: preserve registry metadata during Admin edits

An existing catalog entry can have metadata populated through the API. `RegistryEntry.tsx` currently omits it from both `formFromEntry` and `bodyFromForm`. `handleAdminPutRegistryEntry` then passes a replacement record to `PublishRegistryEntry`, whose SQL update replaces metadata. Saving a description can therefore clear the current metadata. The older immutable version remains available.

Files: `admin-ui/src/pages/RegistryEntry.tsx`, extracted form helpers/tests and the embedded Admin bundle. The small local fix carries the loaded opaque metadata through visible-field edits, preserving existing REST replacement and concurrent-edit semantics. It adds five regressions, registry documentation and an Unreleased note. The local Admin suite passes 98 tests, site suite passes 43 tests, and typecheck/build passes. This patch is not included in v0.9.0.

Risk: low, with the existing loaded-value replacement behavior retained. This fix should precede richer catalog manifests.

## Priority 1: a narrowly scoped specialist caller contract

Caller: a Trinity agent, automation or application that needs one Runkite specialist and its retained result, without general tenant read/write access.

Already present: `internal/auth/agent_perm.go:CanRunAgent`, REST agent-run permissions, v0.9 stable MCP run IDs and durable `waitForExistingRunResult`. The current [Trinity example](../../examples/trinity_specialist_mcp/README.md) explicitly requires a dedicated tenant read/write key. Its fixed client-side target does not narrow that server credential.

Reference: [agentgateway MCP authorization](https://github.com/agentgateway/agentgateway/blob/18f83707118b3e2df500cc63299486958a0e86f5/crates/agentgateway/src/mcp/rbac.rs) distinguishes tool/resource/prompt subjects. Filtered discovery and invocation-time checks are useful patterns.

Proposed change: support existing `agents:<id>:run` with a new, explicit `agents:<id>:observe` grant for a specialist profile. Filter MCP discovery, check invocation authority, and permit only exact run GET/wait observation under that grant. General search, thread history, store access, cancellation and deletion remain separate permissions. Existing run-only credentials must not silently gain reads.

Files: `internal/auth/agent_perm.go`, `internal/auth/auth.go`, `internal/api/mcpserver.go`, `internal/api/runs.go` and the specialist example.

Risk/size: medium, spanning authorization and session lifecycle. MCP callbacks retain initialization identity today; scoped sessions need fresh authority propagation or permission-snapshot binding. Observation must retain a run incarnation, fail after deletion/replacement, and never redispatch. Test permission changes, tenant/agent isolation, separate run/observe grants, concurrent retries and deletion races.

Boundary: agent-scoped observation is not requester-private run ownership. Keep the dedicated tenant/single trusted caller guidance until a separate owner/thread authorization design exists. Do not build another identity issuer or policy engine.

## Priority 2: expose known structured input contracts over MCP

Caller: an MCP client invoking a specialist that requires fields such as a ticket object, rather than a plain message.

Already present: Python LangGraph schema introspection and `internal/api/agents.go:handleReportAgentSchema`. Public MCP still uses the same generic `mcpToolInputSchema` for every agent.

Reference: Langflow derives MCP schema and execution mapping from exposed fields in [flow helpers](https://github.com/langflow-ai/langflow/blob/260cbf347cc5563cc48c79094c175ca31532ea2b/src/backend/base/langflow/helpers/flow.py) and [MCP handlers](https://github.com/langflow-ai/langflow/blob/260cbf347cc5563cc48c79094c175ca31532ea2b/src/backend/base/langflow/api/v1/mcp_utils.py).

Proposed change: retain Runkite's `message`, `input`, `thread_id` and `run_id` envelope, but enrich nested `input` when a usable schema is known. Keep a generic fallback for stubs, unsupported schemas, factories and missing reports. Report provenance honestly: `AgentSchema` currently has no reporter revision or timestamp.

Files: `internal/api/mcpserver.go`, `internal/models/models.go:AgentSchema`, schema reporting and related MCP tests.

Risk/size: small to medium, depending on supported schema dialects and freshness. Test nested `$defs`/references, recursion and bounds; never fetch external references or coerce input. Define session refresh/reconnect behavior because tool catalogs are currently captured at session creation. Publishing a schema does not attest code, prove worker readiness or create a new execution validator.

## Priority 3: show execution capabilities and definition requirements

Caller: an operator choosing a profile, or an application deciding whether a requested operation is supported.

Already present: `Agent.Capabilities`, versioned `RegistryEntry.Metadata`, schema endpoints and detailed profile limits. `bootstrapAgents` initializes capabilities as empty, while Agent Detail already renders the field.

References: OpenHands consumes backend runtime facts in [agent-server-adapter.ts](https://github.com/OpenHands/OpenHands/blob/035cffa0b955bc8c64aca6ce8cd94de2f944dc5a/src/api/agent-server-adapter.ts). Dify's [versioned definition handling](https://github.com/langgenius/dify/blob/7688f7c9b1b46e3c813eccf52bcdc86c1f4f1161/api/services/app_dsl_service.py) makes dependencies and compatibility explicit.

Proposed change: publish an advisory descriptor for configured profile, schema availability, continuity, native resume and checkpoint-ref support. Render unknown explicitly. Add an optional versioned catalog metadata convention for package requirements, connector names and a safe sample input. Preserve opaque metadata first; neither convention should install dependencies or execute a source URL.

Files: `cmd/serve.go:bootstrapAgents`, `internal/models/models.go`, `admin-ui/src/pages/AgentDetail.tsx`, registry form/view and `docs/registry.md`.

Risk/size: small for descriptors and read-only rendering, medium if reports need freshness/version binding. Keep declared configuration, observed capability and qualification receipts distinct. Do not label configured capabilities as live fleet health or sandbox attestation.

## Priority 4: make scheduled execution visible

Caller: an operator asking whether an enabled schedule ran, overlapped another run or failed before dispatch.

Already present: tenant-scoped durable cron claims, timezone parsing, latest-only catch-up, overlap protection and `cron:<name>` run threads. Admin Cron currently emphasizes definitions.

Reference: Agno separates schedules from execution records in [ScheduleRun](https://github.com/agno-agi/agno/blob/3cb5054ae5f8b20b50abe9e3e7364ae1ccf8cee1/libs/agno/agno/db/schemas/scheduler.py) and [ScheduleManager](https://github.com/agno-agi/agno/blob/3cb5054ae5f8b20b50abe9e3e7364ae1ccf8cee1/libs/agno/agno/scheduler/manager.py).

First patch: link each schedule to its tenant-correct existing run thread and show the latest durable run. Calculate any next-fire preview with the scheduler's actual parser/timezone semantics.

Files: `admin-ui/src/pages/Cron.tsx`, `internal/api/server.go:handleListCronSchedules`, `cmd/cron.go` and `internal/api/runs.go:DispatchScheduledRun`.

Risk/size: small for navigation. Skipped overlaps and pre-run failures need separate per-fire metadata receipts and retention, a medium follow-up. Do not invent failed runs for work never created, add automatic retries or replace intentional latest-only catch-up with backlog replay.

## Follow-on product work

### Offline policy regression fixtures

Caller: an administrator checking a proposed grant or HITL change without executing tools.

Archestra separates document validation from installation in [guardrailsPolicyService.validate](https://github.com/archestra-ai/archestra/blob/cd4e2901c39b0f313dfb5a34568c2a99f14f6764/platform/backend/src/services/guardrails-policy.ts). Its policy-scenario tooling is a useful operator pattern. Its model-assisted policy runtime, annotators and waiver behavior should not be copied into Runkite.

Runkite already has policy unit/conformance tests and `runkite sim`. The latter deliberately creates real runs and can call real allowed connectors. A separate nonexecuting CLI could reuse the current policy engine and fixture expectations to test declared identities, arguments, grants and expected decisions against a controlled snapshot. It must make no model, Vault or downstream calls. Logical policy tests do not prove runtime enforcement or remote business effects.

Files: `internal/policy`, `cmd/sim_parse.go`, `cmd/sim_expect.go` and a separate CLI entry in `cmd/main.go`. Medium feature. Keep live simulation unchanged.

### Qualified external callers and adapters

- **Langflow**: first low-cost caller recipe. Its [MCP Tools client](https://docs.langflow.org/mcp-client) supports remote servers and headers. Point it at Runkite `/mcp`, qualify discovery and a real specialist call, then consider upstream documentation. A model-generated run ID is not a durable receipt; qualify retries separately.
- **promptfoo**: provide an [evaluation provider](https://www.promptfoo.dev/docs/providers/custom-api/) using public create/get/wait. Reuse stable operation IDs for transport retries, fresh IDs for intentional repeats, and normal governed runs. `runkite sim` is not an output-quality evaluator. Add a provider and deterministic fixtures under `examples/promptfoo`; do not embed an eval studio or grant CI Admin access. Small to medium.
- **AG-UI/CopilotKit**: add an app-side text transport using the public [AG-UI encoder](https://github.com/ag-ui-protocol/ag-ui/blob/2330368a2a74573902858867072406638ba3073d/sdks/typescript/packages/encoder/src/encoder.ts). Reuse the existing AI SDK bridge's application ownership, durable finish, attempt checks, detach and explicit Stop. Start with text and terminal lifecycle only. Medium adapter; no frontend runtime inside the Go plane.
- **Temporal**: a customer-owned Activity can call a Runkite specialist. The [Activity SDK](https://github.com/temporalio/sdk-typescript/blob/4a5ee3476e20bcce0bde088ab035792a298118b5/packages/activity/src/index.ts) stays outside Runkite. Persist operation identity before submission; retries observe retained work, and missing retention fails closed. Temporal owns the business process, Runkite the specialist. Example only, after a real caller needs it.
- **Langfuse**: qualify a deployment recipe using existing OTLP export. Its [current endpoint contract](https://langfuse.com/integrations/native/opentelemetry) requires HTTP rather than Runkite's default gRPC. Configure OTLP over HTTP, trace endpoint and headers, optionally with collector mapping. Go/Python use protobuf; the current TypeScript HTTP exporter sends JSON, which Langfuse also accepts. Do not add a dedicated exporter or claim prompt capture from metadata spans. Small configuration/qualification task.
- **Cloudflare Agents**: an existing Worker can be an MCP caller, using its [public client API](https://developers.cloudflare.com/agents/model-context-protocol/apis/client-api/). Store intent and credentials in the application and keep Runkite self-hosted. This does not make the whole Worker conversation a managed Runkite run. Defer the example until a consumer asks for it.

Each adapter belongs initially in Runkite's examples or the caller application's repository. Upstream documentation changes should follow an actual pinned interoperability test. No upstream fork or unsolicited public issue is needed to evaluate the boundary.

## Additional projects screened

- [Trinity](https://github.com/abilityai/trinity): existing v0.9 specialist example is the baseline. Complete native runtime qualification before expanding support claims.
- [IBM ContextForge](https://github.com/IBM/mcp-context-forge): study explicit pre/post tool hooks. A post-result inspection seam is optional medium work, best supplied by an existing external target gateway first. A blocked response cannot undo a tool effect. One inspected output-length plugin path was unavailable at the observed current commit; do not promise that plugin as a current integration.
- [NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell): isolation, usage frames and lifecycle qualification already exist experimentally in Runkite. The follow-on [effective-policy qualification](openshell-effective-policy.md) uses the actual official 0.1.8 checker and matching current/loaded policy snapshots around a fresh provider-free fixture. It adds no production enforcement gate, atomic exec binding, continuous-kernel proof or general provider-state attestation. Compatibility and measured local/hosted results remain explicit in that note.
- [Deep Agents](https://github.com/langchain-ai/deepagents): Runkite already has a LangGraph-based example. Its constructor returns a compiled graph; another runner kind would duplicate existing machinery.
- [Spritz](https://github.com/textcortex/spritz): its [ACP gateway](https://github.com/textcortex/spritz/blob/4e223cd8207a05e213a1595fb29e1ffee73a5f26/api/acp_gateway.go) separates conversation authorization and runtime connection. Useful reference for a future user-owned coding-agent product, not a reason to add a Kubernetes instance controller now.
- [Agent Client Protocol](https://github.com/agentclientprotocol/agent-client-protocol): investigate an external runner adapter only for a real coding-agent caller. Session/permission semantics and version negotiation need their own conformance. This ACP is distinct from Runkite's Agent Protocol and does not guarantee that every native tool requests permission.

Together with Langflow, OpenHands, Dify, Agno, agentgateway, Archestra, AG-UI, promptfoo, Temporal, Langfuse and Cloudflare Agents above, these form the 17-project survey.

## Proposed next release boundary

Ship the metadata fix, scoped specialist authority, typed MCP discovery and a small capability/cron UI increment as independently reviewable slices. Qualify one Langflow caller example against those contracts. These changes make an existing specialist safer to delegate to and easier to discover, use and operate.

Keep offline policy testing, promptfoo and AG-UI as follow-on slices with distinct acceptance tests. Avoid bundling several new runtimes and transport contracts into the same first release. No release number or public availability label changes during this research.

Rejected scope: another vault or identity issuer; a second PDP; a general workflow scheduler; universal conversation memory; a copied visual app builder; automatic code installation from the registry; automatic tool replay after unknown delivery; and claims that traces, a protocol adapter or a model-based filter can stop every agent action.

## Evidence and verification boundaries

Observed repository heads and individual file hashes are retained with the task research outputs. The pinned code links above were read at those commits through GitHub's public API. Some initial documentation reads used cached mutable `main`; they are references for investigation rather than tested release compatibility. No upstream runtime or provider integration was executed during the survey.

At the end of the initial research, the metadata patch was a separately tested local prototype. The research itself changed no release artifact, dependency lockfile or public site. See the implementation plan for subsequent work.
