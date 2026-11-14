# Tenant-scoped review and investigation

Status: v0.7.0 candidate source (not published), built on the
[connector action evidence milestone](connector-action-evidence.md). Published
v0.6.1 binaries do not implement these permissions, routes or sessions. The
original permissions/session slice adds no SQL migration or identity directory. Current
source also includes [target-bound approvals](target-bound-approvals.md), whose
migration and maintenance-window requirements apply when upgrading from a release.

## The caller we could not serve

A tenant's security reviewer needs to inspect an operation and approve or deny
it. Global Admin already does this, but also reads every tenant and manages
policy, grants, break-glass and conversations. An editable tenant filter does
not reduce that authority. The first evidence milestone deliberately kept Admin
global. This change adds a smaller authenticated-tenant workspace.

We reuse primary API-key/JWT/webhook authentication, existing SQL approvals,
metadata action records, session storage, CSRF and UI components. We do not turn
review into a second policy engine or add user-management tables.

## Separate surfaces make the boundary explicit

Global `/admin-api` keeps its deployment-wide Admin gate. Review uses a separate
`/review-api` route allowlist and `/admin/review/` UI. The static shell is public;
all operation data remains behind the API. `governance:read` reads own-tenant
approval metadata and connector evidence. `governance:approve` includes that
read access, the existing redacted argument display and approve/deny actions.
Neither grants client `read`/`write`, agent dispatch, conversation access, policy
editing, grants, break-glass or access to another tenant.

Review requires a nonempty verified identity and an explicitly configured,
nonempty tenant. Use `tenant_id: "default"` deliberately for a single-tenant
reviewer. Empty permissions never grant review, even with compatibility
`strict_permissions: false`. Compatibility identities used when a JWT subject or
webhook user identity is absent cannot become reviewers. A real explicitly supplied identity remains
valid even when its literal text matches a compatibility display name.
Only the primary provider authenticates Review. Separate `auth.admin_keys`
continue to work for Admin and do not become reviewer credentials.

A primary credential with explicit `admin`, identity and tenant can enter its
own tenant's Review workspace. Its Review cookie is attenuated to review-only
permissions. It does not retain global Admin permission or forwarded claims.

## Authenticate scope, then enforce it again

`internal/auth/review.go` recognizes the new permission vocabulary and authorizes
only the registered review methods/paths. Unknown routes and read-only mutations
fail closed. `tenant.ScopedContext` explicitly clears an inherited system marker
and applies the verified tenant. No header or query parameter selects authority.

`internal/api/review.go` repeats capability and scope validation before store
access. Foreign IDs return the same 404 as missing IDs. Foreign tenant selection
is refused; a cursor remains an ordering position, never a tenant capability.
Bounded lists use a lookahead row to emit a next cursor only when another page
exists. New readers are metadata-only; they do not provide a conversation route.

SQL pending getters, approval updates and status updates now apply context tenant
predicates on all three SQL backends unless a trusted caller explicitly uses
`SystemContext`. Existing Admin handlers supply that global context. Review
never does. This repairs the previously global ID lookup/update primitives
before using them for a scoped caller, rather than relying on hidden buttons.
Existing internal tests/jobs must use the real tenant or a justified system
context; there is no fallback when a scoped lookup fails.

## Share approval rules, not administrative authority

Admin and Review share the existing approval/denial business core. Review does
not call a handler that changes its request to system context. The same current
policy, hard-deny, required-audit, argument-digest, generation, run-incarnation,
expiry and one-shot consumption rules continue to apply. Managed rows also bind
the configured target revision. Review exposes that revision and refuses an old
or changed target, using the same business core as global Admin. Recording approval
does not execute a tool, resume a framework or make an uncertain effect safe to
retry. Standalone expiring decisions remain app-owned and create no managed run.

Review pending responses use an explicit field allowlist. Read-only responses
omit arguments; approvers receive only the existing capped, redacted display.
Free-form policy reason text is excluded from both projections. Fixed reason
codes and rule IDs remain available. Argument display is not the exact original
policy object and named-field redaction cannot find every sensitive value in
arbitrary text. Connector action/events use the existing typed metadata model,
with no result bodies or credential values. Run and thread IDs are plain text
in Review, not links that imply permission to read conversations.

## Browser credentials are a separate authority

Admin and Review use separate cookie names, paths and storage namespaces.
Review's `runkite_review_session` is scoped to `/review-api`, and Redis uses
`rk:review:sess:` instead of `rk:admin:sess:`. Memory stores are separate too.
A server-assigned purpose marker is checked on every request. Admin-cookie
requests also recheck Admin permission; moving a Review ID into an Admin cookie
cannot elevate it. Review rejects an Admin-purpose session presented as Review.
The separate Redis namespace also prevents an old Admin replica from looking up
a new Review ID through its old, less restrictive cookie path.

Builtin stores implement optional `ReviewSessionCreator.CreateReview`, which
mints attenuated review-only authority and an absolute lifetime of at most
15 minutes. Activity never extends that limit. Redis enforces that expiry cap
atomically in Lua; memory follows the same rule. The old Admin session lifetime
and legacy Admin blob format remain compatible. Unsupported custom stores fail
closed for review login instead of placing scoped records in the Admin namespace.

Mutations under cookie authentication require the Review CSRF token. Login never
stores the pasted credential in browser storage. Forwarded `Extra` claims do not
enter Review session records. Logout only confirms success after store deletion;
a storage failure remains unconfirmed. Mixed bearer plus browser-cookie logout
is rejected. Bearer-only logout cannot revoke an API key or JWT.

Review responses use `Cache-Control: no-store`. The separate frontend client
holds only session status/CSRF in memory, bounds requests, aborts obsolete reads
and discards data on expiry or scope changes. Fixed-expiry sessions capture
permission grants at login; changing IdP grants is not instant revocation of an
already-issued cookie. Logout or absolute expiry ends that session. Independent
Admin and Review sessions have independent logout.

## Rollout without accidental widening

Upgrade every serving replica before issuing the new role strings. Older JWT
permission filtering does not understand them, and compatibility handling of
unknown/empty permission lists must not become an authorization strategy. Use
`strict_permissions: true`, configure the actual identity/tenant mapping, then
issue least-privilege reviewer credentials. Do not mix old and new replicas while
relying on the new boundary. Keep a known working global Admin credential for
bootstrap and recovery; reviewers cannot grant themselves that role.

The Review permission slice adds no SQL schema. Upgrading current source from
v0.6.1 also applies Postgres/SQLite 18 or MySQL 19 for target-bound approvals,
after the earlier evidence/run-incarnation migration. Follow the
[target-binding upgrade](../ops-runbook.md#connector-target-revision-upgrade): stop all
writers, take a verified backup, migrate and replace every replica before traffic.
Historical managed approvals missing a target or run binding require fresh review;
neither field is inferred from whichever configuration/run exists at upgrade time.

## Verification and limits

Auth tests cover bearer/cookie access, explicit identity/tenant, JWT role parsing,
missing subject, route denial, default-open rejection, purpose/cookie swaps,
CSRF, logout failure and absolute expiry. Redis tests cover isolated namespaces,
legacy Admin blobs and expiry despite continued activity. API tests use two
tenants and both roles, guessed IDs, forged scope, paging, inherited system
context, filtered payloads, required audit and existing approval fences. Shared
SQL pending conformance verifies scoped get/approve/deny on all three backends.
Frontend tests cover session validation, expiry, request bounds, stale-data
suppression, fixed API base and role-limited mutations. Browser qualification uses
synthetic evidence and credentials, with no live model/provider data.

### Qualify the assembled multi-replica path

The existing `scripts/smoke-multi.sh` and `bench/soak` cover multi-process managed
execution and recovery. Their Compose profile deliberately has no client auth.
The new permissions needed a separate test of authenticated `serve` processes,
shared Redis sessions and atomic SQL authority across process boundaries.

`make test-review-ha` runs `test/e2e/reviewha` through
`scripts/test-review-ha.sh`. It creates a uniquely named disposable Postgres/Redis
pair, starts two actual binaries and uses a deterministic MCP fixture. The
fixture seeds an active run/attempt/queue assignment; it does not run an SDK,
model or managed admission. Production middleware, configuration, session
storage, proxy dispatch and the real action reconciler remain on the path.

The checks cover cookies and connector capabilities crossing replicas,
read/approve projections, tenant isolation, CSRF and Admin separation. Competing
approval and approved-dispatch requests must have one winner. Both replicas
must expose the same durable metadata without fixture credential, argument,
RPC-ID or response-body sentinels. After a target accepts an operation, the test
kills one process and waits for the surviving process's real reconciler to mark
the unconfirmed intent unknown without replay. Restart preserves evidence and
the Review session. Logout through the other peer invalidates that cookie.

Use `RUNKITE_TEST_REVIEW_REPETITIONS=3 make test-review-ha` for bounded repetition
(1 through 20). Each repetition includes a kill/reconcile/restart cycle, normally
about one minute. The wrapper prints an output directory containing
`review-ha-report.json`, sanitized process logs and fixture startup/cleanup logs.
The `review-ha` CI job runs one cycle independently of the SDK matrix. Report or
log persistence failures fail the test.

The wrapper never consumes inherited database URLs or a repository `.env` and
removes only its own Compose project. Each invocation generates a fresh database
password and retains its configuration reference rather than its value.
Direct test invocation refuses non-loopback
database URLs, a database without the dedicated prefix, a pre-existing public
schema or populated Redis. It never truncates or flushes shared infrastructure.
Only database container mappings are loopback-bound. The current CLI uses
`:port` HTTP/gRPC listeners, so run this test on a firewall-protected workstation
or isolated CI worker. Random per-invocation fixture keys are not production
credentials, and this harness is not itself a sandbox.

This closes a reproducible two-process qualification gap. It does not establish
database/Redis failover, long-duration or cloud soak results, live-provider
compatibility, throughput capacity, or safety for hostile in-process agent code.

This is not general per-resource RBAC or a per-user identity directory. Review
remains flat-tenant scoped. Current optional controls also provide
[requester separation and reviewer quorum](approval-authority-and-quorum.md),
[configured policy/optional schema binding](target-bound-approvals.md),
[durable evidence export](action-delivery-outbox.md), and
[registered Vault database leases](credential-lifecycle.md). Review can inspect
owned lease metadata but cannot renew/revoke or inspect credential values. None
of these controls proves remote implementation identity, instant external
revocation, or exactly-once business effects.
