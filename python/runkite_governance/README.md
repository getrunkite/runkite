# Runkite governance helper

A standard-library-only Python client for `POST /governance/decide` and `GET /governance/actions/{id}` on your self-hosted Runkite plane. It does not depend on the runner package or an agent framework.

This distribution is prepared locally and has not been published by this change. Install from the checkout:

```sh
python -m pip install ./python/runkite_governance
```

```python
from runkite_governance import GovernanceClient

governance = GovernanceClient("http://localhost:2026", api_key="server-side-key", timeout_ms=10_000)
decision = governance.decide(
    agent_id="notes-app",
    connector="workspace",
    tool="createNote",
    args={"title": "Release notes", "body": "A draft"},
    run_id="stable-app-turn-and-tool-call-id",
)
```

Your application executes after `allow`, refuses after `deny`, and persists the action ID and exact original arguments after `pending`. On a later application request, use `governance.action(action_id)` and execute only after `approved`. Rejected or consumed actions must not execute. Implement idempotency in the application's database: GET does not consume approval.

`resolve(...)` performs a bounded wait and returns an allow/deny outcome. Its deadline covers the initial decision and every poll. `decide` and `action` accept `timeout_ms` and a `threading.Event` as `cancelled`. Cancellation stops the caller promptly. Blocking standard-library I/O unwinds separately with a socket timeout and response-chunk checks; a bounded shared worker pool prevents unlimited abandoned resolver/socket threads. Do not hold a serverless request open for long operator approval.

Use a server-side key with `governance:decide`. Tenant identity comes from that key. The caller retains its model loop and tool execution. This path creates no Runkite thread or run and does not apply run kill switches, break-glass or spend caps. It is separate from the deliberately deferred full client SDK.

## Optional Microsoft Agent Framework middleware

Install the pinned optional dependency from the checkout:

```sh
python -m pip install './python/runkite_governance[agent-framework]'
```

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
See the [framework example](../../examples/governance_agent_framework/README.md)
for a durable application transaction, offline streaming and non-streaming tests,
and supported argument constraints. Execution retains native mutable containers;
the app must not mutate invocation arguments after authorization. Importing the
base client does not import the optional framework. This extra is not a runner
adapter and creates no run.

License: BUSL-1.1, with the repository's additional use grant. See `LICENSE`.
