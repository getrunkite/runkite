# Trust & governance

Runkite is a control plane. Security that fits is **governance of the plane**: who can run which agent, with which connectors/tools/secrets, with kill-switches, break-glass, budgets, and an audit trail — self-hosted. (Admin Spend + FinOps alerts/export/holds/routing ship on SQL; full BI warehouse / multi-currency stay out of scope.)

This page documents the trust boundary and what is (and is not) enforced today. It deliberately under-promises.

**Announce bar (verify, don’t rebuild):** run-binding, connector policy deny + durable audit, Admin audit search, and connector HITL (pending → approve → one-shot retry) are exercised by `make smoke-governance` on Postgres. That smoke is the laptop proof for those exits — not Mongo parity, not in-graph tool sandboxing, not a paid cloud soak, and not a full FinOps BI warehouse (Admin Spend on SQL is separate).

**Admin UI (SQL):** `/admin/grants`, `/admin/mandatory-hitl`, `/admin/pending`, `/admin/kill`, `/admin/break-glass`, `/admin/audit`. See [Admin UI](admin.md#governance-pages-sql-backends).

**In-process tool decide:** `POST /governance/decide` answers one tool call for an app that keeps its own model loop. It does not create a thread or a run, so Admin Threads and Runs stay empty for that app. Grants, mandatory HITL, the pending queue, and audit apply. Kill switches, break-glass, and spend caps do not. The app executes the tool after allow, or after the pending row is approved.

## Trust boundary

```
Clients (SDK / HTTP / Admin UI)
        │  Agent Protocol + client auth
        ▼
┌─────────────────────────────┐
│  Runkite control plane      │  ← enforcement surface
│  auth · tenants · runs      │
│  connectors · MCP proxy     │
│  store/vector proxy         │
└───────────┬─────────────────┘
            │ Runner Protocol (gRPC) + /internal/* HTTP
            ▼
        Runners (framework code, LLM calls, native tools)
```

**What the plane always sees:** client identity (when auth is configured), run create/cancel/stream, job dispatch/fencing, connector session minting, MCP proxy forwards, proxy-mode store/vector calls.

**Data the plane handles:** managed-run input, streamed/final output, thread
values and opaque checkpoints can contain chat text, tool arguments and tool
results. They are not automatically redacted merely because built-in tracing is
metadata-only. Treat the state store, event broker, backups and Admin access as
access to those payloads.

**What the plane does not automatically observe:** every model-provider HTTP
request or response, framework-native tool bodies that bypass connectors, and
direct-mode Postgres/pgvector access from the runner. Runkite does not proxy model
HTTP by default or inspect prompts for safety/PII. Its built-in OTLP spans carry
metadata rather than full prompt/completion capture; `LOG_FORMAT=json` separately
controls stdout log formatting.

**Execution ownership:** an app-owned Decide call has no managed run and an
allow is not evidence of execution. The managed AI SDK runner creates
actual Threads/Runs, but local tools still need real connector gates for policy
enforcement. Cancellation cannot undo completed effects, and trailing usage can
arrive after the plane has settled a cancellation. See
[integration guidance](client-sdk.md#choose-who-owns-execution).

## Run-binding (current)

Proxy paths that touch tenant data or credentials require an **active run**:

| Path | Binding |
|------|---------|
| `POST /internal/connectors/{name}/session` | Required |
| `POST /internal/connectors/{name}/mcp` | Required |
| `/internal/store/*` | Required |
| `/internal/vectors/*` | Required |
| `/internal/checkpoints/*` | Required |
| `POST /internal/a2a/runs` | Required for the active parent |
| Other `/internal/*` (schema, status, connector metadata) | Runner token only |

Runners send `X-Runkite-Run-Id` and `X-Runkite-Generation` (the fencing generation from `GetJob`). The control plane looks up the in-flight `RunAssignment` and derives `tenant_id` / agent (`graph_id`) / principal (`user`) from that record. Runner-supplied `X-Runkite-Tenant-Id` is **not** trusted on bound paths. The supplied generation must exactly match the active assignment, including when either value is `0`. Zero is never a wildcard: a caller sending `0` cannot bind to generation `1` or a reclaimed generation `2`. An actual legacy assignment at generation `0` accepts only `0` until it is reclaimed.

Runners and custom proxy clients must echo the generation received from `GetJob` on every bound HTTP call, Heartbeat, StreamEvents and ReportStatus. Both shipped runners already do this. Older clients that omit the field must be upgraded for positive-generation assignments; the plane does not infer their attempt from the current lease. This closes the old compatibility bypass that let an unversioned stale worker act on a newer attempt.

Stable `reason_code` values on denials include:

| `reason_code` | Meaning |
|---------------|---------|
| `runner_credentials_invalid` | Missing/wrong runner kind token |
| `runner_tenant_denied` | Tenant not on `RUNNER_TENANTS_<KIND>` |
| `run_binding_required` | Missing/invalid run id or generation |
| `run_not_inflight` | No active assignment for that run id |
| `run_generation_mismatch` | Stale/superseded generation |
| `run_binding_lookup_failed` | Queue lookup error |

## Tiers

| Posture | Notes |
|---------|--------|
| Local (`runkite dev`) | Runner auth off; fine for single-user laptops |
| Production baseline | Durable store + shared transport + `RUNNER_TOKEN_*` (optional comma allowlist per kind for fleets/rotation) + client `auth` (admission refuses bare `serve`) |
| Multi-tenant | Client auth + run-binding on proxy paths; `serve` requires `RUNNER_TENANTS_<KIND>` for every tokenized kind so unbound `/internal/*` routes cannot claim arbitrary tenants |

## In-graph `allowed_tools`

Optional per-graph allowlist in `langgraph.json`:

```json
"allowed_tools": {
  "sales-assistant": ["search", "get_record"]
}
```

- **Key absent** for a graph: no runner-side in-graph filter (connector policy still applies on session/MCP paths).
- **Key present** (including `[]`): Python and TypeScript LangGraph runners refuse tool names not listed when they see them in the stream — before ToolNode side effects. Events: `tool_call` → `tool_auth` (`effect: deny`, `reason_code: tool_not_allowed`) → `error` → `end` status error.
- This is **not** sandboxing of arbitrary code, not argument-level policy, and not `AuthorizeTool` RPC. Unwrapped or custom tool paths outside the supported runner invocation hooks are not filtered.
- **Managed Microsoft (v0.6.0):** the same omitted/unrestricted, empty/deny-all and named-list semantics apply at the native local function middleware boundary, before a tool body runs. A configured restriction fails before model execution for custom clients without that supported boundary. A forbidden call emits `tool_auth` (`effect: deny`, `reason_code: tool_not_allowed`) and fails the run while preserving observed usage and previously committed history. This does not turn local tools into connector calls or enable native approval/resume.
- Durable SQL `audit_events` rows for connector Decide remain separate; in-graph denials are visible on the run event stream.

## Explicit non-goals (today)

- Prompt-injection / jailbreak classifiers
- PII scanning or output moderation
- Sandboxing arbitrary tool code inside the runner
- Transparent egress MITM for every BYO `httpx`/`fetch` call
- Replacing your IdP
- Equal security / governance proofs on Mongo or Experimental backends (see [Governance durability](#governance-durability-sql-backends))

## Fixture replay

`runkite sim` drives the same connector policy path with `X-Runkite-Simulation`. FinOps holds and UTC-day run caps skip; concurrent admission, rate limits, kill, and HITL still apply. Not a sandbox. See [Fixture replay](sim.md).

## Execution containment is an external boundary

An operator running shell, browser or generated-code tools on untrusted input
needs controls below an SDK callback. A tool can open a file or make a direct
network request without calling a Runkite connector. Existing runner allowlists
and connector approvals do not confine that process. The Helm chart already
defaults to a non-root identity, disabled privilege escalation and dropped
capabilities in `deploy/helm/runkite/values.yaml`. Its original optional ingress
NetworkPolicy and the newer egress profile are both disabled by default. Its root
filesystem is writable by default.
Those controls are useful but do not establish an arbitrary-code sandbox.

v0.7.0 adds separately credentialed execution pools, an opt-in
Helm egress profile and an experimental external OpenShell 0.1.8 JSONL workload
bridge. The trusted Runkite worker remains outside the workload, owning runner
authentication, MCP authority, checkpoint CAS and launch receipts. Ordinary SDK
adapters continue executing in their trusted worker process; selecting a pool
alone does not hide that process's credential from its agent code.

OpenShell supplies the external filesystem/process/network boundary. Runkite
supplies managed admission and business-tool policy. Either denial remains a
denial. The bridge requires an explicit compatible workload and immutable
image/policy, and refuses automatic relaunch after an uncertain durable launch
intent. A recorded configuration hash does not attest kernel or CNI enforcement.
The initial chart NetworkPolicy is ingress-only; the new optional egress profile
needs explicit allowed destinations and an enforcing CNI. See the implementation,
fixture controls and qualification limits in
[the constrained execution record](internal/constrained-execution.md).
The experimental JSONL profile has no model/token-usage or spend-settlement
contract. Zero recorded spend does not establish free execution, and existing
native SDK usage reporting is not inherited. Use external provider accounting
and limits for unreported workload calls.

## Required audit before access

Availability: v0.7.0. Published v0.6.1 binaries do not
implement this mode.

The optional `policy.audit_required: true` setting requires a durable policy decision before managed admission, connector access or an in-process decision can allow or become pending. It requires auditing enabled and a SQL state backend. Startup rejects an incompatible configuration. The default remains best-effort auditing.

A required-audit outage blocks dispatch, including approved connector retries and break-glass use. Approved capabilities remain unconsumed. Existing explicit denials keep their reason; traces and SIEM separately identify the audit failure. A failed write cannot appear as a successfully saved SQL audit row. Use operational logs, traces and database health to investigate, then retry only a request known to have been blocked before dispatch.

Argument metadata recursively removes common password, token, secret and API-key aliases while preserving the original arguments for policy evaluation and the exact digest. Name-based filtering cannot find credentials embedded in arbitrary text. Avoid secret-bearing tool arguments and sensitive payload capture. A policy decision remains separate from evidence that a downstream operation executed.

<a id="managed-mcp-action-evidence-unreleased"></a>

## Managed MCP action evidence (v0.7.0)

v0.7.0 adds optional `policy.action_evidence` on SQL, requiring
`policy.audit_required`. Each valid managed tool request gets its own identity.
The original policy effect is retained separately from the final approved or
break-glass authority. SQL managed approvals bind to the immutable original run
incarnation even when action capture is off. Historical unbound managed approvals
require fresh submission and review; they are not inferred from a current run
with the same ID. Standalone expiring decision approvals remain separate. SQL commits a generation/incarnation-fenced dispatch intent
with any approval consumption before the protected MCP send. This is an attempt
trail, not a transaction with the external target.

Response observations exclude bodies and distinguish protocol error/tool error
from an unknown outcome. Even an observed response does not establish business
success. A late response remains on the original generation. SQL persistence
retries never replay a tool. Actions survive run pruning and have independent,
opt-in resolved-record retention. Required audit by itself continues to record
decisions only. See the [manual](../site/support/connector-actions.html) and
[design rationale](internal/connector-action-evidence.md). Managed approvals now
bind the [configured target revision](internal/target-bound-approvals.md) and
[configured policy revision](internal/approval-authority-and-quorum.md).
`mcp.pin_tools` optionally binds the observed advertised tool schema. These
digests do not pin upstream implementation or attest a remote PDP's behavior.
Optional [registered Vault database leases](internal/credential-lifecycle.md) and
[durable action-event export](internal/action-delivery-outbox.md) reuse the same
gate and SQL event transactions. Capture alone enables neither option; no second
vault is introduced.

<a id="scoped-human-review-unreleased"></a>

## Scoped human review (v0.7.0)

v0.7.0 adds `/review-api` and `/admin/review/` with primary
`governance:read` / `governance:approve` permissions. Scope comes from a verified
identity and explicit tenant. Global Admin remains global. Reviewers cannot edit
policy/grants, mint break-glass or read conversations. Read-only pending replies
omit arguments; approvers receive the existing redacted display. Free-form
reason text is excluded. Approval still follows current policy and exact binding;
it does not execute the tool.

Separate Review sessions have purpose checks, their own cookie/storage namespace,
CSRF and at most 15 minutes absolute lifetime. There is no anonymous Review mode
or automatic permission from client read/write. Configured target revisions apply
to managed approvals on both Review and Admin. Current source additionally offers
[requester exclusion and distinct-reviewer quorum](internal/approval-authority-and-quorum.md),
configured policy binding and optional advertised-tool-schema binding. The
default remains one approval without requester exclusion. No new user directory,
proof of independent humans, upstream implementation attestation or instant IdP
revocation is claimed. See [Tenant review](../site/support/tenant-review.html).

## Governance durability (SQL backends)

Connector **enforcement** (static grants, sync webhook, fail-closed Decide) runs on every state backend when `policy` is configured. The **durable governance trail** needs SQL:

| Capability | Postgres / MySQL / SQLite | Mongo |
|------------|---------------------------|-------|
| In-process Decide (grants + predicates + webhook) | Yes | Yes |
| Durable `audit_events` + Admin search / Audit UI | Yes | No (writes skipped; Admin `501`) |
| Durable `policy_grants` Admin CRUD overlays | Yes | No (`501`) |
| Durable `pending_actions` HITL approve/deny | Yes | No (pending fails closed to deny; Admin `501`) |
| Durable `kill_switches` (tenant/agent pause + drain) | Yes | No (`501`) |
| Durable `break_glass_windows` (time-bounded policy bypass) | Yes | No (`501`) |
| Durable `mandatory_hitl_rules` Admin overlays | Yes | No (`501`) |
| Durable `usage_events` + Admin usage summary + Spend UI | Yes | No (`501`) |
| Live FinOps overlays (`/admin-api/finops`) | Yes | No (`501`) |
| Run admission Gate (`agents:<id>:run` + kill + optional `run.create` Decide + optional finops budgets) | Yes | Authz works; kill / break-glass / budget durability SQL-only |
| OTel `policy.decide` + optional SIEM `policy_decision` | Yes | Yes (not store-backed) |
| `tool_auth` RunEvents | Yes | Yes |

**Security marketing rule:** claim audit search, Admin grant overlays, connector HITL, kill switches, break-glass, and mandatory HITL for SQL state backends. Mongo is fine for agent runs and connector policy *enforcement* when grants/webhook live in `langgraph.json`, but it is **not** an equal governance proof. Production HA still prefers Postgres + Redis (direct-mode checkpoints; Compose soak); Helm packages that backend profile but cluster install stays Compatible until a cloud soak. MySQL/SQLite share the governance tables.

## Connector policy

When `langgraph.json` has a `policy` section (including empty `"policy": {}`), connector access is **fail-closed**:

| Stage | Gate |
|-------|------|
| `POST .../session` | Grant must match `(tenant_id, agent_id, connector)`; MCP responses include a short-lived `session_token` |
| `POST .../mcp` | `X-Runkite-Connector-Session` must match that token for `(tenant_id, agent_id, run_id, generation, connector)` (15m absolute TTL) |
| `POST .../mcp` `tools/call` | Same grant + optional per-grant tool allow/deny + argument digest / config predicates |

Static grants evaluate in-process. Optional **argument predicates** (`policy.predicates` in `langgraph.json`) run after a matching grant and before the sync webhook: they inspect `tools/call` `params.arguments` and may `deny` or `pending`; they never `allow`. Missing `when.path` does not match (the call proceeds to webhook / allow). Numeric comparisons (`eq` / `neq` / `gt` / `gte` / `lt` / `lte`) preserve exact JSON decimal values, including configured thresholds, without binary-float conversion. This is connector MCP, pre-call only: it does not wrap in-graph tools, does not inspect tool responses, and is **not** `AuthorizeTool`. Config-only this release (restart the control plane; no Admin CRUD). Copy-paste and the operator path: [Grants & HITL](../site/support/grants.html#argument-predicates). An optional sync webhook (HMAC, same shape as `preflight_hooks`) can add an external deny; first deny wins, and a static allow does **not** bypass the webhook. Policy denials use JSON-RPC `-32000` with `data.reason_code` on MCP, or HTTP `403` + `reason_code` on session. **Denials do not trip the connector circuit breaker.**

When grants and a webhook are both configured, grants act as a first-pass allowlist: a request with no matching grant is denied immediately and the webhook is never called. Predicates are not consulted on a grant deny either. `default_effect: allow` is only honored when there is **no** webhook (grants-only setups). With a webhook and no grants, the webhook decides; `default_effect` is the fallback if nothing else returns.

Every decision is written to `audit_events` on SQL backends when `policy.audit` is true (default). Connector `tools/call` rows include `attrs.args_digest` (a versioned SHA-256 of sorted-key JSON with normalized exact numbers) and a size-capped, secret-stripped `attrs.args` for display. `tool_auth` RunEvents carry the digest only. Mongo skips the write; see [Governance durability](#governance-durability-sql-backends).

**Admin search:** `GET /admin-api/audit-events` lists decisions newest-first with cursor paging (`X-Next-Cursor`) and filters (`tenant_id`, `decision`, `action`, `run_id`, `agent_id`, `connector`, `tool`, `since`/`until` as RFC3339). Example — denials for tenant `acme` in the last 7 days:

```
GET /admin-api/audit-events?tenant_id=acme&decision=deny&since=<RFC3339>
```

Returns `501` on Mongo. The Admin UI **Audit** page (`/admin/audit`) lists the same data with tenant/decision/connector filters.

**Observability:** every Decide adds an OTel span event `policy.decide` (effect, reason_code, run/agent/connector/tool) on the active HTTP span when OTLP is configured. Optional async SIEM export:

```json
"policy": {
  "grants": [ ... ],
  "siem": { "url": "https://siem.example/hooks/runkite", "secret": "..." }
}
```

That registers a `policy_decision` webhook sink (HMAC + retries + dead letters) — it does **not** block connector/MCP paths.

**Run stream DX:** when `policy.run_events` is true (default), session and MCP `tools/call` denials / pending HITL also publish a `tool_auth` RunEvent on the run's event broker so Agent Protocol SSE/WS clients see the decision in-stream (`effect`, `reason_code`, connector, tool, optional `action_id`). Event IDs use `{run_id}_tool_auth_{hex}` (not the runner `{run_id}_evt_{n}` namespace).

**Admin grant CRUD:** `langgraph.json` `policy.grants` are immutable deployment defaults. Durable overlays live in SQL `policy_grants` and are mutated via `/admin-api/policy-grants` without redeploy — DB rows win on the same `(tenant_id, agent_id, connector)`. A second create for the same key with a different `id` returns `409`. An empty `"policy": {}` section still enables the engine (fail-closed) so deployments can be 100% Admin-managed. The writing replica hot-reloads immediately; every other control-plane replica converges via a 15s SQL fingerprint poll (same cadence as cron). Mongo returns `501`.

**Connector HITL (`pending`):** a webhook or argument predicate can hold a
connector call. Admin approves or denies the row; it does not run the tool or
resume the framework. The caller retries the original active run/generation
with the same tenant, agent, connector, tool and exact argument digest. A
matching approval is consumed once; a changed payload cannot consume it.
Current explicit policy deny wins before consumption, while a matching approval
can bypass another pending result. Pending decisions are not cached. Approval
re-evaluates the stored capped/redacted display map, so predicates must not rely
on omitted secret fields. SQL stores provide durable one-shot consumption;
Mongo cannot persist pending and fails closed.

v0.7.0 also requires the immutable run incarnation
and configured connector target revision. The managed request keeps one target
snapshot through dispatch, and both capture modes enforce the same approval
match. Admin and Review refuse approval of a historical or changed target;
resubmit for fresh review when policy requires it. Credential values can rotate
without changing an otherwise unchanged descriptor. This is configuration
identity, not upstream code or schema attestation. Standalone expiring decision
approvals retain their existing contract. See
[target-bound approvals](connectors.md#target-bound-managed-approvals-unreleased)
and the [maintenance-window upgrade](ops-runbook.md#connector-target-revision-upgrade).

**v0.6.0 approval upgrade:** policy arguments reject ambiguous JSON and preserve numeric value and submitted type. MCP requires object arguments; governance retains its documented null/omitted shorthand. New digests use a v2 namespace. Old hashes and legacy empty digests are not reused. Old pending/approved rows remain audit records, but Admin refuses new approval of them and app action polling returns `rejected` with `approval_binding_legacy`. Resubmit for fresh review; do not automatically replay uncertain external writes. Drain older replicas and follow [the rollout guide](ops-runbook.md#hardening-upgrade).

**Mandatory HITL (`policy.mandatory_hitl` + Admin overlays):** rules that force matching `tool.call` **allows** to `pending` (`reason_code: mandatory_hitl`) even when static grants or the sync webhook would allow — defense-in-depth against a misconfigured/compromised PDP. Hard deny still wins (never elevated to pending). Empty `agent_id` = whole tenant; empty `tools` = every tool on that connector. Same `pending_actions` / Admin approve / one-shot retry path as webhook pending. `langgraph.json` `policy.mandatory_hitl` are immutable deployment defaults; durable SQL overlays (`mandatory_hitl_rules`) via `/admin-api/mandatory-hitl` win on the same `(tenant_id, agent_id, connector)` — writer hot-reloads; siblings converge via the 15s overlay poll. Break-glass skips `Decide` entirely, so it also skips this override; the break-glass audit Attrs note `mandatory_hitl_bypassed` when a rule would have matched. Mongo → `501`.

**Run admission:** every `createRunCtx` path shares one `hooks.Gate` pipeline (same as `preflight_hooks`). The admission Gate checks, in order: (1) SQL kill/pause switch for tenant or tenant+agent, (2) agent-scoped authz `agents:<id>:run` (route-level write only on run-create paths; blanket `write`/`admin` still allow any agent; empty permissions stay unrestricted), (3) when policy is enabled, `Decide` at stage `run.create` (skips connector grants; optional sync webhook can still deny; decisions are audited; break-glass may skip this step), (4) optional `finops.budgets` UTC-day caps (tenant and/or `tenant/agent`) against durable `usage_events` + run counts — soft caps allow + `budget_soft` audit; hard caps deny with `budget_exceeded` (break-glass does **not** bypass hard budgets). Deny → HTTP 403. Kill activation (`POST /admin-api/kill-switches`, Admin UI `/admin/kill`) refuses new creates immediately on every replica (SQL read) and, unless `pause_only`, drains **all** pending/running runs in scope on the writing replica (paginated SearchRuns until empty; interrupted/success/error/timeout are already terminal and are not re-cancelled).

**Run manifest:** the same `createRunCtx` call that runs admission also freezes a dispatch-time snapshot of what the run was authorized to do — resolved agent + version, runner kind, `connector_needs`, `allowed_tools` (nil = unrestricted, present including empty = deny-all), whether connector `policy` was configured for this dispatch, and the requesting principal's identity/permissions when auth is configured. Written once to `run.metadata.run_manifest` and, when the run is actually dispatched to a runner, copied verbatim onto the queued `RunAssignment` so the control plane, the runner, and Admin/audit all reference the identical record — not live config, which can drift after dispatch. LLM cache hits also get a metadata manifest (no `RunAssignment`, since no runner runs); A2A delegation never takes the cache fast-path so depth/parent bookkeeping always runs. No new storage: it rides inside the `metadata` JSON column every run already has, so it works unchanged on every state backend. `GET /admin-api/runs/{id}` has always returned it; Admin UI surfaces it as a formatted card on Run detail (raw JSON one click away). This is a record of intent, not an enforcement layer — the fail-closed grants, mandatory HITL, and break-glass mechanics above are still what actually decides allow/deny/pending; the manifest is what you check afterward to see what any of them were actually working with. See [Run manifest](../site/support/run-manifest.html).

**Usage / FinOps:** optional `finops` in `langgraph.json` — `pricebook`, `budgets`, `alerts.soft_pct` (default 80), `reservation` (`usd_per_run` / `tokens_per_run` / optional `hold_ttl`), and `routing` (`enabled`, `soft_pct`, `aliases`). Terminal runs with `Output.usage` write SQL `usage_events`; open creates may take `usage_holds` counted toward day caps until terminal release. Soft/hard/approach trips emit webhook `budget_alert` and audit rows (`budget_soft` / `budget_exceeded` / `budget_alert`); near-cap alias rewrites audit `budget_route` (does not bypass hard deny). Optional `on_hard_breach: cancel_inflight` drains pending/running runs in scope on hard breach (audit `budget_kill`). Two more audit rows cover usage-integrity gaps rather than budget thresholds: `usage_unpriced` (tokens present, no `cost_usd`, and a non-empty pricebook missing that model id — priced $0 by omission, not by design) and `usage_unmetered` (a reply came back but no extraction path found any token/cost data at all in any recognized shape — a brand-new/unrecognized provider or framework integration, distinct from an agent that genuinely made no LLM call). LangGraph (Python + TypeScript) and adapters that expose metrics (CrewAI `usage_metrics`, LlamaIndex/LangChain/AutoGen best-effort) emit top-level `usage`. Admin: `/admin/spend` plus `GET /admin-api/usage/{summary,alerts,holds,export}`. Mongo → `501` / budgets fail-open.

**Break-glass windows:** a separate SQL table (`break_glass_windows`) for time-bounded **policy** bypass — not kill columns. While a window is active for the tenant (or tenant+agent), admission and connector gates skip `policy.Decide` for `run.create` / `connector.session` / `tool.call` after a durable audit row is written (`reason_code: break_glass`). Fail-closed if the audit write fails. Hard max duration **24h** on mint. Does **not** bypass kill, `agents:<id>:run`, `admission_limits`, rate limits, or **hard finops budgets**. Admin: `GET|POST /admin-api/break-glass`, `GET|DELETE /admin-api/break-glass/{id}`, UI `/admin/break-glass`. Mongo → `501`.

Optional **`admission_limits`** (occupancy/quota, not request-rate): per-tenant / per-agent `max_concurrent` (pending+running) and `max_daily` (UTC day), enforced atomically with run INSERT under a scope lock. Exceed → HTTP 429. See [Configuration](configuration.md).

### Bring your own PDP (sync `policy.webhook`)

Runkite does not ship a proprietary policy product. You point `policy.webhook.url` at **your** decision service (OPA, Cedar, internal ABAC, or a 50-line script). The control plane sends `policy.decide` JSON (HMAC optional), fail-closes on errors, and enforces `allow` / `deny` / `pending` on connector paths.

Reference example (deny `delete_repo`, pending `transfer_funds`, HMAC + `self_check.py`): [`examples/policy_webhook/`](../examples/policy_webhook/). If that PDP works against the contract, any service that speaks the same envelope can replace it. This governs the **control plane** (connectors / admission), not arbitrary in-graph tools that never call a Runkite connector.

Example:

```json
"policy": {
  "default_effect": "deny",
  "grants": [
    {
      "id": "acme-sales-sf-read",
      "tenant_id": "acme",
      "agent_id": "sales-assistant",
      "connector": "salesforce",
      "tools": { "allow": ["query", "getRecord"], "deny": ["updateRecord"] }
    }
  ]
}
```

Absent / empty `policy` preserves V1 open access after runner auth + run-binding.

<a id="revision-bound-review-and-lifecycle-controls-unreleased"></a>

## Revision-bound review and lifecycle controls (v0.7.0)

v0.7.0 adds `policy.approvals` for operations already held by policy.
`required_approvals` ranges from one to five; `exclude_requester` prevents exact
requester self-review. Reviewer identities are authenticated and compared exactly,
with no case folding or user directory. Votes are transactional history; partial
quorum remains pending and cannot dispatch. The immutable requester comes from
the durable managed run principal or authenticated standalone caller. Managed
requester exclusion requires the manifest's server-captured
`principal.verified_identity: true`; legacy principals and fallback display names
do not establish verified requester authority. A changed
standalone separation/quorum plan requires fresh review and is rejected when
polling an obsolete result. Standalone still creates no run and gains no managed
kill, spend or break-glass controls.

Managed review additionally binds current configured policy and optional observed
MCP tool schema, alongside arguments, target, generation and incarnation. A manual
PDP revision label does not attest remote policy execution; schema identity does
not pin a remote implementation. Retained historical managed rows with missing
revisions cannot authorize new effects.

Action export inserts a durable metadata delivery in the same SQL transaction as
each opted-in action event. Export retries send the same evidence ID over HTTP;
they do not retry a tool. Registered Vault database bindings issue credentials
only to a trusted configured consumer. Vault retains authentication, encryption
and role/revocation authority. Runkite retains bounded lifecycle metadata and an
encrypted cleanup handle, never a database username/password record. Issuance and
revocation can remain unknown, requiring provider investigation.

See [review design](internal/approval-authority-and-quorum.md),
[export design](internal/action-delivery-outbox.md),
[credential design](internal/credential-lifecycle.md), and
[operations](ops-runbook.md#security-completion-upgrade). These v0.7.0
controls are not a guarantee of exactly-once external effects or arbitrary-code safety.
