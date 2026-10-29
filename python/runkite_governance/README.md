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

License: BUSL-1.1, with the repository's additional use grant. See `LICENSE`.
