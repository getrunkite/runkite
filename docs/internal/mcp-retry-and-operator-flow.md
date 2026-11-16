# Retry-safe MCP and a clearer operator workflow

Implemented in v0.9.0. The tagged artifacts and current availability are tracked
in the [release verification guide](../releasing.md#v090-release-verification).
The source qualification below records its actual tested scope; publication does
not establish a live-provider soak or independent security assessment.

<a id="caller-and-existing-implementation"></a>

## Caller and starting point

An MCP client can lose a response after its Runkite run was created. Retrying
the same request must be able to observe that run instead of starting another
model/tool cycle. The REST API already supports caller-supplied `RunCreate.RunID`,
`findRunForRetry`, exact-JSON input comparison, concurrent-insert handling and
durable existing-run observation. Before this change, the public MCP tool
accepted only `message`, `input` and `thread_id`, so that entry point could not
use those contracts.

An operator investigating a managed connector action already had its exact
approval ID, but global Admin sent them through a filtered list. Tenant Review
already opened the existing approval detail directly. The same Admin already
showed inputs, raw output, run manifests, usage, delegated runs and recorded
connector actions. There is no need for another approval store or timeline.

A developer evaluating app-owned governance can already run the AI SDK note
example. Its application owns a durable proposal, exact saved arguments and
idempotent local execution. The evaluation gap is a guided path that does not
require a paid model account or confuse app-owned actions with managed runs.

## Scope and design choices

1. Add an optional stable `run_id` to public MCP agent tools and reuse the REST
   retry machinery. Check existing runs before generating a default thread.
   Recover concurrent matching requests through the existing race path. Use a
   context-based durable observer for an already-created run, not the
   nil-assignment cache-hit shortcut in `waitForRunResult`.
2. Recheck current agent execution permission before returning a retained run
   through a create-and-retry path. Observing a retry must not bypass authority
   because creation admission is no longer reached. Local no-auth compatibility
   remains deliberate; scoped authenticated callers retain their boundaries.
3. Link a connector action to its immutable approval ID using the existing
   detail route. Keep the broader approval list as secondary navigation and
   preserve the tenant Review route and API authorization.
4. Show a conservative plain-text final-answer preview for supported successful
   output shapes alongside raw JSON. A preview is presentation only. It cannot
   turn tool proposals, reasoning, intermediate messages or ambiguous custom
   output into a final answer.
5. Add an explicitly scripted mode and guided continuation story to the
   existing application-owned AI SDK example. The example must continue to call
   the real self-hosted decision API; only its model response is scripted.

## Boundaries to preserve

- An omitted MCP `run_id` keeps the existing new-run behavior. Callers must
  persist and reuse their own operation ID to request a retry.
- Retry equivalence includes the requested agent, any explicit thread and
  exact JSON input. A conflicting reuse must not return an unrelated result.
- Delivery cancellation does not prove that the worker stopped or that an
  external side effect did not happen. Reattachment must observe durable status
  and must not fabricate success for pending, interrupted, error or timeout.
- Retained run identity is the retry record. Pruning/deleting it ends that
  retry protection. A reused thread ID alone is not operation idempotency.
- Run insertion and queue enqueue remain separate operations. This increment
  does not promise atomic accepted dispatch or exactly-once external effects.
  Concurrent first calls that omit a thread may leave an extra idle generated
  thread, while retaining one run and one assignment. Cleanup after canceled
  admission uses a five-second uncanceled context with the original tenant and
  identity; a database outage can still prevent cleanup.
- Incoming `/mcp` invokes Runkite-managed agents. It is distinct from the
  run-bound outgoing connector proxy and from `/governance/decide`.
- App-owned governance still creates no managed Thread/Run, applies no run
  budget/kill/break-glass control and does not execute a tool after approval.
  The app authenticates continuation and owns its execution deadline and
  side-effect idempotency. Pending expiry is not expiry of approved authority.
- Scripted demo output must be labelled and selected through trusted server
  configuration. A browser cannot switch a real model deployment into demo
  mode or supply an approval result.
- Raw output remains available. Preview text is escaped, bounded and rendered
  without HTML execution or a new Markdown dependency.

## Why this fits the control plane

The public Trinity implementation made the path from request to review to
result especially tangible. The useful lesson is to expose existing authority
and durable results clearly, with safe trigger semantics. Runkite keeps its
framework-neutral boundaries and reuses its stores, routes and policy engine.
It does not embed another product's runtime, scheduler, vault, memory system or
UI. A Trinity delegation example would be separate compatibility work through
public MCP; it is not implied by these changes.

## Qualification and release gates

The qualification record for this candidate must include:

- MCP legacy calls, equivalent retries, concurrent duplicates, conflicting
  agent/thread/input, scoped-key denial, cross-tenant isolation, disconnect and
  reattachment, and durable successful/failed/timeout results.
- REST retry regressions for the shared observation and permission checks.
- Exact approval navigation with custom IDs and unchanged tenant Review scope.
- Preview extraction from actual runner shapes, unsupported/ambiguous output,
  tool/reasoning exclusion, oversized text and escaped hostile markup.
- The scripted note proposal, pending review, approval/denial, continuation,
  repeated continuation, unavailable plane and real-provider default behavior.
- Admin production build, relevant Go and SDK tests, documentation links,
  OpenAPI drift and coordinated version metadata.

### Executed local qualification, 2026-11-16

- Go 1.26.11, `CGO_ENABLED=0`: full `./internal/... ./cmd/...` suite passed
  1,145 top-level tests, with 127 environment-dependent or opt-in skips and no
  failures. The API suite includes official-SDK HTTP MCP calls and rejection of
  a revoked write permission on an existing MCP session. Focused MCP retry
  regressions also passed ten consecutive runs.
- Admin: 93 tests passed, TypeScript and production build passed. Lint passed
  with inherited Fast Refresh export warnings. Browser checks used the actual
  built bundle against an isolated SQLite-backed API: exact approval links,
  raw-only structured output, literal hostile markup without injected elements,
  no initial event-replay page jump, and no horizontal overflow at 390 and 320
  pixels. The preview is plain text, so Markdown syntax remains literal too.
- TypeScript runner: 292 passed, two binary-dependent checks skipped. Governance
  source checks passed 35 helper tests and 20 pinned AI SDK example tests.
- The scripted application used the real SQLite policy, review and audit
  handlers with separate app, reviewer and operator credentials. Approval alone
  created no note; continuation created one, repeated continuation returned the
  same result, and a turn replay retained its original reply. Human rejection,
  policy denial and an unavailable plane produced no further notes. App-owned
  calls created no managed run. This fixture was local and temporary, with no
  model-provider or real connector dispatch.
- Both installed governance distributions passed outside the checkout: npm
  exact package members, helper tests and all 20 AI SDK example tests; Python
  wheel and an offline rebuilt source distribution passed runtime, metadata and
  license checks, including the optional Agent Framework boundary. Local Python
  build tools matched the release pins except cached pip 26.2.2 rather than
  26.3. These were candidate builds, not uploads.
- OpenAPI integrity passed with 169 Go routes and no spec changes. Coordinated
  v0.9.0 metadata and all seven version-gate tests passed. The static handbook
  and interaction tests passed; links were checked across 68 HTML pages.

Hosted Ubuntu qualification remains a separate gate. Local checks do not cover
the Linux race suite, live external backends, container scans, multi-replica
soak or live model providers. A green source test or a prepared tag is not
package publication, and it is not an independent security assessment.

### Rollout

There is no new SQL migration or Runner Protocol change in this increment.
Upgrade every HTTP replica before clients rely on the new MCP `run_id` field,
and verify it is advertised by `tools/list`. Preserve the existing MCP session
routing requirement; reinitialize a session when replacing its server. Do not
infer support from a client accepting an unknown argument. Retention of the
original run record is part of retry protection. See the
[operations runbook](../ops-runbook.md#mcp-and-operator-flow-upgrade-planned-v090)
for the rollout and investigation procedure.
