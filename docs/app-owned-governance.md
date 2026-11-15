# App-owned tool governance: installation and operator setup

Use this guide when a Python or Node.js application keeps its own agent loop and
wants Runkite to decide whether a tool call may proceed. Both independent
`runkite-governance` packages first ship in v0.8.0. The
[website walkthrough](https://getrunkite.com/support/sdk-integration.html#governance-setup)
and this guide describe the same operator and application responsibilities.

## Why a separate governance package?

`runkite-runner` executes work dispatched by the control plane and creates
managed Threads/Runs. `runkite-governance` is a small HTTP decision client inside
an existing application. Installing either does not install the other.

- The Go `runkite` binary supplies the self-hosted API, policy engine and Admin.
  Neither language helper replaces that server.
- The [PyPI helper](https://pypi.org/project/runkite-governance/) lets any Python
  backend call Decide and poll an approval without installing a runner or an
  agent framework. The base client uses only the standard library and requires
  Python 3.11 or newer. The Microsoft function middleware is an optional extra.
- The [npm helper](https://www.npmjs.com/package/runkite-governance) provides the
  same decision door to server-side JavaScript and TypeScript on Node.js 22 or
  newer. It ships ESM JavaScript and declarations with no runtime dependencies.
  `createGovernedExecute` can guard an ordinary single-result tool executor.

An existing AI SDK or Microsoft application can therefore add tool governance
without moving its model loop into a runner. These are general decision clients,
not packages restricted to those two frameworks. For managed dispatch, history,
usage and cancellation instead, follow [SDK execution ownership](client-sdk.md).

## 1. Prepare the self-hosted plane

Use a running Runkite installation with primary authentication and a SQL state
backend: Postgres, MySQL or SQLite. SQL supplies the pending queue, durable audit
and Admin policy overlays. Mongo is not an equivalent approval/audit backend.
For production HA, use the documented Postgres and Redis profile.

Merge the snippets below into your existing full configuration. They are not a
standalone configuration: the loader still requires `graphs`, even when this
particular application never dispatches a managed agent. Preserve the existing
storage, runner authentication and global operator access. No runner needs to
execute the app's Decide requests. Use HTTPS outside a local loopback demo.

See [installation](../site/support/install.html),
[configuration](configuration.md) and [Admin login](../site/support/admin-login.html).

## 2. Give the app and reviewers separate credentials

The following names are placeholders. Provision real credentials through your
existing deployment process and keep the app's key in its server environment.
Do not ship that key to a browser, mobile client or model prompt.

```json
{
  "auth": {
    "type": "api_key",
    "strict_permissions": true,
    "keys": {
      "replace-with-app-api-key": {
        "name": "notes-backend",
        "tenant_id": "default",
        "permissions": ["governance:decide"]
      },
      "replace-with-reviewer-api-key": {
        "name": "notes-reviewer",
        "tenant_id": "default",
        "permissions": ["governance:approve"]
      }
    }
  }
}
```

The app key can submit decisions and poll actions in its tenant. Ordinary
`read`, `write` and `agents:<id>:run` permissions do not grant those routes.
The authenticated key selects the tenant; a submitted `tenant_id` only checks
that assertion. Keep `agent_id`, `connector`, `tool` and every authorization
relevant argument under trusted server control. `governance:decide` does not
bind the key to one agent/tool name inside that tenant.

The reviewer uses `/admin/review` with a verified identity and explicit tenant.
`governance:approve` permits that tenant's approval review; `governance:read`
permits metadata-only review. Neither grants global Admin access. Keep the
operator's existing `admin` credential separate for editing grants and seeing
cross-tenant audit. `auth.admin_keys` alone is not an app Decide credential.

For JWT or webhook authentication, map the same explicit permission, verified
identity and tenant through the primary provider. If enabling requester
exclusion or approval quorum, give each reviewer their own verified identity;
shared credentials cannot distinguish people. Upgrade every HTTP replica before
enabling reviewer roles. See [tenant review](../site/support/tenant-review.html)
and [approval authority](internal/approval-authority-and-quorum.md).

## 3. Grant the logical tool and choose what needs review

This example permits `notes-app` to request `workspace.createNote`, but holds
every such operation for review. Both names are logical policy identifiers.
This path needs no MCP connector definition and no registered runner agent.

```json
{
  "policy": {
    "default_effect": "deny",
    "audit": true,
    "audit_required": true,
    "grants": [{
      "id": "notes-create",
      "tenant_id": "default",
      "agent_id": "notes-app",
      "connector": "workspace",
      "tools": { "allow": ["createNote"] }
    }],
    "mandatory_hitl": [{
      "id": "notes-review",
      "tenant_id": "default",
      "agent_id": "notes-app",
      "connector": "workspace",
      "tools": ["createNote"]
    }]
  }
}
```

With this SQL configuration, required audit turns a persistence failure into a
denial. Auditing otherwise defaults to best effort. No `policy` section disables
the decision endpoint, which returns HTTP 409. An empty `"policy": {}` enables
the engine and denies unmatched calls until grants exist.

You may instead use **Admin > Grants** and **Admin > Mandatory HITL** for durable
SQL overlays on an enabled policy engine. Use the same exact tenant, agent,
connector and tool names. An overlay replaces the configuration grant at the
same tenant/agent/connector key. The writing replica reloads immediately;
siblings poll within about 15 seconds. Configuration changes, including
argument predicates, require restarting the plane. Do not confuse that
convergence interval with an instantaneous fleet-wide revocation guarantee.

A mandatory HITL rule holds an otherwise allowed call. It does not turn a deny
into pending or grant permission on its own. For selective review by argument
value, use the existing `policy.predicates`; they apply to app-owned Decide as
well as connector tool calls. Predicates may deny or hold, never allow. See
[Grants and HITL](../site/support/grants.html#argument-predicates).

## 4. Install only the helper your app needs

```sh
# Python application
python -m pip install runkite-governance
# Optional Microsoft function middleware in that Python application
python -m pip install 'runkite-governance[agent-framework]'
# JavaScript or TypeScript server application
npm install runkite-governance
```

Set `RUNKITE_HTTP_URL` to your own plane's URL and `RUNKITE_API_KEY` to the app
credential in that application's server environment. These variable names are
used by the examples; the clients receive their values explicitly and do not
automatically discover a deployment.

These two equivalent probes request a decision only. They execute no tool and
need no model-provider account. Use a stable, unique application operation ID
for each real proposal and persist it with the proposal before continuation.

Python:

```python
import os
from runkite_governance import GovernanceClient

governance = GovernanceClient(
    os.environ["RUNKITE_HTTP_URL"],
    api_key=os.environ["RUNKITE_API_KEY"],
    timeout_ms=10_000,
)
decision = governance.decide(
    agent_id="notes-app",
    connector="workspace",
    tool="createNote",
    args={"title": "Release notes", "body": "A draft"},
    run_id="python-note-probe-1",
)
print(decision)  # This is permission metadata, not an execution result.
```

JavaScript (save as an `.mjs` file, or use an ESM project):

```javascript
import { createGovernanceClient } from "runkite-governance";

const governance = createGovernanceClient({
  baseUrl: process.env.RUNKITE_HTTP_URL,
  apiKey: process.env.RUNKITE_API_KEY,
  timeoutMs: 10_000,
});
const decision = await governance.decide({
  agentId: "notes-app",
  connector: "workspace",
  tool: "createNote",
  args: { title: "Release notes", body: "A draft" },
  runId: "node-note-probe-1",
});
console.log(decision); // This is permission metadata, not an execution result.
```

Expect `effect: pending` and an action ID with the example policy. Python uses
`action_id` / `reason_code`; JavaScript uses `actionId` / `reasonCode`. A policy
deny is also an HTTP 200 response, so check `effect`, not just transport success.
Authentication, network, timeout and malformed-response failures are not
permission to execute. Let them stop the operation or record a blocked result.
The probes deliberately have no fallback executor.

For real integration, use the [TypeScript guard](../typescript/runkite-governance/README.md)
or the Python client before your tool. Optional Microsoft middleware and its
argument constraints are in the [Python README](../python/runkite_governance/README.md).
The base Python client does not automatically wrap arbitrary functions.

## 5. Review, then let the app continue

Open **Admin > Pending** using the global operator login, or **Review > Approvals**
at `/admin/review` using the tenant reviewer. Locate the returned action ID and
check tenant, agent, tool, requester, arguments and any approval count. The
argument display is capped and redacted; approval binds the original argument
digest, not a replacement value typed by a browser.

After review, the application's own continuation route calls
`governance.action(saved_action_id)` in Python or
`await governance.action(savedActionId)` in JavaScript. It must authenticate the
app user, load the saved original arguments and check the returned `status`:

- `pending`: remain held. A successful reviewer vote can still be short of quorum.
- `approved`: the app may execute that exact saved operation.
- `rejected` or `consumed`: do not execute. Expired pending rows and obsolete
  approval bindings are rejected. Obtain a fresh decision and review when
  appropriate.

Proposals still awaiting review expire after 24 hours. This is not an execution
deadline for an already-approved operation: polling can keep returning approved
after that timestamp. Enforce any execution deadline in the app's saved operation.
Approval does not call the app, execute its tool or resume its model loop.
Polling does not consume approval. The app must record execution atomically with its local
effect, use a downstream idempotency key or use its existing outbox. An operation
`run_id` alone cannot prevent duplicate side effects. Do not hold a serverless
request open for a human review; return promptly and resume through a later
authenticated application request. `resolve()` is only a bounded-wait option.

## 6. Check the right records and diagnose failures

**Admin > Pending** and the tenant's **Review > Approvals** show held decisions.
With SQL auditing enabled, **Admin > Audit** shows the submitted policy decisions
and supports filtering by operation `run_id`. **Admin > Threads**, **Runs**,
**Spend**, managed manifests and managed connector-action evidence are not
populated by this decision-only path. An allow or an audit decision is not proof
that the app ran a tool. Keep the app's own execution log and correlation ID.

The helper sends the tool identity and arguments you supply; it does not
automatically upload the rest of a chat. The plane evaluates the full submitted
arguments. SQL audit and pending records retain identity/decision metadata, the
argument digest and a capped, redacted argument display. That display can still
contain application content. Apply your access, backup and retention rules to
those records, and keep the exact executable proposal in the app's own store.

- **401:** verify the app credential and primary auth provider.
- **403:** verify `governance:decide`, the authenticated tenant and any requester
  identity requirement. Inspect `effect: deny` and `reason_code` separately for
  policy denials returned with HTTP 200.
- **409:** add/enable the plane's `policy` section.
- **400:** check names, strict JSON arguments and the operation ID. A pending
  decision requires a nonempty `run_id`.
- **501:** pending needs Postgres, MySQL or SQLite. Required audit also needs SQL
  and refuses an unsupported deployment at startup.
- **No approval row:** a deny creates no pending action. Check Audit and matching
  grant names before expecting a hold. Review cannot override a policy deny.
- **Approval appears stuck:** use the app's saved action ID, check expiry and
  quorum, and ensure the app actually polls/continues. Admin does not push a
  continuation into an app-owned loop.

This boundary provides no run kill switch, break-glass, run spend cap, process
sandbox, downstream secret injection or observation of calls that skip Decide.
If you need those managed operational records and controls, choose the relevant
[runner profile](runners.md) and its actual connector gates. For app-owned
execution, the app continues to own tool credentials and execution safety.

## Validate before enabling a real side effect

Using isolated local data, confirm a matching grant plus HITL returns pending,
review changes polling to approved without executing a tool, and only the app
can continue the stored operation. In separate test scopes, remove the hold to
check allow and remove the grant to check deny. Do not loosen a live grant for
testing. Verify rejection, expiry, a plane outage and repeated continuation all
leave the expected application record without duplicate side effects.

The runnable [AI SDK example](../examples/governance_ai_sdk/README.md) exercises
application persistence and idempotent continuation. The
[Microsoft example](../examples/governance_agent_framework/README.md) exercises
local function middleware. Their offline tests use no paid provider. See
[governance distribution](internal/governance-distribution.md) for installed
artifact qualification and immutable publication boundaries.
