# Known Limitations

Honest gaps, not hidden ones. Use the **Release summary** below in GitHub Release notes and launch posts; the long list after it is the full deep dive.

<a id="unreleased-current-source-boundaries"></a>

## v0.10.0 boundaries

The [integration guide](integrations.md#unreleased-current-source) distinguishes
v0.10.0 runtime features from checkout-only caller examples and qualification
tools. Use matching plane and runner artifacts; the release does not turn
those source assets into registry packages.

- Assignment identity protocol 1 is available in v0.10.0 and requires
  matching planes and runners. Exact run ID, original record identity and
  generation are echoed on bound HTTP and lifecycle RPCs; missing proof cannot
  borrow the current assignment. Event and cancellation namespaces keep records
  separate. This is continuity fencing, not authentication, attestation,
  exactly-once effects or protection from a compromised current holder. Drain
  old assignments and all writers, verify backups, apply Postgres/SQLite migration
  23 or MySQL migration 24, then deploy matching builds. Legacy unbound accounting,
  bare terminal-hook claims and same-ID receipts remain ambiguous; reconcile
  collisions explicitly and use fresh operation IDs for intentional new work.
  No automatic backfill or double-billing repair is provided. Mongo's private
  ObjectID proof leaves public SQL-only capabilities unchanged. Kafka fails closed unless
  `RUNKITE_KAFKA_ALLOW_NONATOMIC_FENCES=true` explicitly accepts non-atomic
  Compatible mode; Redis does not add cross-replica queue CAS. See the
  [contract](internal/assignment-identity.md) and
  [rollout](ops-runbook.md#assignment-identity-upgrade).
- Scoped specialist observation is agent-wide on exact public GET/wait and
  requires SQL run references. It is not requester-private ownership, broad
  read authority, streaming or cancellation. First reference acquisition cannot
  prove prior continuity, and deletion/pruning ends retained-run protection.
- Structured MCP discovery and configured capability descriptors are advisory.
  Schema freshness, deployment identity and live worker readiness remain
  unknown. Discovery does not apply defaults or validate invocation; unknown
  profiles and execution pools do not inherit support from their names.
- Static Python LangGraph schema recovery caches reports once and atomically
  fills only existing rows whose four schema fields are exact bootstrap stubs.
  It leaves useful/partial reports intact and falls back to neither PUT nor a
  new row. Matching plane/worker versions and a supporting store are required.
  The 60-second interval is not a delivery SLA. Startup PUT retains replacement
  semantics, and an older same-kind worker can win the first recovery after a
  reset. No factory, TypeScript, generic-adapter or named-tenant introspection,
  reporter revision/time, deployment attestation or MCP session refresh is added.
  See [schema recovery](internal/schema-publication-recovery.md).
- Optional `mcp.tools_only` narrows the connector proxy's inbound methods to
  initialize, initialized notification, ping and tool list/call. Unsupported
  requests are rejected locally; notifications get an empty 202. It is not an
  outbound SDK protocol gate, content filter or sandbox. Tool results can carry
  arbitrary resources/links and data. Earlier session creation can still acquire
  credentials. New requests check the current profile even with an existing
  session token, but already admitted calls are not retroactively revoked.
  Enable it only after upgrading all HTTP replicas. It changes managed target
  fingerprints to v4; default behavior/fingerprints stay unchanged. See the
  [tools-only contract](internal/tools-only-connectors.md).
- Registry metadata preservation is a loaded-value replacement roundtrip,
  without a merge or concurrent-edit guard. SQLite and Postgres retain exact
  opaque JSON numeric values within their existing limits. MySQL native JSON
  can round precise decimals; Mongo keeps its legacy JSON/BSON boundary.
  Earlier lost digits cannot be recovered. Declared requirements install,
  execute and authorize nothing.
- Exact Admin Agent/Registry selectors preserve supplied tenant bytes. An
  explicit empty `tenant_id=` selects the default tenant; an omitted selector
  retains global Admin API lookup/history behavior. Registry mutations mask
  inherited system authority. Existing entries use `/admin/registry/{name}/edit`,
  including a literal name `new`; `/admin/registry/new` remains creation mode.
  Changing record, tenant or mode resets form/confirmation state and stale or
  unhydrated saves are rejected. This adds neither general identifier
  normalization nor concurrent-edit protection.
- Optional Admin cron `include_preview=true` computes one future calendar
  boundary using the shared backend parser. It is independent of history reads
  and does not claim, dispatch, reserve or guarantee a run. Invalid configured
  timezones signal UTC fallback when used; an explicit expression timezone can
  override that location without a fallback flag. `@every` returns
  `relative_interval_unavailable` because no anchor is read. DST/date behavior follows the pinned parser. Preview is not a skip
  ledger, live countdown or proof of exactly-once effects. See
  [cron preview](internal/cron-next-fire-preview.md).
- `@every` keeps its anchor on not-due polls and advances to a handled fire's
  timestamp rather than poll time after success, a lost equal-fire claim or a
  skipped busy-thread overlap. Retryable errors retain the old anchor. This
  preserves a common starting phase across unequal polls, but fresh staggered
  anchors remain local. No global epoch or one-fire-per-relative-interval HA
  guarantee is added. Claims deduplicate equal timestamps, not different phases.
  Prefer a calendar expression and consistent explicit timezone for a shared HA
  cadence. Preview does not infer relative anchors; deterministic phase tests
  are not native relative-HA qualification. Calendar bookkeeping and existing
  overlap/latest-only catch-up remain.
- Cron shows retained rows on the schedule's thread, including rows another
  caller created. It has no receipt for a skipped overlap or failure before run
  creation. Empty history does not mean never fired. v0.10.0 named-tenant
  routing separates new schedule threads while preserving an existing owned
  legacy thread. Default IDs remain unchanged. Stop all cron writers for a
  coordinated upgrade or rollback; mixed versions can split first-fire history.
  No thread/checkpoint state is merged or migrated, and older global
  schedule-name constraints are not repaired. See the
  [routing procedure](ops-runbook.md#cron-tenant-thread-routing-unreleased).
  Retained-run links open guarded, read-only snapshots with manual
  Refresh. These views expose no live SSE, cancellation or child-run navigation;
  the guard is not an SSE incarnation fence. The view adds no next-fire preview
  or dispatch/retry behavior.
- Offline policy fixtures test a declared static snapshot, not authenticated
  authority, live overlays/webhooks, connector enforcement or tool execution.
  A logical pending result creates no approval. Live simulation has a different
  boundary and can perform real work.
- Exact positive policy identities do not revoke decisions or app-owned
  continuations already handed to an application. Restrictive rules still
  match conservatively; padded configuration can deny more. Pause affected
  callers/continuations while upgrading every replica, resubmit managed reviews
  and discard/resubmit saved app continuations using the
  [policy identity rollout](internal/exact-policy-identities.md). Admin grant
  forms and filters retain exact loaded identities. Tool fields now require
  JSON string arrays, not comma-separated names; invalid input is rejected
  before a write. A blank or empty allow list still means no allow-list
  restriction, not deny-all, and deny entries retain precedence.
- The Langflow proof covers pinned native headless MCP discovery/conversion and
  invocation with a real Python worker. It does not qualify its visual editor,
  browser credential resolution, surrounding model loop or durable caller
  receipt ownership. Dedicated read/write and run-only scoped profiles passed
  separately on integrated source with the same native dependency versions,
  each producing two runs and two real worker assignments. The MCP-only scoped
  fixture grants no REST observation authority and does not qualify that path.
- The promptfoo proof covers its pinned ordinary evaluator with local assertions
  and a scripted worker. Broad read/write and scoped run/observe profiles each
  produced four runs, four threads and four reference-pinned waits; the scoped
  profile also passed six denial probes. Intentional repeats are fresh work;
  unknown delivery uses only read recovery. Arbitrary strategies, external
  retries, real models and upstream UI/server features are outside that proof.
- The AG-UI example is an application-side plain-text transport, not a full
  protocol compliance claim. It requires app-owned identity and state, complete
  retained replay and confirmed durable completion. Detach is not cancellation;
  explicit Stop cannot undo earlier side effects. The measured real-plane
  qualification is SQLite/in-process, not a Postgres/Redis or multi-replica claim.

The detailed [qualification checklist](internal/ecosystem-integration-checklist.md)
lists the source dependencies and gates before these slices are presented
together. Demand-led integrations and broader runtime features stay deferred.

## Release summary

The checkout-only [Trinity specialist example](../examples/trinity_specialist_mcp/README.md)
uses the v0.9.0 MCP contract. It qualifies a pinned Trinity
configuration shape and a real Runkite API/runner protocol path separately.
Native Trinity runtime execution is not yet qualified. Its fixed trusted
caller in published v0.9.0 uses a dedicated tenant read/write key. v0.10.0
source adds separate agent run/observe grants and durable SQL reference
preconditions. Observation is agent-wide, not requester-private, and the first
reference acquisition cannot prove absence of prior deletion/recreation. Neither
profile sandboxes the surrounding agent. Only the invoked specialist
creates a Runkite run. See the
[engineering boundary](internal/trinity-specialist-boundary.md).

What to expect on the **Supported** profile (`POSTGRES_DSN` + `REDIS_URL` + auth + runner tokens + TLS). Everything else is Compatible / Experimental — see [Architecture](architecture.md) backend tiers.

| Area | Limitation |
|------|------------|
| **Scale-out** | Agent Protocol MCP sessions (`/mcp`) are **process-local** (sticky routing). Admin UI sessions and connector MCP capability tokens are shared when `REDIS_URL` is set; without Redis they stay process-local. Runner tokens are per-kind (optional comma allowlist for fleets/rotation), not unique per pod. SSE event streaming uses one goroutine + one Redis connection per concurrent run; per-CP ceiling is ~3,000–5,000 concurrent SSE streams before Redis connection pool pressure (mitigate by adding CP replicas). |
| **Authz** | Route-level `read` / `write` / `admin`, plus optional `agents:<id>:run`. v0.10.0 admits scoped MCP lifecycle/discovery/invocation and adds explicit agent-wide `agents:<id>:observe` on exact public GET/wait with SQL reference preconditions. Not per-tool ACLs (connector tools use policy grants and argument predicates on MCP `tools/call`). |
| **Tenancy** | Flat `tenant_id`; no tenant registry UI. v0.6.0 named-tenant LangGraph checkpoint access requires HTTP proxy mode; direct Postgres is limited to guarded default-tenant IDs. With client auth + `RUNNER_TOKEN_*`, `serve` requires `RUNNER_TENANTS_*` per tokenized kind (use `default` for single-tenant). |
| **Checkpoints** | v0.6.0 direct Postgres LangGraph accepts only default-tenant thread IDs without `:`; named tenants require HTTP proxy mode plus explicit resolution of prior native history. Durable LangGraph checkpoints: **Postgres direct mode** (AsyncPostgresSaver) or **HTTP proxy mode** (opaque blobs via `/internal/checkpoints`, any CP backend). Without POSTGRES_DSN and without RUNKITE_HTTP_URL the runner falls back to in-memory; blank/whitespace `RUNKITE_HTTP_URL` counts as unset (MemorySaver), not a broken proxy attempt. `checkpoint_ref` time-travel is LangGraph-only; CP does not pre-validate that the id exists. Proxy HITL + mid-superstep pending writes (`aput_writes` / `putWrites`) are full-fidelity (blob stores checkpoint+writes; CAS merge; matrix `hitl_restart` for Python **and** TypeScript). Late writes after cancel soft-no-op on `403 run_not_inflight` (must not crash Node). Proxy `alist(filter=...)` is unsupported (raises). Proxy writes use per-blob in-process locks plus **If-Match / ETag CAS** so multi-replica GET-modify-PUT retries instead of silent clobber; GET 200 without ETag refuses the update. Envelope is shared, but Python↔TS cannot resume each other's threads (serde payloads differ). Opaque list soft-caps ~1000 (no cursor yet). **Non-LangGraph adapters** (CrewAI / LlamaIndex / AutoGen / LangChain) reuse the same HTTP proxy with a stable `adapter-state` id for **message continuity** across runs (not channel-state HITL / not full crew-memory); without `RUNKITE_HTTP_URL` they stay single-shot. See deep-dive bullet below. |
| **Streaming** | The bridge checks generation before every publish; output and status commit atomically under a durable attempt fence. v0.6.0 run SSE, thread SSE and WebSocket recovery confirms terminal events through that durable fence, ignores superseded terminals and uses retained replay plus durable-state checks if a broker subscription closes early. Queue checks and broker publication remain separate, so raw nonterminal events can still be advisory during reclaim. Already displayed old-attempt text cannot be withdrawn; clients must respect preserved attempt identities instead of merging attempts. `/wait` returns persisted current output. Generation must exactly match the active attempt. Legacy generation `0` matches only an actual generation `0`, so older runners that omit the field must upgrade for current assignments. |
| **Rate limits** | Shared cluster-wide only with Redis backend (auto when `REDIS_URL` is set). |
| **Observability** | OTel/Prometheus emit run + metadata LLM/tool spans — not full prompt/completion capture (use your OTLP backend / Langfuse). AutoGen uses native GenAI span names, not `runkite.llm` / `runkite.tool`. |
| **Governance on Mongo** | Durable `audit_events` / `policy_grants` / `pending_actions` / `kill_switches` / `break_glass_windows` / `mandatory_hitl_rules` / `usage_events` are SQL-only (Postgres / MySQL / SQLite). Mongo skips audit/usage writes; Admin grant/pending/audit/kill/break-glass/mandatory-hitl/usage routes return `501`; webhook `pending` cannot persist. See [Trust & governance](trust-governance.md#governance-durability-sql-backends). |
| **In-process tool decide** | `POST /governance/decide` does not create a thread or a run. Admin Threads and Runs do not list that caller. Kill switches, break-glass, and spend caps are not applied. The caller executes the tool after allow, or after the pending row is approved. |
| **Upstream connector MCP** | Default JSON-POST mode remains available. Opt-in `mcp.transport: streamable_http` negotiates upstream sessions and consumes POST SSE, returning JSON to runners. Sessions are scoped to one operation; no cross-call upstream session state, subscriptions, sampling or elicitation. See [Connectors](connectors.md#negotiated-upstream-mcp-transport). |
| **Kubernetes / Helm** | Chart + kind K0–K3 ops proofs (`make kind-helm-all`) plus a named EKS smoke/reclaim on `eu-north-1` (see [k8s-eks-soak-results.md](k8s-eks-soak-results.md) / [k8s-eks-soak.md](k8s-eks-soak.md)). Multi-CP HA correctness remains Compose soak — not a multi-hour cloud HA soak. Production cluster installs: packaging **proven on EKS smoke**; treat full cloud HA claims cautiously until a longer soak exists. |
| **FinOps** | Daily budget caps + `usage_events` + Admin Spend (`/admin/spend`, labeled **Estimated USD** — tokens × your pricebook, or a gateway-reported cost when present, never a vendor invoice) with alerts, CSV/JSON export, open-hold KPIs, and cheaper-alias routing on SQL. With a non-empty pricebook, a terminal run that has tokens, no gateway `cost_usd`, and a model id missing from the book raises `usage_unpriced` (empty pricebook stays tokens-only with no alert; a present $0-rate row is intentional free tier). A terminal run whose reply the runner recognizes as a model turn but reports zero token/cost data in any shape it knows raises `usage_unmetered` instead — this covers provider/framework `usage_metadata` shapes this codebase has never seen, so an unmetered agent never looks identical to a free one. HITL pauses and mid-run cancels now meter the tokens spent on turns that ran before the pause/cancel (Python and TypeScript), not just runs that reach `success`. Multi-turn LangGraph spend dedup skips prior checkpoint messages by id and by list prefix (so bare concat reducers without message ids still do not re-bill earlier turns). Caps sum completed usage **plus** optional `finops.reservation` USD/token holds (0 = off; wrong estimates can still overshoot; holds do not double-count `max_runs_per_day`; optional `reservation.hold_ttl` expires stale open holds). Routing only uses configured `finops.routing.aliases` (no automatic model shopping). Optional `finops.on_hard_breach: cancel_inflight` cancels pending/running runs in scope when a hard day cap is already breached (admission deny or post-terminal re-eval); without it, only new creates are refused. Mongo: budgets fail open; usage Admin `501`. |
| **Protocol surface** | Agent Protocol client search is offset-only (Admin lists have keyset cursors). TS / non-LangGraph runners report stub JSON Schema for agents. |
| **Fixture replay** | `runkite sim` is not a sandbox. In-graph tools stay ungoverned. Allow cases hit the real MCP server. Pending does not auto-approve. |
| **Payload shrink** | Opt-in connector MCP result cap. Not a token SLA. No Redis = no shrink. Enabled shrink means durable audit keeps the preview plus ref, not the original body. |
| **License** | Current source and v0.6.1+ artifacts use [Apache-2.0](../LICENSE), including commercial use and redistribution under its terms. Published v0.6.0 artifacts retain their shipped [BUSL-1.1 license](https://github.com/getrunkite/runkite/blob/v0.6.0/LICENSE). This does not rewrite older releases or create a hosted Runkite offering. |

**Not limitations of the product claim:** multi-replica CP behind a load balancer, framework adapters (LangGraph / LangChain / CrewAI / LlamaIndex / AutoGen / LangGraph.js), and the framework×backend matrix are implemented and soak-tested on Supported.

---

<a id="managed-connector-action-evidence-unreleased"></a>

## Managed connector action evidence (v0.7.0)

v0.7.0 can opt into SQL managed MCP attempt history with
`policy.action_evidence` and required audit. It does not cover local tools,
standalone decisions, model calls or every MCP method. Every serving replica
must enable it; additive migrations do not make older or flag-off writers record.
Managed approval capabilities bind to the original run incarnation; historical
unbound managed approvals need fresh review. Standalone decision approvals
remain app-owned and separate. Intent is permission to attempt, not proof of delivery. A response, error or
cancel is not proof that a target committed or rolled back its business effect.
Unknown outcomes are preserved and never trigger automatic tool replay.

New action tables contain metadata only; existing run streams, checkpoints and
policy audit have their own data/retention rules. Managed approvals bind to a
configured target revision and configured policy revision, including when capture
is off. Optional `mcp.pin_tools` also binds the advertised tool schema; none of
these digests pins upstream implementation. Optional
[registered database credential leases](internal/credential-lifecycle.md) and a
[durable action-event outbox](internal/action-delivery-outbox.md) are available
in current source. They are not enabled merely by turning on action capture.
Tamper-evident external archives remain an operator responsibility. Run pruning
does not remove action history. Resolved-record retention
is separate and leaves pending/unknown attempts, including earlier pending
requests whose approval a later attempt consumed. Storage can therefore grow.
See the [operator guide](../site/support/connector-actions.html).

<a id="tenant-review-boundaries-unreleased"></a>

## Tenant Review boundaries (v0.7.0)

v0.7.0 adds flat-tenant approval/evidence permissions in a
separate Review workspace. This is not general per-resource RBAC or a user
registry. Review needs verified primary identity, explicit tenant and a new role;
empty/default-open permissions do not qualify. Global Admin remains global.
No conversation or general Audit API is exposed to reviewers.

Review cookies contain login-time authority for at most 15 minutes absolute;
activity cannot extend it. IdP changes are not instant revocation of an issued
cookie. Admin/Review sessions and logout are independent. Reviewer approval
retains exact argument/run/generation/incarnation/expiry rules and adds configured
target, configured policy and optional advertised-schema matching for managed
connector approvals. Default-one approval permits self-review unless the operator
enables [requester exclusion and/or reviewer quorum](internal/approval-authority-and-quorum.md).
Those options count exact authenticated identities, not independently verified
people. They do not pin upstream code. Upgrade all replicas before issuing roles.
See [setup and limitations](../site/support/tenant-review.html).

<a id="configured-target-binding-unreleased"></a>

## Configured target binding (v0.7.0)

Target revisions describe configured MCP endpoint/transport, nonsecret auth
locators, scopes, tool filters and an optional operator `mcp.revision` label.
Credential values are excluded; reference changes and credential value rotation
are different operations. An unchanged URL and label cannot reveal that a remote
server changed its schema or behavior. Discovery alone does not prove which code
executes, and the manual label is not an attestation or remote rollback control.

Managed target binding is enforced with or without action capture. Standalone
`/governance/decide` does not gain a managed run or target. New SQL migrations
Postgres/SQLite 18 and MySQL 19 leave historical managed target bindings empty;
those approvals need fresh review. Stop all writers and replace every replica
before traffic. Current source rejects a newer schema, but old released binaries
cannot gain that guard retroactively. See the
[upgrade procedure](ops-runbook.md#connector-target-revision-upgrade).

## Authority and secret-safety boundaries

- Required policy audit is opt-in, SQL-only and available in v0.7.0; it is absent
  from published v0.6.1 binaries. It refuses allow/pending during persistence failures, including cache hits and exceptional authorization paths. It is not a durable downstream action-outcome ledger.
- A2A requires the active parent run and exact generation; supplied authority can only narrow the trusted parent. Kind-token mode alone does not distinguish workers sharing a runner kind. v0.8.0 introduced opt-in verified workload ownership and constrained token exchange, with the boundaries below. Cross-store queue/child authorization remains non-atomic.
- Secret-field redaction covers named aliases rather than arbitrary secret discovery. Connector transport and OAuth acquisition errors omit raw endpoint/body text; tool result content remains the upstream service's responsibility.
- Vault references still read KV values. That original hardening slice did not add lease issuance. The v0.7.0 registered database bindings now coordinate issuance, renewal and provider-confirmed cleanup; see [credential lifecycle](internal/credential-lifecycle.md).

<a id="public-mcp-retry-retention-unreleased-planned-v090"></a>

## Public MCP retry retention (v0.9.0)

Optional MCP `run_id` support reuses the existing REST retry identity and
durable run observer. It is not available in published v0.8.0. Without a stable
caller ID, repeated MCP calls remain separate runs; an existing thread ID alone
does not deduplicate. Matching retries do not rerun admission budgets or enqueue
another assignment, but still require current caller authority. Different
agent/input or an explicitly different thread conflicts.

Retry protection lasts only while the original run remains stored. MCP calls
still wait synchronously, and delivery cancellation does not prove worker
cancellation or rollback. This does not atomically join run insertion and queue
enqueue or provide exactly-once external effects. It also does not make public
MCP interchangeable with a run-bound connector proxy. See the
[MCP contract](mcp-server.md#stable-operation-ids-unreleased-planned-v090).

**Enqueue acknowledgement errors (v0.10.0).** Queue acceptance can precede a
delivery error. Conditional rollback now preserves a committed result or an
admitted worker and fences deletion/recreation of its original row. Only a
pending, never-started original run may become `enqueue failed`; store failure
retains its claim. The caller still receives its error, so reconcile the
original run before starting another operation. Cron retains the fire claim
when execution or persistence is uncertain; it retries only a proven
precreation failure or positively applied never-started rollback. This can
leave an occurrence for operator reconciliation rather than automatic retry.
This is not an atomic database/queue publish or external-effect idempotency. See
[recovery guidance](ops-runbook.md#enqueue-acknowledgement-failures-unreleased).

## Deep dive

- **Managed AI SDK is a separate runner profile.** The new `typescript-ai-sdk`
  backend manages real Threads/Runs, native text/local JSON-result tools and
  successful-turn history. It requires the plane's opaque HTTP checkpoint API.
  Native approval/resume, streaming tool outputs, binary/client/provider tools,
  time travel, whole-run caching and unfinished-turn restoration are unsupported.
  AI SDK UI clients need the application bridge in
  [the example](../examples/managed_ai_sdk/README.md). Raw SSE is advisory; the
  bridge confirms durable completion and refuses to combine attempts after a
  reclaim. Interrupted turns retain only previously committed history.
- **Managed Microsoft Agent Framework (v0.6.1) is a separate Python runner
  kind** (`python-agent-framework`). It manages real Threads/Runs with public
  Agent streaming/session APIs, opaque CAS receipts and local tools. Workflows,
  .NET, non-text inputs, caller-supplied assistant/tool history, native
  approval/resume and time travel are out of the initial scope. Unsupported
  native approval, background continuation, replacement text updates or resume
  fails explicitly. Only successful turns commit
  history; interrupted and failed turns retain the previous committed session
  and report observed usage. CAS metadata, framework, graph and native state
  are validated before execution. Receipt recovery avoids repeated work after
  a successful commit; external effects before that commit still need their
  own idempotency. Source-built `runkite-runner` wheels and sdists now include
  the adapter and dedicated `runkite-runner-agent-framework` CLI. The optional
  `agent-framework` extra pins `agent-framework-core==1.23.0`. This install path
  is included in the published PyPI `0.6.1` artifact; see the
  [source install](../examples/managed_agent_framework/README.md).
  The ordinary Python CLI and stock image keep their LangGraph
  defaults; the image does not install the Microsoft SDK. The four older
  CrewAI, LlamaIndex, AutoGen and plain LangChain adapters remain checkout-only.
  Decision-only `runkite_governance.agent_framework` middleware remains a
  different boundary (app-owned loop, no managed run).
- **Managed OpenAI Agents SDK (v0.6.1)** uses `python-openai-agents`, public
  native streaming/history and successful-turn opaque CAS receipts. Its optional
  `openai-agents` extra pins `openai-agents==0.24.0`; source-built wheels include
  the dedicated `runkite-runner-openai-agents` entrypoint. Published PyPI `0.6.1`
  includes this profile. The stock Python image does not install its optional SDK. The first
  profile supports one text agent and local function tools. Native approval or
  resume, handoffs, agents-as-tools, hosted/MCP tools, structured final output,
  non-text input and unfinished-turn restoration are unsupported. Failed or
  interrupted turns retain previous committed history. Receipt replay protects
  work after commit; external effects before commit still need idempotency.
  Native reasoning items remain in durable history and are not UI output.
  Native SDK trace export is disabled; generic-worker OTLP run tracing remains.
  See the [example](../examples/managed_openai_agents/README.md).
- **Managed Pydantic AI (v0.6.1)** uses `python-pydantic-ai`, native text and
  validated JSON final results. The optional `pydantic-ai` extra pins
  `pydantic-ai-slim==2.63.0`; provider packages are application choices. Published
  PyPI `0.6.1` includes this profile. The stock image does not install its SDK.
  Successful native conversations and receipts use opaque CAS state. Per-run
  usage excludes earlier conversation turns. Partial JSON cannot complete a run.
  Native deferred approvals, output functions, multimodal input and additional
  workflow engines are outside the initial scope. Local tools acquire connector
  policy only through the existing connector helpers. See the
  [example](../examples/managed_pydantic_ai/README.md).
- **Managed Google ADK (v0.6.1)** uses `python-google-adk` with one ordinary
  text `LlmAgent`, local function tools and session-local state. The source extra
  pins `google-adk==2.13.0`; published PyPI `0.6.1` includes the profile and the stock
  image does not install its SDK. A run-local public session service uses existing
  opaque CAS state and successful receipts. Cross-session `app:`/`user:` state
  is rejected rather than made thread-local; `temp:` state is not durable.
  Native workflows/delegation, confirmation/resume, memory, artifacts, structured
  output, long-running tools and live audio are unsupported. A final-response
  event alone cannot mark a run successful. See the
  [example](../examples/managed_google_adk/README.md).
- **Managed UI reconnect is a same-attempt example update (v0.6.1).** The
  app may detach browser delivery and attach its stored thread/run again without
  another model invocation. It must retain that mapping and its own UI messages;
  this is not a durable UI-message service. Native full replay requires a start
  and contiguous runner events. Missing/expired history fails explicitly, and
  replay does not combine attempts across reclaim. Before display, persist the
  first assistant ID through `onMessageStart`; on retries/reattach supply that
  ID as `expectedMessageId` to refuse a replacement attempt across connections.
  An observer without that precondition must not merge unknown partial UI state.
  The persistence callback must accept a repeated same ID and reject a different
  existing ID. Redis completed streams have
  a 24-hour retention window; in-process replay is lost on control-plane restart.
  Detach can leave model/tool execution running. Explicit Stop needs the app's
  authenticated cancel route; aborting a local reader is not run cancellation.
  The existing create default remains cancel. See the
  [managed example](../examples/managed_ai_sdk/README.md#reattach-to-an-existing-run).
- **Shared Python UI profiles are v0.6.1 example code.** Microsoft, OpenAI
  Agents, Pydantic AI and Google ADK can use the existing app-side AI SDK UI bridge
  for text and Pydantic final JSON. Attach expects a bridge-created single-user
  string turn. Native tool UI, reasoning display and multimodal parts remain
  unsupported for these profiles. Current-turn input, durable framework and
  generation must match; a historical assistant is never used as a new answer.
  Retained lifecycle/start history, one-attempt replay and the example's 1 MiB
  frame/response limits apply. Results have a 64-level nesting cap and reject
  integral numbers outside JavaScript's safe integer range. The app keeps
  authentication and UI-message storage. See the
  [route recipes](../examples/managed_ai_sdk/README.md#use-the-same-frontend-with-a-python-runner).
- **Opaque adapter persistence now fails closed.** An unavailable state read
  prevents execution. Failed state writes withhold completion and leave the
  assignment recoverable. Malformed or unsupported message checkpoints fail
  before execution. These semantics replace the previous warning-and-continue
  fallback for non-LangGraph adapters. A crash before state commit can still
  repeat external effects. Downstream idempotency remains required.

Honest gaps, not hidden ones:

- **Cache hits require atomic completion (v0.6.0).** Built-in stores atomically
  insert the admitted run, output, merged state and checkpoint. Custom stores
  without optional `state.CachedRunCreator` use normal dispatch on a cache lookup.
  Cache replay does not bill original output usage or extend cache TTL. It still
  creates a logical run for daily quotas. Whole-run caching stays opt-in and
  unsuitable for agents with external side effects; managed AI SDK rejects it.
- **Exact identity transport requires plane-first upgrades (v0.6.0).** Unicode
  or legacy header-unsafe run/tenant identities use new base64url UTF-8 headers;
  older planes do not understand them. New run IDs with controls or outer
  whitespace are rejected. Existing IDs are not renamed.
- **Parent ownership is checked at mutation (v0.6.0).** A deleted thread ID
  recreated by another tenant cannot accept the old tenant's delayed run or
  checkpoint. Mongo parent and tenant-scoped children delete atomically. This
  prevents new malformed relationships and does not reconstruct historical ones.

- **Application JSON number preservation (v0.6.0)** covers run results,
  thread values/history, checkpoint values/tasks/interrupts, store values and
  cached output.
  These paths keep JSON numbers without converting them through a Go float.
  Numeric value is preserved; database formatting can normalize decimal or
  exponent spelling. PostgreSQL's native numeric range still applies.
  MySQL validates its actual JSON conversion and rejects application writes that
  would change a number. Mongo uses exact native int64 or Decimal128 and rejects
  unsupported precision/range; existing BSON doubles retain only their existing
  represented value. A rejected completion withholds acknowledgement and remains
  recoverable, so operators must correct an unsupported producer or use a backend
  that can represent its values. Numeric preservation itself needs no schema
  migration and cannot repair already-rounded rows. Predicate values now also use exact numeric
  binding; unrelated control metadata and server configuration remain unchanged.
  Browser JSON parsing, including Admin display, still has JavaScript's numeric
  limits; the shared chat bridge keeps its unsafe-integer rejection. Use a
  lossless API client or strings for exact large identifiers in browser apps.
- **Crash reclaim has a generation ceiling (`reclaim.max_retries`, default 3).** At the ceiling, the reaper stops re-enqueuing and retains the current assignment and generation until durable completion and finalization succeed, then acknowledges that exact generation. It first recovers an eligible delivered terminal event; otherwise it attempts an `error` completion. Database, broker, or finalization failures leave the assignment available for the next reaper tick. A same-attempt late report and the fallback error compete through the existing terminal transaction: the first durable terminal wins, so a terminal arriving after the recovery check has no guaranteed precedence. NATS sends `+WPI` while completion is retried; native redelivery still passes the normal attempt fence and recovery before dispatch. Backend support tiers and Kafka's documented coordination limits remain unchanged. Set `"reclaim": {"max_retries": 0}` or `RUNKITE_RECLAIM_MAX_RETRIES=0` for unlimited reclaim. Applies on Redis, NATS, Kafka, and in-process transports.
- **Crash reclaim staleness (`reclaim.max_age`, default 6s) is tunable but coupled to runner heartbeat cadence (~2s).** An in-flight job with no successful Heartbeat/Renew within `max_age` is reclaimed. Lower values risk false reclaims (duplicate LLM spend); higher values slow recovery after a real crash. Override via `"reclaim": {"max_age": "6s"}` or `RUNKITE_RECLAIM_MAX_AGE`.
- **Rate limits are per-process unless `backend` is redis (or auto-selected via `REDIS_URL`).** With the memory backend, N replicas each enforce their own copy of the configured ceiling (up to N× the intended limit under load balancing). Set `"backend": "redis"` (or omit `backend` while running with `REDIS_URL`) for a shared cluster-wide ceiling -- see the `rate_limit` section above.
- **`admission_limits` use locked COUNT+INSERT (not Redis).** Multi-replica deployments share the state store; creates for the same tenant/agent scope serialize on a DB lock so burst traffic cannot all clear a stale COUNT. Daily windows are UTC only. `a2a.max_breadth` still uses a separate check-then-create path and can overshoot under concurrent fan-out.
- **Admin UI uses httpOnly cookie sessions + CSRF.** Login (`POST /admin-api/session`) exchanges an API key/JWT for an httpOnly `runkite_admin_session` cookie; the credential is not kept in JavaScript. Mutating `/admin-api/*` calls require `X-CSRF-Token` (from the login/status JSON). Machine clients still use `Authorization: Bearer` (no CSRF). Session lifetime is a 12h sliding TTL. With `REDIS_URL`, sessions live in Redis (`rk:admin:sess:*`) and any replica can authenticate or logout the browser; without Redis they are process-local and multi-replica Admin UI needs sticky routing to the minting replica. MCP session ownership stays process-local either way (sticky `/mcp`).
- **Authorization is route-level plus optional agent run grants.** Permissions are `read`/`write`/`admin` at the method layer. `agents:<id>:run` unlocks run-create POST routes and, in v0.10.0, scoped MCP lifecycle/discovery/invocation (not cancel/delete/store). v0.10.0 `agents:<id>:observe` separately grants agent-wide exact public GET/wait with durable SQL reference preconditions; `CanRunAgent` then scopes the agent_id (see Auth and [Trust & governance](trust-governance.md)). Optional per-graph `allowed_tools` is enforced by LangGraph runners on observed stream `tool_calls` (not a full per-tool ACL engine / AuthorizeTool RPC). Connector tools use policy grants and optional argument predicates on connector MCP `tools/call` (pre-call only; not in-graph AuthorizeTool).
- **MySQL identity isolation requires schema version 17 (v0.6.0).** Older
  default collations can equate distinct tenant/resource identifiers. Version 17
  uses MySQL 8 `utf8mb4_0900_bin` NO PAD on identity/security columns and leaves
  human search/display semantics intact. All writers must stop for this ALTER
  maintenance window. Preflight rejects legacy aliased references or cross-tenant
  child ownership; investigate and repair before retrying. DDL is not one atomic
  transaction, but interrupted upgrades resume and stamp only after foreign-key
  restoration. Downgrade refuses unsafe merging, and overwritten identities
  cannot be reconstructed. See [upgrade procedure](ops-runbook.md#hardening-upgrade).
- **State-store and pgvector schemas use numbered up/down migrations; Qdrant/Weaviate/Pinecone stay create-if-missing.** `runkite db upgrade` / `serve` apply state versions via `schema_migrations`. pgvector uses a separate `vector_schema_migrations` table (same Postgres DB must not share version rows with state) via `Init` / `runkite vector upgrade`. `db downgrade` / `vector downgrade` each request one rollback step. Security migrations can refuse an unsafe downgrade; target-bound approval migration Postgres/SQLite 18 or MySQL 19 requires a reviewed backup for rollback. Baseline (v1) Down is destructive (CLI warns). Pre-versioned databases run baseline Up then stamp v1. Cloud vector backends have no DDL trail.
- **Runner OTel creates a per-job `runkite.run` child span under the control plane's `traceparent` when `OTEL_EXPORTER_OTLP_*` is set** (same env contract as the control plane; no-op otherwise). Python LangGraph / plain-LangChain, TypeScript LangGraph.js, CrewAI (event bus), and LlamaIndex (instrumentation handler) auto-attach thin listeners that open `runkite.llm` / `runkite.tool` children under that span (metadata only -- no prompts/completions). AutoGen nests its own GenAI `invoke_agent` / `execute_tool` spans under `runkite.run` when OTel is on, but does not emit `runkite.llm` / `runkite.tool` (no LLM callback surface without wrapping `ChatCompletionClient`).
- **Direct Postgres LangGraph is default-tenant-only (v0.6.0).** Every non-default tenant fails before factory/state access, since even a delimiter-safe new named key can read legacy default-tenant history. Use HTTP proxy mode for named LangGraph tenants and explicitly resolve prior native state. Default direct thread IDs without `:` retain history. Fresh memory permits named tenants under its delimiter guard: no colon in named tenant IDs or default thread IDs, and no outer whitespace/ASCII controls in tenant IDs. Default cron threads need proxy mode. Proxy supports all tenants and imports no native history automatically; there is no ownership marker or blank fallback. Managed opaque SDK profiles are unaffected. See [upgrade guidance](ops-runbook.md#follow-up-identity-and-completion-upgrade).
- **Proxy-mode writes use GET-modify-PUT with CAS.** An in-process `asyncio.Lock` per blob serializes parallel LangGraph tasks in one runner. Across replicas, PUTs send `If-Match` (ETag/version) and retry on `412` so concurrent `aput` / `aput_writes` merge instead of silently dropping channels. HITL interrupt→resume and mid-superstep pending writes are full-fidelity (same as direct mode for LangGraph); `aput_writes` without `checkpoint_id` raises rather than no-op. LangGraph Pregel calls `aput_writes` for a *new* checkpoint id *before* `aput`, so a missing blob is normal: both `aput_writes` and `aput` create via `If-None-Match: *` (create-only); if a concurrent peer already won, `412` retries as GET+merge instead of clobbering (so an unconditional `aput` cannot wipe an `aput_writes` shell). Weak/malformed `If-Match` is rejected with `400` (not treated as absent). Checkpoint id `latest` is reserved (`400`; conflicts with `GET .../latest`). `alist(filter=...)` raises `NotImplementedError` (metadata filter is not applied to opaque blobs). `aget_tuple` without an id uses `GET .../latest?ns=` (one round-trip). Opaque list endpoints soft-cap at ~1000 rows (no cursor yet) — deep history / many subgraph ns keys can truncate; `/latest` covers the resume hot path.
- **Store/vector/connector session+MCP are run-bound** (tenant derived from the in-flight assignment). Other `/internal/*` routes still honor `X-Runkite-Tenant-Id` after kind-token auth — set `RUNNER_TENANTS_<kind>` in multi-tenant deployments (see docs/auth.md and docs/trust-governance.md).
- **Connector policy durability is SQL-only; Mongo remains a gap.** In-process Decide (static grants + argument predicates + sync webhook) runs on every state backend. Durable `audit_events`, Admin `/admin-api/policy-grants` overlays, `/admin-api/mandatory-hitl`, `/admin-api/pending-actions` HITL, `/admin-api/kill-switches`, `/admin-api/break-glass`, and `/admin-api/usage/summary` require Postgres, MySQL, or SQLite; Mongo returns `501` for those Admin routes, skips audit/usage writes, and fails closed on webhook `pending` (deny, no `action_id`). Do not market equal governance proofs on Mongo — see [Trust & governance](trust-governance.md#governance-durability-sql-backends).
- **Connector HITL approval is an exact, one-shot argument binding (v0.6.0 hardening).** Versioned digests retain numeric value and JSON type. A matching approval can bypass another pending result, but a current explicit deny still wins. Old lossy hashes and empty-digest wildcards cannot authorize new requests; legacy pending/approved app actions must be resubmitted for fresh review. Display maps remain capped/redacted and subject to backend/browser precision limits. No automatic execution or native framework resume is added. See [upgrade guidance](ops-runbook.md#hardening-upgrade).
- **FinOps is spend control + metering, not a full BI warehouse.** Optional `finops` budgets (hard/soft UTC-day USD/tokens/runs), terminal `usage_events`, Admin Spend (`/admin/spend`) with alerts (`budget_soft` / `budget_exceeded` / `budget_alert` / `budget_kill` / `budget_route` / `usage_unpriced` / `usage_unmetered`), CSV/JSON export, and cheaper-alias routing ship on SQL. `usage_unmetered` fires when a reply comes back but every extraction path found zero tokens/cost in any recognized shape (a brand-new/unrecognized provider integration). the LlamaIndex adapter reads only real provider-reported usage for this (`_RealUsageOnlyHandler`), deliberately skipping LlamaIndex's own `TokenCountingHandler` fallback that would otherwise silently re-tokenize the prompt/response with tiktoken (not the actual provider's tokenizer) and report a plausible-but-fabricated number instead of raising this alert. Optional `finops.reservation` holds count toward **USD/token** day caps until terminal release (not toward `max_runs_per_day`. run creates are already counted by `CountRunsSince`); with reservation unset (0), concurrent creates can still overshoot USD/tokens; with reservation on, set `hold_ttl` or a crashed runner can leave holds that inflate caps until UTC midnight rolls. A `hold_ttl` shorter than a still-running job drops that run's hold mid-flight and under-reserves until terminal release. size TTL above your longest expected run. Reservation estimates can be wrong; use `reservation.agents` for per-agent sizes when costs differ. Routing only rewrites configured aliases. Approach `budget_alert` is once-per-day-per-process (best-effort). Mid-run cancel is opt-in (`on_hard_breach: cancel_inflight`); no continuous provider billing stream (isolated workloads may acknowledge partial snapshots, while ledger settlement remains terminal), no multi-currency. Mongo: budgets fail-open; usage Admin `501`.
- **USD on Admin Spend is an estimate, never a vendor invoice.** Native adapters extract token counts from provider/framework response metadata. Current isolated reports are explicit workload claims. Neither verifies an invoice or makes a compromised runner trustworthy. Dollars come from your `finops.pricebook` or a gateway-reported inline cost; reported isolated cost is also a claim, and explicit reported zero remains distinct from missing cost. No path calls a provider billing API. With a nonempty pricebook, a model missing from the book keeps its token counts but raises `usage_unpriced`; an empty book is tokens-only, and a configured zero rate is intentional. Gateway cost headers and asynchronous dashboards are not imported unless that integration also reports cost through a supported response shape.
- **No central tenant registry, and no user/API-key management UI.** See Multi-tenancy and Admin UI -- a tenant exists implicitly the moment a resource is tagged with it, and there's no persisted user/API-key table to manage in the first place (`api_key` entries are static config).
- **`on_tool_call` requires runner cooperation.** The control plane fires it whenever it sees a RunEvent with `method: "tool_call"`, but doesn't parse LangGraph/LangChain message shapes itself (staying framework-agnostic) -- it's up to a framework-aware runner to emit that method. Both LangGraph runners do: Python's `worker.py` (`find_new_tool_calls`) and the TypeScript runner's `executeRun.ts` (`findNewToolCalls`, a direct port -- same recursive scan for `AIMessage.tool_calls`, same dedup-by-`id` semantics, same "checked before interrupt/cancel handling in the stream loop" ordering) both scan every stream chunk and dedupe by tool-call `id` so a message seen in both `values` and `updates` mode (if both are requested) only fires once. Live-verified end to end for Python: `examples/react_agent` (a real `StateGraph` + `ToolNode`, not a mock) run through the real control plane + real Python runner, with a webhook configured for `tool_call`, actually delivers `{"name":"search","args":{...},"id":"call_001"}` to an external receiver; the TypeScript port is unit-tested against the same message shapes (`executeRun.test.ts`) but not yet re-verified against a live tool-using TS example graph the same way. `generic_worker.py` still doesn't emit it -- `run_start`/`run_complete`/`error`/`interrupt` are fully wired for every runner and fire from real control-plane-observed lifecycle transitions regardless.
- **Fixture replay (`runkite sim`) is not a sandbox.** The header skips FinOps holds, usage ingest, daily run caps, and LLM cache. Concurrent admission, rate limits, kill, and connector policy still run. In-graph tools that never go through connector MCP stay ungoverned. Allow cases hit the real MCP server. A pending expect does not auto-approve HITL. See [Fixture replay](sim.md).
- **Payload shrink is not a token SLA.** Opt-in connector MCP `tools/call` result cap. Token spend may drop only if the runner reports fewer tokens; shrink does not write `usage_events` and Admin Spend does not move on shrink alone.
- **No Redis means no shrink.** `payload_shrink.enabled` without `REDIS_URL` forwards the original body. A Redis error after a successful downstream call also forwards the original; the tool call is not failed.
- **Enabled shrink means durable audit does not keep the original MCP body.** The hop's trail is the preview plus ref. Retrieve is for the agent during Redis TTL, not forensics. Grants / pending / HITL / kill stay fully audited. Leave shrink off if the audit path must retain oversized results. See [Connectors](connectors.md#payload-shrink-opt-in).
- **In-graph `allowed_tools` requires runner cooperation and is not a sandbox.** Python/TS LangGraph runners deny non-listed names observed in stream chunks before ToolNode runs. Managed AI SDK, Microsoft, OpenAI Agents, Pydantic AI and Google ADK profiles check local tool names before their bodies run, including an explicit empty deny-all list. The v0.6.0 Microsoft check uses native function middleware; a configured restriction fails before model execution when a custom client lacks that invocation boundary. Older generic adapters do not enforce this allowlist. Tools that never pass a cooperating runner hook are not filtered. There is no general `AuthorizeTool` round-trip; connector policy remains on the proxy path. See [Trust & governance](trust-governance.md).
- **In-runner custom routes are ASGI-only.** `uvicorn` (the SDK's only custom-routes dependency) doesn't serve WSGI apps -- Flask needs an adapter (e.g. `a2wsgi.WSGIMiddleware`) wrapped around it first. Sidecar mode has no such restriction (any language, any framework).
- **Automatic real agent schema introspection is limited to static Python LangGraph.** The runner extracts real input/output/state/config definitions from its loaded static graphs, with independent field fallbacks. v0.10.0 caches these reports and adds bootstrap-only recovery after registration resets or failed publication. It does not introspect factories, TypeScript or other Python adapters. Those profiles retain their existing bootstrap defaults unless a schema is separately supplied. Visual graph structure is not a data schema, and a published definition does not attest the active worker's code. See the [Agents API](api.md#agents) and [recovery limits](internal/schema-publication-recovery.md).
- **`checkpoint_ref` time-travel is LangGraph-only.** A non-empty `checkpoint_ref` on create-run is forwarded on `RunAssignment` and mapped by the Python/TypeScript LangGraph runners to `configurable.checkpoint_id` (LangGraph's native resume-from-past-checkpoint key). Non-LangGraph runners (`generic_worker` adapters) reject it with an error event rather than silently ignoring it. The control plane does not verify that the checkpoint exists for the thread before dispatch -- an unknown id fails at the checkpointer during execution. Empty/whitespace values are treated as absent (resume from the thread's latest checkpoint).
- **Adapter multi-turn uses the same opaque HTTP proxy, not `checkpoint_ref`.** CrewAI / LlamaIndex / AutoGen / LangChain adapters set `checkpoint_framework` and, when `RUNKITE_HTTP_URL` (or the worker http address) is set, `generic_worker` loads `GET /internal/checkpoints/{thread}/adapter-state` before execute and `PUT`s that same id after success/interrupted (not `.../latest`, which is ordered by `checkpoint_id` and can collide with LangGraph UUID blobs on a shared thread). Clients can send only the new turn; LlamaIndex restores native `chat_history`, while other adapters fold prior turns into the next invoke / AutoGen model context. Without an HTTP base URL, adapters stay single-shot (client must resend history). This is not full CrewAI crew-memory restore and not LangGraph channel-state fidelity.
- **`serve` (not `dev`) fails closed on an insecure default posture.** Starting `serve` without a durable state backend (`POSTGRES_DSN`/`MYSQL_DSN`/`MONGO_URI`), a shared transport (`REDIS_URL`/`NATS_URL`/`KAFKA_URL`), `RUNNER_TOKEN_*`, or client-facing `auth` configured now exits `1` with a clear error instead of silently starting a single-node, fully-open instance that still passes its own `/readyz` -- found live in an external audit. Set `RUNKITE_ALLOW_INSECURE_SERVE=1` to start anyway (a private network, CI, or a deliberate quick demo are legitimate reasons); `runkite dev` is unaffected and always starts with the zero-dependency local posture.
- **CORS `"*"` no longer implies credentialed access.** `allow_origins: ["*"]` still means "any origin may read this API," but `Access-Control-Allow-Credentials` is now only sent for an explicitly-listed origin, never for a wildcard match -- an external audit found the old behavior reflected the request's `Origin` header verbatim *and* set `Allow-Credentials: true` for literally any origin once `"*"` was configured, which is worse than a literal `Access-Control-Allow-Origin: *` (browsers refuse that exact combination with credentials; reflecting the origin instead bypassed that protection). List explicit origins alongside `"*"` if some callers need credentialed access.
- **`action=rollback` on run cancel doesn't delete checkpoints.** See the Runs API reference above -- it deletes the run record, but not any checkpoints, despite the Agent Protocol spec's literal wording. Checkpoints are keyed by `thread_id`, not `run_id` (accumulated across every run on a thread, with no per-run attribution to select just one run's slice), and in direct mode live entirely in LangGraph's own Postgres tables, outside this project's own state store.
- **TypeScript runner's Docker image (`Dockerfile.runner-ts`) is built and live-verified** (a real run against `echo_agent_ts` through `docker-compose.ts.yml` completes and echoes back correctly), but its own VG-001/002/003-equivalent verification is otherwise manual-only, not regression-guarded in CI (same as cron's multi-instance claim test).
- **Vector store supports pgvector, Qdrant, Weaviate, and Pinecone.** Every backend named in the original plan is implemented -- see the Vector Store section.
- **In-process requeue retains failed handoffs (v0.6.0).** A full ready
  buffer or cancelled Nack/reclaim call keeps its original lease and generation
  for retry. Reclaim counts only successful ready writes. This closes local
  lost-work paths; it adds no durability across process exit or multi-replica HA.
- **Dispatch verifies actual assignment kind (v0.6.0).** Token-authenticated
  GetJob checks the dequeued job before advancing its durable attempt or exposing
  input, returning a misroute with a generation-bearing Nack. No-token local mode
  stays trusted. NATS durable names map `.` to `_`; kinds such as `a.b` and `a_b`
  cannot coexist under that name. Conflicting filters fail instead of retargeting
  a durable. Reconcile work before renaming config/tokens/runners; no automatic
  consumer or history migration is provided.
- **NATS run IDs must be literal subject/KV partitions (v0.6.0).** Use
  nonempty ASCII letters/digits, `-`, `_`, `/`, `=` or single internal dots,
  without leading/trailing dots or empty dot segments. Wildcards, whitespace,
  colons and Unicode are rejected before NATS queue/event/cancel operations.
  Run creation returns 400 before thread/run/cache mutation. Valid existing keys remain unchanged. Unsupported legacy metadata may still
  be readable, but stream/cancel operations fail closed and queued legacy work
  is retained with delayed negative acknowledgement for operator investigation.
  Postgres/Redis does not impose this NATS-only boundary. See the
  [operator notes](ops-runbook.md#follow-up-identity-and-completion-upgrade).
- **Transport supports in-memory, Redis, NATS (JetStream), and explicit Kafka Compatible mode.** NATS retains its native delivery generation and documented `EventBroker.Close` no-op. Current source refuses Kafka queue construction unless `RUNKITE_KAFKA_ALLOW_NONATOMIC_FENCES=true`. Kafka uses a materialized view and last-write-wins state appends, so identity comparisons are not cross-replica atomic CAS. Pairing Redis supplies event/cancel delivery and a reclaim-leader lease; it does not close queue mutation races. Kafka without Redis also has process-local event/cancel delivery and remains single-instance for those functions. See the [Kafka contract](architecture.md#kafka-transport-jobqueue-only--compatible-with-redis-experimental-without).
- **Admin UI list endpoints support keyset `?cursor=` (preferred) and `?limit=&offset=`** (default 50, max 200). Full pages return `X-Next-Cursor`; cursor and offset are mutually exclusive. Agent Protocol client search stays offset-only. Overview uses real `COUNT`/`GROUP BY`.
- **Factory Graph's `_ExecutionRuntime` construction depends on `langgraph_sdk`'s internal (underscore-prefixed) class.** `langgraph_sdk.runtime.ServerRuntime` has no public constructor -- factory_graph.py constructs `_ExecutionRuntime` directly, which could break on a `langgraph-sdk` upgrade that changes its internals; a `_MinimalRuntime` duck-typed stand-in (covering the same documented `.user`/`.store`/`.execution_runtime`/`.ensure_user()` surface) is used as a fallback if that construction fails, so factory graphs keep working either way for the common case.
- **Factory Graph's `runtime.execution_runtime.context` is always the run's `configurable` dict, not a typed `context_schema` instance.** LangGraph Platform's own beta API supports strongly-typed context via a graph's declared `context_schema`; this passthrough is untyped (a plain dict), sufficient for factories that only read specific keys but not for ones relying on `context_schema`'s validation/defaults.
- **Admin UI write actions**: cancel-run and delete-thread reuse the exact client-facing handlers under system context (`POST /admin-api/runs/{runID}/cancel`, `DELETE /admin-api/threads/{threadID}`); webhook redelivery (`POST /admin-api/webhooks/dead-letters/{id}/redeliver`) is genuinely new, since the client-facing API has no equivalent. Known limitation: `WebhookDeadLetter` doesn't persist the original signing secret, so a redelivery is sent unsigned -- a receiver enforcing `X-Runkite-Signature` will reject it. Redelivery also doesn't remove the entry from the dead-letter list on success (it stays as an audit record, not a mutation of stored state) -- the response's `delivered` field is the UI's only success signal.
- **Retention's `checkpoints_keep_last` only covers proxy-mode checkpoints.** See the Retention section above -- direct-mode Python+Postgres deployments use LangGraph's own `AsyncPostgresSaver` tables, which this feature doesn't touch.
- **SSE event streaming is goroutine-per-run, each holding a Redis connection.** Every `broker.Subscribe(runID)` spawns one goroutine running a blocking `XREAD` loop against the run's Redis Stream (`rk:events:{run_id}`). Go goroutines are cheap (~2-4 KB each), but each one occupies a Redis connection from go-redis's pool for the duration of the SSE stream. At ~3,000-5,000 concurrent SSE connections per CP, the Redis connection pool becomes the bottleneck (Redis default `maxclients` is 10,000, shared across all CPs, queues, heartbeats, and cancels). Mitigation today: horizontal scaling (add CP replicas behind the load balancer). Future optimization path: a multiplexed XREAD reader that tails all active streams in one Redis command/connection and dispatches to per-run Go channels in-memory — same `EventBroker` interface, same handler code, 50x fewer Redis connections. Not implemented because current target scale (hundreds to low thousands of concurrent runs) is well within the goroutine-per-run model.

<a id="security-completion-boundaries-unreleased"></a>

## Security completion boundaries (v0.7.0)

v0.7.0 adds optional reviewer separation/quorum, policy and MCP schema
binding, durable action export, registered Vault database leases and execution
pools. Published v0.6.1 artifacts do not implement these controls.

- Quorum counts exact authenticated identities, not independently verified humans.
  Default-one local approval can still lack an actor; strict quorum or requester
  exclusion cannot. Existing policy must first hold the operation.
- Configured policy hashing cannot attest changes behind an unchanged PDP label.
  MCP schema pinning cannot establish remote code identity or transactional tool
  effects. Cross-system authority checks and sends are not one atomic transaction.
- Evidence export is at least once and may arrive out of order. Receiver-side
  deduplication uses delivery ID. Changing destination holds the former sink's
  queue; dead-letter retry resends evidence only. Delivered rows require explicit
  operator retention/capacity planning.
- Dynamic credentials are limited to registered Vault database roles and trusted
  credential-aware consumers. KV `secret_ref` remains KV lookup. No arbitrary
  secret issuance, user OAuth-token vault, automatic callback renewal, workload
  identity bootstrap or universal MCP credential compatibility is added. A lost
  issuance response may leave an unrecoverable handle and an unknown lease.
  Confirmed revocation does not undo a query or terminate every existing session.
- Pools isolate fleet routing and credentials, not hostile code inside one worker.
  Helm egress manifests need an enforcing CNI. The external OpenShell JSONL bridge
  remains experimental, with explicit immutable image/policy and durable uncertain
  launch handling. Historical v0.7.0 SQLite plane/installed pooled worker/OpenShell
  0.1.8 lifecycle qualification and a separate kind 0.33.0/Kubernetes 1.37.0
  default-kindnet egress qualification have passed. These do not qualify a
  different runtime/kernel/CNI, cloud metadata service, hostile workloads sharing
  one worker, the later combined multi-replica profile or live model/provider. Existing SDK
  adapters do not become sandboxed merely by selecting a pool. See the
  [exact qualification scope](internal/constrained-execution.md#verification-and-remaining-qualification).
- Combined external-runtime HA qualification is not missing: the v0.8.0 wrapper
  already exercises two Postgres/Redis planes, TLS, installed-worker identity,
  reclaim/receipt replay, cancellation, partial usage and bounded failover, with
  a recorded historical hosted pass. v0.10.0 fixture hardening adds explicit
  environment and structured acceptance controls plus a routine-cancellation
  sandbox-absence observation before final cleanup. Current candidate evidence
  must be measured separately. Repetition covers the partial-usage/failover
  scenario, not all lifecycle cases or sustained load. Wrapper cleanup does not
  establish worker cleanup after every crash; uncertain intents still need
  reconciliation. The profile remains experimental, with independent RFE #52
  assessment, production PKI rotation and long-running production soak outside
  these bounded results. See [the precise combined scope](internal/constrained-execution.md#rfe-47-current-source-qualification-hardening-unreleased).
- The v0.10.0 checkout [effective-policy qualifier](internal/openshell-effective-policy.md)
  optionally captures matching current/loaded sandbox policy and compares it
  with the original boundary using the official OpenShell 0.1.8 checker.
  It changes no production worker or authorization gate. Its passing local
  macOS fixture uses in-memory checkpoint/usage clients; inspect the separate
  Linux workflow's actual current result for that environment. Global/provider
  composition and unsupported checker inputs are rejected. Observed snapshot equality does not
  atomically fence exec, prevent change-and-restore between observations or
  establish continuous kernel enforcement. No blocked URL was supplied in the
  recorded run, so it adds no runtime network reachability claim. The profile
  remains experimental and does not acquire universal isolation or independent
  certification from a configuration comparison.
- Published v0.7.0 has no supported isolated usage frame. v0.8.0 adds the bounded
  reporting contract below. Neither version can infer absent
  provider charges from zero recorded spend.

See [support operations](../site/support/security-completion.html) and
[upgrade constraints](ops-runbook.md#security-completion-upgrade).

<a id="identity-and-isolated-usage-boundaries-v080-source"></a>

## Identity and isolated usage boundaries (v0.8.0)

v0.8.0 adds optional strict mTLS ownership that distinguishes workers by verified
URI, public-key hash and
certificate expiry. It requires compatible SQL state, an in-process or Redis
queue and direct verified TLS on both runner surfaces. A fleet token remains
required. This proves possession of the configured credential, not hardware
attestation, trustworthy code, a distinct human or accurate usage. Certificate
replacement needs drain/reclaim; deny-list changes require updating every
replica. See [workload identity](internal/workload-identity.md).

The experimental isolated profile can durably acknowledge versioned cumulative
usage before final completion. Unknown measurements, reported zero and partial
reports remain distinct. The completion transaction uses the latest accepted SQL
snapshot, including after cancellation, reporting-plane death or receipt replay.
A report that loses the race to cancellation is rejected. The plane does not
wait for unacknowledged work, discover unreported provider calls or verify the
workload's claims. Mongo does not support this reporting capability. Native SDK
usage extraction remains a separate adapter capability.

Settlement still uses the existing terminal usage ledger. Durable partial
snapshots are not a provider billing stream or a hard token/USD ceiling against
a dishonest workload. Keep provider quotas and independent accounting. The
bounded combined two-plane/TLS/OpenShell fixture is an operator command, also
invoked by a manually dispatched qualification workflow. It is not an automatic
PR or release gate, and workflow presence is not a hosted-run result. Preserve the separate
historical v0.7.0 runtime/CNI results above; they do not prove this newer combined
deployment. See [the usage contract](internal/isolated-usage.md) and
[combined qualification](internal/constrained-execution.md#combined-ha-tls-and-durable-usage-qualification).
