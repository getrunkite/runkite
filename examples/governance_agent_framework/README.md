# Microsoft Agent Framework with app-owned governance

The optional `GovernanceFunctionMiddleware` checks local Python function calls
through Runkite's existing `POST /governance/decide` API. The app keeps its model
client and loop. This integration uses `agent-framework-core==1.23.0` and the
framework's public `FunctionMiddleware`, `FunctionTool` and `Agent` APIs.

Only allow reaches native function execution. Deny and pending raise
`GovernanceBlockedError`, a native `MiddlewareFailure`, so the model loop cannot
silently retry a blocked operation as an ordinary tool error. An outage or invalid
configuration also stops execution. The middleware decides once and never polls,
retries, bypasses native approval, or resumes a suspended agent.

## Install and test

From the repository root, use Python 3.11 or newer:

```sh
python3 -m venv examples/governance_agent_framework/.venv
examples/governance_agent_framework/.venv/bin/python -m pip install -e './python/runkite_governance[agent-framework]'
PYTHONPATH=python examples/governance_agent_framework/.venv/bin/python -m unittest discover -s examples/governance_agent_framework -p 'test_*.py'
```

`make test-governance-agent-framework` also runs the focused middleware checks.
Tests use the installed framework's real function loop and streaming pipeline,
an offline scripted model client, and a fake governance transport. They need no
provider SDK, credentials, model endpoint or running plane. Dependencies must be
installed first. The base governance package still has no runtime dependencies;
Microsoft's framework loads only when its optional module is imported.

## Use your existing model client

`application.py` provides a local note operation with durable pending storage and
idempotent continuation. From this example directory, pass your existing
framework client and a server-owned operation ID:

```python
from application import NoteStore, build_notes_agent, continue_note
from runkite_governance import GovernanceClient
from runkite_governance.agent_framework import GovernanceBlockedError

governance = GovernanceClient("http://localhost:2026", api_key="your-server-side-key", timeout_ms=10_000)
store = NoteStore("data/state.db")

# In your async request handler. Authenticate the caller and allocate this ID
# in your application. Reuse it only for the same logical note operation.
operation_id = "stored-app-operation-id"
agent = build_notes_agent(your_existing_client, governance, store, operation_id)
try:
    response = await agent.run("Create a note titled Release plan with body Review the changes.")
except GovernanceBlockedError as blocked:
    outcome = dict(blocked.decision)
    # Return pending or denied to the app. Pending is already stored durably.

# On a later authenticated request, load the operation ID from your app.
# Call only when the app wants to check the saved approval, not in a poll loop.
operation = await continue_note(store, governance, operation_id)
```

Use a server-side Runkite key with `governance:decide` and the intended tenant.
The [AI SDK example's policy configuration](../governance_ai_sdk/README.md#run)
also applies here: `notes-app`, logical connector `workspace`, tool `createNote`.
No MCP connector or runner registration is required. An operator approves the
pending action in Runkite Admin. The app checks that exact stored action ID on
continuation; it accepts no replacement arguments or browser approval flag.

The sample supports one logical note operation per application request. Reusing
an operation ID for distinct arguments fails. For multiple tool calls, allocate
and persist separate operation IDs in the app. The middleware's `run_id` field is
an operation correlation value, not a Runkite run or an idempotency guarantee.

`NoteStore` stores the exact proposed arguments, including Python defaults.
SQLite serializes continuations and commits note creation with its completion
record in one transaction. Concurrent, repeated and restarted continuations
return the stored result. Rejected or consumed actions never create a note.
For external effects, use the downstream API's idempotency key or your existing
outbox. A local database transaction cannot make a remote write exactly once.
Protect the continuation route and operation ownership in your application.

## Attach middleware to another agent

Register the exact local `FunctionTool` objects in `GovernedFunction` bindings.
Set `agent_id`, connector and tool names on the server. Supply an `operation_id`
callback that returns your stored application operation identity. An optional
async `on_pending(PendingInvocation)` callback persists the action ID and frozen
arguments before the pending error is raised. Without this callback, the app
still receives the decision metadata, but the adapter stores no continuation.

Install this middleware last, after any argument-repair middleware. It checks
that the callable, schema and arguments did not change while awaiting policy.
Arguments and defaults must be lossless JSON values that stay unchanged under
the function's native validation. Rich Python values, coercing or non-idempotent
validators, variadic callables and positional-only parameters are unsupported.
Injected framework context and credentials stay out of policy arguments, so any
authorization-relevant target must be an explicit function argument. This adapter
governs registered local functions; hosted or provider-executed tools do not pass
through this boundary.

Execution uses ordinary native dictionaries and lists. This middleware guards
the handoff into execution; it does not isolate the function from other code in
the same process. The pinned framework may reuse prepared argument objects.
Application code and other middleware must not mutate invocation arguments
after authorization. Frozen pending proposals remain independent snapshots.

Cancellation stops decision work and prevents starting the next function. After
execution begins, the app owns cancellation, argument mutation and recovery.
Native Microsoft approval, when configured, remains effective and does not
substitute for a Runkite allow decision.

These calls create no Runkite thread or run. Admin Threads and Runs, run kill
switches, break-glass and spend caps do not cover this decision-only path. This
adds no second scheduler, policy evaluator, credential vault or model runtime.
