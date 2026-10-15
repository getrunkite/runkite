# Vercel AI SDK app fit

> **Status:** proposed (2026-10-16). Not started. This is the plan for a later
> change, written so it can be checked against source. It does not change the
> locked release in `embedded-governance-next-release.md`.
>
> **One-line goal:** a Vercel AI SDK app keeps its own model loop and calls
> Runkite at the tool hook. Partner Assistant is the concrete app. It is a
> Nuxt app. The same hook exists for a Next.js app because the hook is on the
> AI SDK, not on the web framework. This plan does not migrate Nuxt to Next.js,
> and it does not turn the app into a Runner-Protocol runner.

## 1. What "run through Runkite" means

Runkite owns the operational check. The app owns the chat.

The app calls `POST /governance/decide` before a write tool runs. On allow,
the app executes the tool. On deny, the app does not. On pending, the app
waits for an operator in the Admin pending page, then executes only if the
poll says approved. Threads, messages, streaming, and the workspace files
stay in the app.

That is the fit the governance door was built for. Moving Partner Assistant
onto `runkite serve` so the plane owns the run is a different project, and
it is the force fit this plan does not do.

## 2. Already shipped

Checked against `main` at `dca67c9` (2026-10-16).

| Piece | Where | Commit |
|---|---|---|
| `POST /governance/decide` | `internal/api/governance_decide.go` | `c6fbd43`, pending behavior in `d30ead9` |
| `GET /governance/actions/{action_id}` | same file | `d30ead9` |
| TypeScript helper | `typescript/runkite-governance` | `e52b828` |
| Python helper | `python/runkite_governance` | `dca67c9` |

The TypeScript package name is `runkite-governance`. `package.json` sets
`"private": true` and `"exports": "./src/index.ts"`. It is on GitHub. It is
not on npm. `import { createGovernanceClient } from "runkite-governance"` does
not resolve from the registry.

`resolve()` polls until the row is no longer pending. `approved` comes back
as `effect: "allow"`. `rejected` and `consumed` come back as `effect: "deny"`.
The default wait is 60 seconds (`DEFAULT_TIMEOUT_MS` in
`typescript/runkite-governance/src/index.ts`), then it throws
`GovernanceError` with status 408. The helper has no abort signal.

Kill switches and spend caps are not on this door. They run in
`admissionGate` (`internal/api/admission.go`) when Runkite creates a run.
An inbound webhook for pending is still a later slice. Both stay out of
the locked release note.

## 3. What Partner Assistant is today

Repo: `partner-assistant`. Not this repository.

- One Nuxt server. The chat turn is
  `POST /api/workspaces/:workspace_id/threads/:thread_id/messages`
  in `server/api/workspaces/[workspace_id]/threads/[thread_id]/messages.post.ts`.
- The model loop is `createChatAgent` in `server/agent/index.ts`. It
  constructs a `ToolLoopAgent` from `ai`. `package.json` depends on
  `ai` `^7.0.130`.
- `IAgentScope` is `userId`, `workspaceId`, `threadId`. The route passes
  those three. It does not pass a turn id.
- Tools are built in `server/agent/tools.ts` by `workspaceTools(files)`.
  The function receives the file client only. It does not receive `scope`.
- Six file tools. The three writes pass `needsApproval: false` into
  `files-sdk` (`uploadFile`, `deleteFile`, `copyFile`). The comment in
  that file says this is mandatory because there is no approval UI, and a
  write would hang forever.
- The assistant message id is minted with `generateId()` in the route
  after `createChatAgent` returns. The agent does not see that id today.

AI SDK 7 deprecates `needsApproval` on `tool()` for `generateText`,
`streamText`, and `ToolLoopAgent`. The replacement is `toolApproval` on
the call or the agent. Documented at
`https://ai-sdk.dev/docs/ai-sdk-core/tools-and-tool-calling` (read
2026-10-16). A per-tool function receives the tool input. A single
function for every tool receives `toolCall`. Those are different
signatures. `undefined` and `'not-applicable'` execute the tool with no
approval parts. `'approved'` records an automatic approval, then executes.
`'denied'` and `{ type: 'denied', reason }` record a denial and do not
execute. `'user-approval'` ends the generation and waits for a later
request where the chat user answers. It does not block inside the
function.

`files-sdk` still puts `needsApproval` on the tool object. This plan does
not claim what the SDK does if that flag and `toolApproval` are both set.
The wiring removes `needsApproval: false` from the three writes and puts
the policy only on `toolApproval`.

## 4. Proposed wiring

Not built. Partner Assistant changes belong in that repo. None of them
are in this commit.

Put `toolApproval` on the `ToolLoopAgent` inside `createChatAgent`, where
`scope` already exists. Leave `workspaceTools` as the tool set.

Use one stable `agent_id`, proposed value `partner`. Do not use
`workspaceId`. A grant matches the agent id string, so a per-workspace
id would need a grant per workspace. `connector` is `workspace`, the
logical name from the locked design. It is not an MCP server lookup.

`run_id` is one turn, not the thread. The route should mint the assistant
message id before `createChatAgent` and pass it in. Two identical write
calls in one thread must not share an open pending row. Dedup is
`(run_id, generation 0, connector, tool, args_digest)`.

Only the three write tools call Runkite. Reads stay off `toolApproval`.

Two approvers. Do not collapse them.

- Policy allow: return `undefined`. The tool runs. The chat transcript
  does not gain an approval part.
- Policy deny: return `{ type: 'denied', reason }` using the decision
  reason. The tool does not run.
- Policy pending (operator in Admin): `await governance.resolve(...)`
  inside the async function. If the outcome is allow, return `'approved'`.
  If it is deny, return `{ type: 'denied', reason }`. Do not return
  `'user-approval'` for a Runkite pending row. The chat user's click
  would not approve that row, and an Admin click would not satisfy a
  `'user-approval'` the function already returned.
- Chat-user approval stays a separate product. It needs a control in
  Partner Assistant that does not exist. It returns `'user-approval'`
  only when Runkite has already allowed the call, or when Runkite was
  not asked to hold it. This plan does not build that control.

`resolve()` holds the open chat request until the operator acts or the
timeout fires. The route already aborts the agent when the client
disconnects (`AbortController` on `req` `close`). The helper ignores
that signal today. Before this wiring ships, `resolve()` needs a way to
stop polling when that signal aborts. Otherwise a closed tab keeps
polling until the timeout.

The first useful policy can allow all three writes and hold none of them.
The app then behaves as it does now, plus an audit row and a network hop,
and a grant change can deny a tool without another app deploy. Operator
hold is a grant plus a `mandatory_hitl` rule on the specific tool, not a
code branch per tool.

## 5. What has to exist before the app calls it

- A running control plane the Nuxt server can reach. Laptop: SQLite.
  A deployed app that must survive restarts wants Postgres. That is
  existing Runkite deployment, not a new backend.
- An API key with `governance:decide`. `read`, `write`, and
  `agents:<id>:run` do not imply it. Tenant is the key's tenant. A
  `tenant_id` in the body that disagrees is 403.
- One grant: agent `partner`, connector `workspace`, tools `uploadFile`,
  `deleteFile`, `copyFile`, unless a tool should be denied. Add
  `mandatory_hitl` only for a tool an operator must hold.
- Someone watching Admin Pending Actions, only if a tool is held.
- A way for the Nuxt app to import the helper. Proposed: vendor
  `typescript/runkite-governance/src/index.ts`, or a git dependency on
  this repo. Do not publish it as `@runkite/sdk`. That package is the
  deferred full client SDK in `docs/client-sdk.md`.

## 6. Later, not this plan's first change

- **Turn admission.** A call before the model runs, so a kill switch or
  a daily budget can refuse the turn. That is §4.4 of the locked note.
  The tool door cannot do it, because the Gemini call happens inside
  Nuxt and never enters `admissionGate`.
- **Abortable poll.** Small helper change. Required before `resolve()`
  runs inside the chat request. Listed in §4 because the wiring blocks
  on it.
- **Inbound webhook.** So a held tool does not need a poll loop. Still
  later. The chat request would still need a way to resume.
- **Shared credentials.** Only for a tool Runkite should call outbound
  through the connector proxy. The six file tools use the app's own
  workspace storage. `decide` does not broker those credentials.

## 7. Do not do

- Do not put Partner Assistant on a gRPC runner or rewrite its threads
  onto Agent Protocol.
- Do not migrate the app from Nuxt to Next.js in order to use Runkite.
- Do not map `pending` to `'user-approval'`.
- Do not use `workspaceId` as `agent_id` or `threadId` as `run_id`.
- Do not describe the tool door as a kill switch or a spend cap.
- Do not start this wiring in the Runkite tree beyond the helper gap in
  §6. The app change is a separate repo.

## 8. Order of work

Proposed. None of it is started.

1. Review this note.
2. Add an abort signal to `GovernanceClient.resolve` so a closed request
   stops the poll.
3. In Partner Assistant, mint the turn id first, set `toolApproval` on
   `createChatAgent`, stop passing `needsApproval: false`, and vendor or
   link the helper.
4. Run a control plane with the `partner` / `workspace` grant. Confirm
   allow executes, deny does not, and a held tool resumes after Admin
   approve.
5. Leave kill, budget, and the webhook for their own notes.
