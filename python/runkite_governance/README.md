# Runkite governance helper

A standard-library-only Python client for `POST /governance/decide` and `GET /governance/actions/{id}` on your self-hosted Runkite plane. It does not depend on the runner package or an agent framework.

Use this helper when an existing Python service keeps its agent loop and needs
an allow, deny or pending answer before a tool call. It works with ordinary
application code; Microsoft function middleware is an optional integration, not
a requirement. A managed `runkite-runner` instead executes assignments
dispatched by the control plane and does not require this helper.

This helper has its own distribution, separate from `runkite-runner`, first
published in Runkite v0.8.0. Published v0.7.0 did not include it. Install a
verified registry release with:

```sh
python -m pip install runkite-governance
```

For source that has not been published, install from a checkout:

```sh
python -m pip install ./python/runkite_governance
```

The runtime requires Python 3.11 or newer. Installing a managed runner does not
install this decision-only helper.

## Configure the plane and application

The administrator configures authentication, tenant-scoped grants, optional
human approval and durable SQL review storage on the control plane. The
application installs this helper and supplies its plane URL and server-side API
key. Follow the [operator and application setup guide](https://getrunkite.com/support/sdk-integration.html#governance-setup)
or the [repository handbook](https://github.com/getrunkite/runkite/blob/main/docs/app-owned-governance.md)
for the complete setup, Admin review flow and troubleshooting.

Import the same module after a registry or checkout installation:

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
    run_id="stable-app-turn-and-tool-call-id",
)
```

Your application executes after `allow`, refuses after `deny`, and persists the action ID and exact original arguments after `pending`. On a later application request, use `governance.action(action_id)` and execute only after `approved`. Rejected or consumed actions must not execute. Implement idempotency in the application's database: GET does not consume approval.

If `decide`, `action` or `resolve` raises, stop the operation without executing
the tool. HTTP/protocol and deadline failures can raise `GovernanceError`;
network failures can also propagate standard-library transport exceptions.
Do not treat an exception, an unavailable plane or a timeout as allow. Handle
the failure at the application's request boundary and retain any saved pending
operation for an explicit later continuation.

`resolve(...)` performs a bounded wait and returns an allow/deny outcome. Its deadline covers the initial decision and every poll. `decide` and `action` accept `timeout_ms` and a `threading.Event` as `cancelled`. Cancellation stops the caller promptly. Blocking standard-library I/O unwinds separately with a socket timeout and response-chunk checks; a bounded shared worker pool prevents unlimited abandoned resolver/socket threads. Do not hold a serverless request open for long operator approval.

Use a server-side key with `governance:decide`. Tenant identity comes from that key. The caller retains its model loop and tool execution. This path creates no Runkite thread or run and does not apply run kill switches, break-glass or spend caps. It is separate from the deliberately deferred full client SDK.

## Optional Microsoft Agent Framework middleware

For a verified registry release, install the optional middleware with:

```sh
python -m pip install 'runkite-governance[agent-framework]'
```

For unpublished source, install the extra from a checkout:

```sh
python -m pip install './python/runkite_governance[agent-framework]'
```

The extra pins the tested framework core; model-provider packages remain
application choices.

Import `GovernanceFunctionMiddleware`, `GovernedFunction`,
`GovernanceBlockedError` and `PendingInvocation` from
`runkite_governance.agent_framework`. Register exact local `FunctionTool` objects,
server-owned policy identities and an application operation-ID callback. Install
the guard last, after argument repair. It snapshots JSON arguments and Python
defaults, decides once, and reaches native execution only after allow. Deny and
pending stop the agent with a native `MiddlewareFailure`; an optional async
`on_pending` callback persists the exact frozen proposal first.

The app owns pending continuation and execution idempotency. Rich Python types,
coercing validators and provider-executed tools are outside this adapter's scope.
See the [framework example](https://github.com/getrunkite/runkite/tree/main/examples/governance_agent_framework/README.md)
for a durable application transaction, offline streaming and non-streaming tests,
and supported argument constraints. Execution retains native mutable containers;
the app must not mutate invocation arguments after authorization. Importing the
base client does not import the optional framework. This extra is not a runner
adapter and creates no run.

License: Apache-2.0. See `LICENSE` and `NOTICE`. Third-party dependencies retain
their own licenses.
