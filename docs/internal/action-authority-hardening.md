# Parent authority and secret-safe policy decisions

This note records the first security-roadmap implementation. It strengthens
existing execution and connector paths. A downstream action ledger, Vault
dynamic leases, isolated execution pools and provider-confirmed revocation
remain separate future work.

## Why these changes come first

Runkite already has assignment binding, exact argument digests, one-shot managed
approvals, server-side MCP credential injection, external secret references and
SQL policy audit. These controls need consistent behavior at their boundaries:

- A child run must inherit authority from its authenticated parent, rather than
  a body-selected identity.
- When durable audit is required, an audit failure must stop exceptional access
  as well as ordinary access.
- Provider diagnostics and display metadata must not accidentally carry
  credentials into operational output.

The implementation reuses those controls. It adds no new database schema,
identity provider, vault or external policy engine.

## Parent-derived delegation

`internal/auth/runbind.go` now includes `/internal/a2a/runs` in run-bound routes.
The middleware validates the parent run ID, actual generation, runner kind and
tenant allowlist. Duplicate generation headers are rejected. The handler in
`internal/api/a2a.go` requires that the body parent matches the binding and
resolves the parent within the assignment's tenant.

The parent assignment supplies identity and claims. A frozen manifest principal,
when present, must agree. Omitted `on_behalf_of` inherits that authority. A body
may request narrower explicit permission strings; it cannot replace the identity,
change trusted claims or remove existing restrictions with an empty list.
Null permission lists are rejected. An unchanged unrestricted parent can retain
its existing unrestricted semantics.

This deliberately uses literal permission containment. It does not add a second
permission language or infer that a wildcard grants an unrelated permission.
Existing admission authorization still decides whether the child agent can run.
Trusted Extra claims remain unchanged; narrowing Runkite permissions does not
downscope an external bearer token that an operator forwards in those claims.

The Python and TypeScript A2A helpers carry the current parent identity and real
generation. A hand-built configuration without a usable generation, or a
configuration that disagrees with the active worker context, fails before HTTP.
Zero is valid only for a genuine generation-zero assignment.

Queue checks and durable child creation remain separate operations. The
implementation rechecks parent liveness around slow admission work and before
dispatch, but this is not an atomic parent-attempt/child transaction or a new
per-worker isolation guarantee. Same-kind fleets retain their documented trust
boundary. See [A2A](../a2a.md).

## Required audit is an availability choice

`policy.audit_required: true` is opt-in. `audit` defaults to true, and an explicit
false value is incompatible. `cmd/policy.go` requires a supported durable SQL
auditor; `cmd/serve.go` refuses startup on an invalid configuration.

`Engine.Decide` writes every decision, including cache hits. It keeps cached
policy evaluation separate from persistence failure. If a required write fails,
an allow or pending result becomes `policy_audit_failed`. An existing explicit
deny keeps its original reason and receives an internal `AuditFailed` marker.
Traces and SIEM see the final result and a separate failure marker.

`checkConnectorPolicy` prevents exceptional authorization after that failure.
Admission uses the same function. The MCP path refuses to consume an approved
capability during the outage. Break-glass continues to require its own durable
use audit and cannot bypass a failed required decision write.

An unavailable audit store cannot save its own failure row. Operators must use
database health, operational logs and configured trace/export evidence. This
option intentionally trades availability for durable authorization evidence;
best-effort audit remains the default. A decision record does not prove that the
tool executed or that its intended business effect happened.

## Keep diagnostic classifications, discard secret-bearing text

OAuth acquisition previously appended non-200 provider bodies to errors that
connector callers log. A provider could echo credentials in that content.
`OAuthEndpointError` now retains status and explicitly recognized standard OAuth
codes. It excludes descriptions, bodies, request IDs and arbitrary provider
codes. A character/length check alone would still permit a credential-shaped
value in an error field.

Shared connector transport sanitization removes endpoint URLs and arbitrary
transport/read/close error text on OAuth, Vault and MCP acquisition/forwarding
paths. Fixed error classifications preserve cancellation, deadlines and bounded
body handling without retaining the original secret-bearing error chain.
Recognized connection errors continue to support safe configured error mapping.
Tool JSON-RPC responses remain opaque upstream content, with existing size limits
and retry behavior. This is not content-wide redaction of arbitrary tool output.

The recursive argument-display redactor now recognizes common snake_case,
camelCase and header-style credential aliases. Policy predicates still receive
the original parsed arguments, and the v2 canonical digest is unchanged. Audit
and webhooks receive the bounded display copy. Naming rules cannot find secrets
embedded in ordinary text, so applications should avoid credential-bearing
arguments and unnecessary payload capture.

## External secrets and rotation remain explicit

Existing `env:`, `file:` and Vault KV references resolve through
`ResolveSecretRef`. A configured Vault token file is reread during resolution;
Agent injection was already supported and documented. This patch does not add
dynamic-secret issuance or revocation.

OAuth client-credential tokens remain cached until their existing expiry buffer.
Rotating an external client secret does not revoke an already-issued access
token. Resolution still happens before the cached-token lookup, so a failing
Vault lookup does not silently fall back to cached authority. Changing that
outage policy requires a separately configured and tested contract.

MCP credentials stay on the plane and are injected server-side. Non-MCP sessions
can vend credentials to trusted runners. Their advisory expiry is not upstream
secret revocation. Run cancellation cannot retract an already-dispatched effect.

## Upgrade and regression evidence

There is no schema migration or wire-protocol generation change. Custom A2A
callers must supply current parent binding and stop relying on body-selected
authority. Custom connector error mappings based on arbitrary provider body text
must switch to recognized safe codes. Existing standard OAuth and connection
classifications remain actionable.

Regression coverage includes:

- OAuth secret sentinels in text/JSON logs, bodies, URLs and transport errors;
  preserved cancellation, bounds and single-dispatch behavior.
- Nested secret aliases with unchanged original arguments and a pinned existing
  canonical digest.
- Required-audit outage/recovery, cached decisions, pending capability retention,
  break-glass and admission blocking, using a downstream effect counter.
- Foreign/stale parents, identity/permission/claim replacement, delayed requests,
  cancellation/reclaim during admission, valid narrowed delegation and SDK
  identity/header compatibility.

Validation for this patch passed the full Go internal/command suite with the Linux race detector, live Postgres/Redis state/transport/API/command checks, and repeated final API race suites after the child-prewarm correction. The TypeScript runner passed 279 unit tests; its real-plane AI SDK profile passed all 9 tests on SQLite and all 9 on Postgres/Redis. Admin passed 39 tests, the chat bridge passed 116 tests, and site interactions passed 43 tests. The Python runner target and four source-distribution/wheel checks passed. OpenAPI checks cover all 149 registered Go routes, and the site scan found no broken local links. Test fixtures use offline models and disposable local services.
Live-provider behavior and deployment-specific isolation require their own
qualification. No finite regression suite proves the absence of every defect.
