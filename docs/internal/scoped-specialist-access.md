# Scoped specialist caller access

Status: Unreleased, source checkout only. Published v0.9.0 has stable incoming
MCP run IDs but does not include this permission/reference contract. No release
version or install label changes as part of this feature.

## Authority

A specialist caller uses `agents:<id>:run` plus the separate additive
`agents:<id>:observe`. Run permission now admits the exact `/mcp` GET, POST and
DELETE transport lifecycle. Discovery contains only runnable agents and each
invocation checks the agent again through existing admission. Observe permission
never grants invocation. Existing run-only credentials gain no public GET reads.

Observe permits only exact public `GET /runs/{run_id}` and `/wait`, with the
stored tenant-scoped agent checked before input/output is returned. It grants no
search, thread/history/store access, stream, cost, cancellation or deletion.
Ordinary broad read/write/admin authority and no-auth local mode keep their
existing behavior. Scoped observation requires a SQL durable incarnation;
unsupported backends return 503 rather than claim a replacement fence.

Observation is agent-wide, including older retained runs, not owner-only privacy.
Existing run creation can continue a known tenant thread. Keep a dedicated tenant
and one trusted caller for the specialist example. Requester/thread ownership is
a separate authorization feature, not inferred from an operation ID or manifest.

## Stateful MCP authority

The SDK captures initialization identity. The HTTP session owner therefore
retains a digest of canonical tenant/identity, sorted unique permissions,
verified identity, provenance, extra claims and the delegated JWT assertion's
fingerprint. Canonical JSON also normalizes map ordering. Permission order and
duplicates have no effect. Changed authority is rejected before the SDK handles
a later request, even when its identity display string remains unchanged.
Reconnect after changes or token/provenance rotation. Raw claims and tokens do
not enter the ownership record or its rejection response.

Fresh authentication still happens on each HTTP request. This is not instant
revocation of already admitted calls or an external policy refresh service.
The existing sticky-session and process-local MCP session boundaries remain.

## Durable identity precondition

SQL already gives every inserted run a private immutable incarnation. No new
issuer, database table or deduplication service is added. A SHA-256 reference
binds that existing incarnation, tenant and run ID without exposing the internal
incarnation value or changing public Run JSON.

Exact public run GET/wait responses add `Runkite-Run-Reference`. Completed MCP
agent results add the same nonsecret value in `_meta["runkite/run_reference"]`,
including terminal tool errors. Save the first reference durably with the caller's
intent and supply `Runkite-Expected-Run-Reference` on later exact GET/wait calls.
These headers are identity preconditions, not authentication tokens or HTTP ETags.

- Missing retained row returns 404.
- Supplied reference mismatch or duplicate headers returns 412.
- Scoped wait without the expected reference returns 428.
- Scoped observation without a durable incarnation returns 503.
- A row deleted/replaced during an admitted wait fails without returning the
  replacement's output. Completion checks the expected incarnation inside the
  existing SQL transaction lock, including the race after the API's last read.

A supplied expected reference is honored for broad callers too. Existing broad
callers without it remain compatible. Scoped GET may acquire the first reference;
that first acquisition cannot prove the run ID was never deleted and recreated
before it. A first response can be lost before its reference is saved. Retention
and trusted caller intent are required through that interval. Neither the header
nor incoming MCP run-ID retry prevents recreation after pruning. Only retained
run records provide the existing create-retry protection.

The durable wait helper observes the captured row. It never falls back to create,
enqueue or another tool invocation. It preserves the original incarnation across
broker events, polling and completion. Unknown delivery remains unknown; external
business effects still need their own idempotency.

## Trinity caller

The checkout example keeps immutable intent and an attempt marker before sending.
It saves the first response reference in a separate mode-0600 `.run-reference`
file using its existing fsync plus exclusive-publication discipline. Concurrent
writers must agree. Later GET and wait requests use that saved reference, validate
run/agent/thread/exact input, and refuse conflicting reference or missing retention.
A missing reference from an incompatible server fails observation instead of
reinvoking an attempted operation. This does not sandbox the surrounding agent.

## Verification

Provider-free recorder tests exercise the real MCP HTTP session handshake,
filtered discovery, hidden-tool rejection, permission ordering and session
attenuation. Auth tests cover the exact route surface and separate grants.
SQLite tests cover stored-agent/tenant isolation, concurrent retained observers,
reference mismatch for broad callers, deletion/recreation and completion's
transactional incarnation precondition. The existing installed Trinity worker
qualification now configures the narrow grants; its native upstream runtime
qualification remains a separate boundary.
