# Embedded Governance: design notes for the next release

> **Status:** final (2026-10-15). Design is locked. Implementation can start. Working notes,
> cross-checked against the Go source on the v0.5.2 tree. A review corrected three claims:
> `PolicyInput` does have `Generation`; kill switches and budgets do not run inside
> `Engine.Decide`; the AI SDK `needsApproval` flag is the chat user, not Admin.
>
> **One-line goal:** let an app that owns its own request/response loop (Vercel AI SDK,
> `ToolLoopAgent`, or anything similar) call Runkite's existing policy engine for a tool
> decision (grants, predicates, mandatory HITL, audit) without becoming a Runner-Protocol
> runner. This release does not enforce kill switches or spend caps. See §4.4.
>
> **Locked decisions:** `POST /governance/decide`. New permission `governance:decide`, not
> implied by `agents:<id>:run`. Tenant comes from the key. `agent_id` is caller-supplied
> and does not have to be pre-registered. Allow and deny ship first. Pending is poll-only
> until a later slice. TypeScript helper first. Kill and budget stay out.

---

## 1. The problem

`Engine.Decide` in `internal/policy/policy.go` is real and framework-agnostic. It takes a
tenant, an agent, a tool name, and arguments, and returns allow / deny / pending. Grants,
argument predicates, mandatory HITL, and the audit write sit on that function. The Admin
grant and pending pages sit on the same tables.

Kill switches and daily budgets do not. They run in `admissionGate` (`internal/api/admission.go`),
and that gate runs only when Runkite itself is creating a run. A tool-call door that only
calls `Decide` will not enforce them.

The catch for the policy engine: every existing call site assumes an in-flight run that
Runkite created and dispatched to a registered gRPC runner.

```
internal/api/server.go:881      connector session mint (run-bound proxy)
internal/api/server.go:1003     MCP tools/call
internal/api/runs.go:722        pre-warm during run creation
internal/api/admission.go:103   run.create admission (Decide at stage run.create, after kill and authz)
internal/api/admin_pending.go:114   Admin re-checks policy while resolving a pending decision
```

`docs/connectors.md` is explicit: `/internal/connectors/{name}/session` and `/mcp` require
`X-Runkite-Run-Id` + `X-Runkite-Generation`. A bare call without them gets `401
run_binding_required`. There is no public endpoint that calls `Decide` outside Runkite's
own run lifecycle.

An app shaped like a typical Vercel AI SDK agent (Nuxt or Next, `ToolLoopAgent`, tools
executed in-process, no separate runner, no gRPC) cannot reach `Decide` today without either:

- giving up its own chat loop and becoming a Runner-Protocol runner, or
- not using Runkite at all.

The second option is the right one for an app that does not yet have a shared credential,
a write with real consequences, a spend cap, or a cross-agent audit need. This note is not
an argument that every such app should adopt Runkite. It removes the architectural reason
one could not, once it needs to.

## 2. Why this is a product decision, not a one-app patch

Any team on `streamText` / `ToolLoopAgent` / a plain request-response Node or Python backend
has the same shape: the agent executes inside the HTTP request, not behind a job queue.
Runkite's README says it is not another agent framework and that it governs what happens
around the agent. A second, lighter entry point to the same policy engine completes that
promise for this shape of app.

Partner Assistant is one example of the shape, not the customer this has to land in.

## 3. Verified facts (checked against source, not assumed)

- `Engine.Decide(ctx, PolicyInput) PolicyDecision` is plain Go. `internal/policy/policy.go:311`.
  The match uses tenant, agent, connector, tool, predicates, and the optional webhook. It
  does not load a run row.
- `PolicyInput` fields: `Stage, TenantID, Principal, AgentID, RunID, Generation, Connector,
  Tool, Args, ArgsMeta, ArgsDigest, Claims, Labels`. `Generation` is on the struct
  (`policy.go:66`). `Decide` does not read it when choosing allow / deny / pending. The
  webhook body (`webhook.go`) and the audit row (`audit.go:55`) copy it through.
- Three stages: `connector.session`, `tool.call`, `run.create`. The comment on
  `StageRunCreate` says run.create skips connector matching, and that authz and kill
  switches run in the admission gate. Grant matching on connector and tool happens only at
  `tool.call` (`internal/policy/static.go:69`). That is the stage the new call should use.
- `EffectPending` is documented as connector HITL: Admin approves, then Runkite retries the
  proxied MCP call. `persistPendingAction` (`internal/api/policy.go:244`) refuses an empty
  `RunID` ("pending actions require run binding"). It does not look up a row in `runs`.
  `pending_actions.run_id` is `TEXT NOT NULL` with no foreign key. An opaque caller id
  works if it is non-empty and stable for that turn. `FindOpenPendingAction` and
  `ConsumeApprovedAction` key on run id, generation, connector, tool, and args digest. A
  fresh id on every decide call splits one approval into many rows.
- `audit.go` writes `RunID` and `Generation` as given. `ResourceType` is hardcoded to
  `"connector"`. `audit_events.run_id` is plain text, indexed, not a foreign key. Nothing
  in that write checks that a run exists.
- `FindActiveKill(tenantID, agentID)` and `checkBudgetAdmission` live on `admissionGate`,
  which is registered on the before-run hook. They are not inside `Engine.Decide`.
- `static.go` matches the connector string. It does not ask the connector registry whether
  that name is a real MCP server. The Admin grant form (`admin-ui/src/pages/PolicyGrants.tsx`)
  is a free-text connector field. The MCP proxy path is different: `handleProxyMCPRequest`
  calls `connectors.Get` and 404s an unknown name. The new handler must not reuse that path.
- Auth at every existing call site comes from `auth.FromContext` / `tenant.FromContext`,
  which Runkite fills from a request it authenticated. The new endpoint keeps that: the
  tenant is the key's tenant, and `agent_id` is the only identity the body may name (§4.3).
- Custom routes (`docs/connectors.md`) are a reverse proxy that injects `X-Runkite-Identity`,
  `X-Runkite-Tenant-Id`, and `X-Runkite-Permissions` into traffic bound for the app. They
  are not a way for the app to call into `Decide`. The header pattern is a reasonable
  precedent for how the new endpoint should name the caller.
- `docs/client-sdk.md` (decision 2026-08): do not ship `pip install runkite` /
  `npm install @runkite/sdk` until LangGraph SDK plus raw OpenAPI are not enough. A
  decide/approve helper would be a new, smaller surface than that deferred SDK.

## 4. Proposed design: the second door

A new entry point onto the same `Engine.Decide`, stage `tool.call`, with no run creation,
no job dispatch, and no Runner-Protocol runner.

### 4.1 Decide call (allow and deny)

```
POST /governance/decide
{
  "agent_id": "...",
  "connector": "workspace",
  "tool": "deleteFile",
  "args": { "key": "notes/q3.md" },
  "run_id": "stable-id-for-this-turn"
}
→ { "effect": "allow" | "deny", "reason": "...", "reason_code": "..." }
```

- Maps onto existing `PolicyInput` / `PolicyDecision`. No change to the match inside
  `Engine.Decide`. The handler sets `TenantID` from the authenticated key (§4.3).
- First slice returns allow and deny only. A `pending` effect with no way to wait is a
  deny that looks like a feature. Do not return `pending` until §4.2 exists.
- `connector` may be a logical name (`workspace`) with no MCP server behind it. Grants and
  the Admin grant page already key on that string.
- `run_id` is an opaque caller id for the audit row. It does not create a thread or a run.
  When §4.2 lands, the same id must stay stable for the turn, because pending rows are
  deduped on it.
- Pass `Generation` as `0` unless a later revision defines one. The match ignores it. The
  audit row stores it.
- The caller executes the tool. Runkite answers and writes the audit row.

### 4.2 Waitable pending (separate estimate)

New state, not a thin wrapper. Today pending means "Admin approves, then Runkite retries
the MCP call it proxied." That retry does not exist when the app executed the tool.

Needed:

- `Decide` may return `effect: "pending"` plus `action_id`.
- `GET /governance/actions/{action_id}` returns `pending` / `approved` / `rejected`.
- Poll first. `GET /governance/actions/{action_id}` is the v1 resolution path. A webhook
  is a later slice, not part of this release.
- Admin resolves the row the way it resolves connector HITL today. Reuse the pending
  pages. `handleAdminApprovePendingAction` already re-runs `Decide` and refuses the
  approve if policy now denies.
- Orphan expiry for an action nobody polls.
- `persistPendingAction` can store a caller-supplied `run_id`. It must not be empty, and
  it must not change between the decide and the poll.

Two different approvers. Do not collapse them:

- Chat user. The AI SDK `needsApproval` flag pauses the tool until that user approves
  inside the chat, on a later request. Runkite's job there is the allow/deny (and the
  audit row). The app already has to render the approval control. Partner Assistant sets
  `needsApproval: false` on the write tools because that control does not exist yet.
- Operator in Admin. `needsApproval` does not wake up when someone clicks approve in
  Admin. The app polls (or receives a webhook) and then feeds the resolution back into
  its own message stream. A long wait does not fit one open HTTP request. That resume
  step is app work. It is not free because the SDK has a flag.

### 4.3 Trust boundary

Decided:

- New permission `governance:decide`. `agents:<id>:run` and ordinary `write` do not grant
  it. `admin` still implies every permission, which is existing behavior (`internal/auth`).
- `TenantID` on `PolicyInput` comes from the authenticated key (`tenant.FromContext`), never
  from the body. `tenant_id` is not a field of this API. If a client sends one and it
  disagrees with the key, respond `403`. A matching value is accepted and ignored. A
  caller who already holds a valid key can try other `tenant_id` values and learn
  theirs from 200 versus 403. That is low severity: the key already acts as that tenant.
- `agent_id` is caller-supplied and does not have to exist in `langgraph.json` or as a
  registered agent. The Admin grant form is already free text for both agent id and
  connector (`PolicyGrants.tsx`). Isolation is the tenant on the key: a caller can only
  decide for agent ids inside their own tenant. There is no per-agent allowlist on the key.

Still required while writing the handler, not a product question: `args` are caller-supplied
JSON, unlike today's JSON-RPC body. Cap the size, redact into `ArgsMeta` through
`policy.BindArgs`, and keep predicate evaluation on that parsed value.

### 4.4 Kill switch and budget (not in the tool-call door)

`FindActiveKill` and `checkBudgetAdmission` run only inside `admissionGate`, before a
Runkite run is enqueued. Calling `Engine.Decide` at `tool.call` does not invoke them.

This release does not call them. An operator's kill in Admin keeps stopping
Runkite-dispatched runs and does not change `POST /governance/decide` answers. A spend
cap before the model call is the same: it is not a `tool.call` check, and it is not in
this release. A later slice can call `FindActiveKill` and `checkBudgetAdmission` from
the handler, or from a separate request at the start of the turn. Do not describe this
release as budget enforcement or as a kill switch for in-process apps.

## 5. Non-goals

- No requirement to become a Runner-Protocol runner. No gRPC, no `GetJob`.
- No requirement to move thread or message storage into Runkite. The app keeps its own
  database.
- No Agent Protocol rewrite. `useChat` / `UIMessage` stay as they are.
- This is not "Partner Assistant integrates with Runkite." That app does not need this
  today: one agent, six file tools, each scoped to the caller's own workspace prefix.
  The note exists so a Vercel-AI-SDK-shaped app can adopt Runkite when it crosses a
  trigger below.

### Triggers that make adoption make sense

- A second agent needs a credential the first one already uses.
- A tool writes somewhere with real consequences (email, CRM, a quote), and a human
  must approve. Chat-user approval can stay in the app. Operator approval is §4.2.
- Spend must be capped before the model call. That is a later slice (§4.4). It is not
  this release.
- Ops needs "who called which tool, when" beyond tracing. That is the audit row from §4.1.

## 6. Decisions

All eight questions are decided. None are still open.

1. **Path: `POST /governance/decide`.** Bare top-level, same as `/threads` and `/runs`.
   Not under `/admin-api/` or `/internal/`.
2. **Auth: new permission `governance:decide`.** Not implied by `agents:<id>:run` or
   `write`. Tenant on the decision is the key's tenant. See §4.3.
3. **`agent_id` does not have to be pre-registered.** Caller-supplied, matched as a
   string, same as the Admin grant form already allows. The key's tenant is the boundary.
4. **Pending resolution: poll first.** `GET /governance/actions/{action_id}`. No inbound
   webhook in this release.
5. **Rate limit: the existing `Limiter`.** Middleware already applies global, user, and
   tenant (`internal/ratelimit/ratelimit.go`). The handler calls `AllowAgent(agentID)`
   after parsing the body, same as `createRun` (`internal/api/runs.go`). No new tier.
6. **Audit row: caller-supplied `run_id` is stored as text.** `audit.go` copies `RunID`
   and `Generation` and sets `ResourceType` to `"connector"`. No foreign key to `runs`.
   `persistPendingAction` still requires a non-empty `run_id` and dedupes on
   `(run_id, generation, connector, tool, args_digest)`.
7. **SDK: TypeScript first, Python later.** Decide, then approve. Smaller than the
   deferred full client SDK in `docs/client-sdk.md`.
8. **Kill switches and budgets: not in this release.** Admin kill still stops
   Runkite-dispatched runs. It does not change this door's answers. Say that in the
   release notes. See §4.4.

## 7. Next steps

- [x] Cross-review against source (2026-10-15).
- [x] All eight questions decided. Status is final.
- [x] §4.1 implemented locally (2026-10-15). A separate written estimate was not
      produced; implementation started on request. HTTP effect is allow or deny only.
- [ ] §4.2 waitable pending. Not started. §4.4 stays out of this release.
