# 4. Integrations, discovery and operator truth

[Notebook home](README.md) · Previous: [authority and evidence](03-authority-credentials-and-evidence.md) · Next: [proving and shipping](05-proving-and-shipping-the-system.md)

Reading baseline: v0.10.0, source `34f04219`. This chapter connects the current
implementation to decisions I need to recall when extending it. Explanations
of motivation are design inferences from code and tests, not a reconstruction
of private founder conversations.

The recurring problem is deciding what an observation permits us to conclude.
A caller can discover a specialist without owning its execution loop. An agent
definition can describe capabilities without proving a worker is connected.
A schedule can have a next calendar boundary without promising a dispatch.
Keeping those distinctions visible explains both the integration architecture
and some deliberately restrained Admin behavior.

## Start with the operation being delegated

Consider a support application that wants an existing refund specialist to
inspect a ticket. If Runkite should own the specialist execution, the caller
creates an ordinary managed run. Public MCP is another entrance to that same
operation. In [`newMCPServer` and `mcpToolHandler`](../../../internal/api/mcpserver.go),
configured agents become tools, discovery filters through `CanRunAgent`, and
invocation checks authority again before using `createRunCtx` and the normal
wait path. MCP does not introduce a second scheduler or bypass normal run
admission. Exposing the agent as a tool does not route all of its own native
tools through the connector proxy.

Invocation and observation are separate grants. A specialist's `run` scope
does not acquire REST reads. [`publicRunObservation`](../../../internal/api/run_observation.go)
checks the explicit agent observation authority and, for the scoped profile,
requires a durable SQL run reference. That authority covers the agent's retained
runs in the tenant; it is not requester-private ownership. The reference binds
record identity, not authorization. This matters before sharing one specialist
credential among unrelated application users.

If the application instead owns the refund loop and only wants permission to
execute its local refund function, it should use Decide. The
[`handleGovernanceDecide`](../../../internal/api/governance_decide.go) handler
and independent governance helpers leave execution and pending continuation in
the application. An allow result does not create a Runkite Thread/Run or move
the local function into managed accounting and cancellation. The
[`TestGovernanceDecide_AllowDenyAndPending`](../../../internal/api/governance_decide_test.go)
is a useful entry into this separate contract. Choosing the wrong entrance
would create misleading expectations about who resumes work after approval.

Two other boundaries run alongside execution. OTLP exports observations to an
external backend. Model routing selects a backend for the runner's model
client. The [Semantic Router integration](../../integrations.md#vllm-semantic-router-routes-the-runners-model-calls)
uses an OpenAI-compatible client inside the AI SDK runner: Runkite admits the
run, the SDK owns its loop, and the separate router selects a model. The plane
does not become a model proxy. My inference is that this division avoids
duplicating model policy, credentials and failure handling inside dispatch.
A real requirement to govern individual model requests would require an
explicit new boundary, rather than treating run admission as that mechanism.

Likewise, [`tracing.Init`](../../../internal/tracing/tracing.go) configures an
exporter and W3C propagation; it does not create an authoritative execution
store in the receiver. [`TestOTLPHTTPWire`](../../../internal/tracing/otlp_http_test.go)
checks actual exporter behavior. The [Langfuse guide](../../langfuse.md)
distinguishes ordinary run/LLM/tool spans from generation measurements. A
span name does not supply prompt, token or cost semantics. The receiver's
access controls also do not inherit Runkite tenant permissions. A collector
can map attributes where justified, but inventing missing measurements would
make the integration less useful for diagnosis.

## Adapt the caller without absorbing its product

The external examples translate protocols while preserving those owners.
They remain checkout assets, even where their required plane and runner
features ship in registry packages. Installing a runner does not install
Langflow, promptfoo, a Trinity caller or an application UI bridge.

The [Trinity specialist caller](../../../examples/trinity_specialist_mcp/README.md)
stores intent before its first MCP invocation and recovers through observation
without redispatch. Its receipt binds the intended operation; a saved SQL run
reference detects subsequent replacement. It cannot prove what happened before
the first reference was acquired or after required evidence was pruned.
[`TestBridgeReplacementReferenceBetweenReadsNeverRedispatches`](../../../examples/trinity_specialist_mcp/observation_test.go)
captures the consequence: replacement is a reason to stop, not permission to
create a substitute run. Its protocol qualification does not establish native
Trinity deployment or universal tool governance in Trinity's own loop.

[Langflow's native probe](../../../examples/langflow_mcp/qualify_native.py)
uses the installed LFX MCP client to discover a specialist and invoke saved
arguments after reconnecting. The MCP-only scoped profile needs the specialist
run grant; it does not need REST observation authority merely because another
integration uses it. The visual editor and surrounding model-driven flow are
separate deployment checks. Persisting intent outside the tool invocation is
important: a model supplying a plausible run ID is not durable caller memory.

The [promptfoo provider](../../../examples/promptfoo/runkite-provider.mjs)
makes this distinction particularly concrete. An intentional evaluation call
gets fresh operation identity. A transport problem within that call retains
the original identity. Its `handlesOwnRetries` contract prevents the qualified
native scheduler from replaying a whole side-effecting provider call. Creation
is attempted once; subsequent recovery reads the original run. In
[`lost create followed by not-yet-visible row stays unknown`](../../../examples/promptfoo/runkite-provider.test.mjs),
a missing read after a lost create response remains unknown. Automatically
posting again would confuse unavailable evidence with proof of nonexecution.
This is an application provider over managed APIs, not an evaluator or model
runtime embedded in Go.

The [AG-UI bridge](../../../examples/managed_ag_ui/bridge.ts) translates the
shared application's validated chunks using the official encoder. Its host
still authenticates the user, resolves chat/turn ownership and binds the
assistant identity. `createManagedChatChunks` and `attachManagedChatChunks` in
the [shared bridge](../../../examples/managed_ai_sdk/bridge.ts) retain lifecycle
ownership. The test [“AG-UI finish waits for durable confirmation”](../../../examples/managed_ag_ui/bridge.test.ts)
prevents a native text ending from becoming successful managed completion.
Abort can detach delivery while work continues; explicit authenticated Stop
requests cancellation and cannot undo an external effect. This text slice
does not fabricate tool, reasoning or approval events. Supporting those would
need their own identity and replay contracts, not additional event-name aliases.

## Discovery is a report, not a readiness claim

Back in the support application, a useful input form is worth providing, but
its authority must stay clear. [`mcpInputSchema`](../../../internal/api/mcp_schema.go)
enriches the nested `input` property while retaining the existing message,
thread and operation envelope. It copies the reported schema, relocates local
schema references and refuses remote resolution. Unsupported, invalid or
oversized reports receive a fresh generic envelope. Instance data inside
defaults/examples is not rewritten as though it were schema syntax.

This does not turn discovery into runtime input validation. The
[`TestMCPInputSchemaProtocolPreservesRawInputDefaultsAndRetry`](../../../internal/api/mcp_schema_test.go)
intentionally sends input inconsistent with the advertised requirements and
defaults, then inspects the actual assignment and retry behavior. Input is
forwarded rather than silently coerced. An MCP session keeps its catalog
snapshot; publication recovery does not push an update into that session.
Freshness remains unknown. Adding stricter validation later would be a caller
compatibility decision, not a harmless schema-display improvement.

The same discipline applies to profile descriptions.
[`configuredAgentCapabilities`](../../../cmd/agent_capabilities.go) records
`control_plane_configuration` provenance. Known LangGraph profiles describe
checkpoint-dependent behavior; managed SDK profiles describe successful-turn
HTTP continuity rather than native resume or time travel. Unknown framework
and pool combinations remain unknown. A pool name is not evidence that its
worker implements the apparent framework. Bootstrap preserves opaque keys,
and [`TestBootstrapAgentCapabilitiesPreservesHistoryAndStableVersions`](../../../cmd/agent_capabilities_test.go)
checks that repeated registration does not manufacture new definition versions.

My inference is that conservative unknowns are more valuable than a green
badge assembled from configuration guesses. A caller needing proof of deployed
code or live capacity would need separately defined deployment identity,
freshness and probing semantics. Reusing an advisory descriptor as permission,
readiness or attestation would cross those boundaries without implementing them.

## Preserve metadata without promising collaborative editing

Discovery also depends on not destroying information that the UI does not
understand. Registry metadata is opaque publisher data. An operator changing
the refund specialist's description should not erase unknown requirements or
round a numeric value through the browser's ordinary JSON representation.

[`formFromEntry`, `registryEntrySaveRequest` and `jsonBodyFromForm`](../../../admin-ui/src/lib/registryEntryForm.ts)
carry the raw `metadata_json` fragment. Parsing checks its shape; serialization
uses the original fragment. An older replica lacking that representation is
not allowed to save a rounded reconstruction. The
[production fetch-wrapper regression](../../../admin-ui/src/lib/registryEntryForm.test.ts)
checks the outgoing request, not just an isolated serializer. Exact selectors
and form remount keys also stop a previously loaded record from being saved
under a newly selected tenant or route.

These protections solve different problems from concurrency control. PUT still
replaces the submitted entry. Two editors of the same record can overwrite
one another, and omitting metadata through the API retains replacement
semantics. The [precision contract](../opaque-json-preservation.md) also
distinguishes SQLite/Postgres preservation from MySQL/Mongo limitations;
already rounded values cannot be reconstructed. A validated requirements
installer or conditional revision write would be a separate feature. If
concurrent catalog editing became a real caller requirement, I would look for
an explicit backend-enforced precondition rather than describe this transport
fix as merge or CAS.

## A schedule has several different kinds of truth

Cron makes the distinction between definition and evidence visible. The
[`handleAdminListCronSchedulesAt`](../../../internal/api/admin_cron.go)
projection can add history and preview independently. History resolves the
schedule's exact tenant thread and searches for the latest retained run. It
omits input, output and raw provider errors. A thread can contain another
caller's run, and retention can remove earlier runs. Consequently, “empty”
does not mean “never fired”; “unavailable” means a read failed.

[`resolveCronThread`](../../../internal/api/cron_threads.go) is shared with
dispatch. Named tenants prefer a framed, hashed canonical identity while an
owned legacy thread can preserve conversation continuity. It never adopts a
foreign tenant's thread. The
[`TestAdminCronHistoryUsesScheduleTenantAndLatestRetainedRun`](../../../internal/api/admin_cron_test.go)
also keeps history visible after the schedule's current agent changes. These
choices preserve operational evidence without pretending to build a fire
ledger from whichever run happened to remain.

Preview answers a different question. [`Compile` and `Preview`](../../../internal/cronschedule/schedule.go)
share parsing with the scheduler and project a later calendar occurrence.
Expression timezone precedence, configured-zone UTC fallback and server-local
time remain explicit. Relative `@every` intervals have no preview without
their scheduler anchor. The [calendar and unavailable-state tests](../../../internal/cronschedule/schedule_test.go)
document that distinction. A timestamp predicts neither admission nor worker
availability. If an operator needed proof of every skipped or attempted fire,
the design would need durable per-occurrence identity and outcome recording;
neither another history query nor a countdown supplies it.

## Make integration paths discoverable

The later public presentation follows the same boundaries as the v0.10.0
implementation. An engineer asking how to test an agent or connect an existing
chat UI should not have to infer the answer from a release note or a list of
frameworks. The homepage now introduces those jobs in "Works with your stack";
the [public integrations hub](../../../site/support/integrations.html) sends
the reader to the existing task guide, example and qualification evidence.
The [source integration guide](../../integrations.md) remains the detailed
contract map. Static HTML is published separately from Markdown, so editing a
source note alone does not update what a website visitor sees.

The distinction matters for promptfoo. `RunkiteProvider.callApi` submits an
ordinary managed run; its observation path returns final output to the
evaluator. The bundled configuration's equality assertion expects `ready`.
The real CLI qualification checks that connection with a scripted worker.
It does not run a full automated red-team campaign or inspect the audit log to
prove that a forbidden connector action never executed. A security-oriented
scenario would need those additional observations or a controlled tool backend.
Describing it as "evaluate a managed agent" communicates its current value
without turning a transport test into a security verdict.

Published runtime features, checkout examples, configuration recipes and
experimental fixtures are labeled separately in the public path. This avoids
implying that installing a runner installs another product, that a native-client
test covers its entire UI, or that interoperability is a partnership. A new
integration should add a discoverable task and link the narrow evidence, rather
than place an unqualified logo beside a universal support claim.

## Presentation must not strengthen the evidence

The same rule guides Run Detail. [`finalAnswerPreview`](../../../admin-ui/src/lib/finalAnswerPreview.ts)
requires durable success and a recognizable terminal assistant message. It
does not search backward past a trailing user/tool message or guess an answer
from arbitrary fields. Mixed reasoning/tool shapes stay raw. The
[renderer regression](../../../admin-ui/src/lib/finalAnswerPreview.test.ts)
checks that markup remains plain text, including Markdown-looking links.
This is intentionally less convenient than displaying any attractive string
as the completed answer.

Likewise, a cron link to retained evidence opens a guarded read-only
[`RunDetail` snapshot](../../../admin-ui/src/pages/RunDetail.tsx), with manual
Refresh and without live events, cancellation or child navigation. Those
surfaces cannot inherit a snapshot's record binding by association. The
[`TestAdminRetainedRunReadGuardRejectsReplacementPayload`](../../../internal/api/admin_cron_test.go)
is an answer to that specific risk, not a general stream-fencing claim.

My maintenance rule is therefore to ask what stronger conclusion a new UI or
adapter would invite. If the supporting contract is absent, keep the unknown,
raw value or unavailable state visible until that contract is designed.

## Questions to revisit without rereading everything

1. A specialist call times out, then GET returns missing. May the client create
   another run? Trace the saved intent and unknown-delivery case in the
   [promptfoo tests](../../../examples/promptfoo/runkite-provider.test.mjs).
2. What changes when a schema advertises a default? Read the raw-assignment
   assertions in [MCP schema tests](../../../internal/api/mcp_schema_test.go),
   then distinguish discovery from validation.
3. Which registry bugs are prevented by raw metadata and form identity guards,
   and which stale-editor race remains? Follow the
   [form helper](../../../admin-ui/src/lib/registryEntryForm.ts) into its tests.
4. Can a future cron boundary or an empty history prove execution? Compare
   [preview](../../../internal/cronschedule/schedule.go),
   [history projection](../../../internal/api/admin_cron.go) and the
   [retained-history contract](../cron-run-visibility.md).
