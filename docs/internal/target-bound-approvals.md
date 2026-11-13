# Target-bound managed connector approvals

Status: unreleased source after v0.6.1. This extends the existing managed MCP
approval contract on all three SQL backends. It applies with action capture on
or off. It does not change standalone expiring `/governance/decide` approvals or
add a framework runtime. See the [operator procedure](../ops-runbook.md#connector-target-revision-upgrade)
and [connector configuration](../connectors.md#target-bound-managed-approvals-unreleased).

## The operation can stay identical while its destination changes

A reviewer approves `files.write` for one active run and exact arguments. The
connector name, tenant, agent, generation and run incarnation are all unchanged.
A deployment then maps `files` to another endpoint or auth principal. Without a
target binding, a later request can present the old approval to a different
configured destination.

We already had the difficult adjacent boundaries: exact argument digests,
immutable run incarnation, generation fencing, current policy re-evaluation,
required audit and atomic one-shot consumption. Those remain. We add configured
target identity to the same capability and transaction rather than introducing
a second approval service or policy engine.

## Define configuration identity without retaining credentials

`internal/connector/target_revision.go:TargetRevisionForConfig` serializes a
versioned descriptor and computes SHA-256. The stored reference is
`target-v1-` followed by 64 lowercase hex digits. The field layout is part of
that version; changing its meaning needs a new version rather than
reinterpretation of earlier approvals.

The descriptor contains the exact MCP URL, effective transport, optional manual
`mcp.revision`, auth type, issuer/token endpoint, client ID, secret-reference
locator, scopes and tool allow/deny sets. Scope and filter sets are sorted and
deduplicated. An omitted transport equals `json`; otherwise endpoint spelling and
locator identity are deliberately significant. Error mappings and breaker
thresholds are operational settings outside the descriptor.

API keys, bearer tokens, client secrets and values resolved through `secret_ref`
are excluded. A value can rotate behind the same reference without requesting
another approval. Changing the reference or configured principal/scope changes
target identity even if the resolved value happens to match. Existing OAuth
cache, expiry and provider-revocation behavior is unchanged; this is not a lease
or revocation subsystem.

All descriptor fields must be nonsecret configuration. MCP and nonempty OAuth
issuer/token endpoint URLs reject userinfo, unsupported schemes and recognized
credential query keys. UTF-8 and manual-label bounds also fail closed. These
checks cannot identify every possible secret hidden in arbitrary configuration;
operators must keep credentials in auth fields or the external secret system.
A hash is not encryption, endpoint authentication or a promise of metadata
anonymity. The pending/action record stores the reference, not the descriptor or
resolved credentials. Protect evidence access and backups as before.

## Snapshot before the asynchronous gates

`Registry.BindMCPRequestTarget` takes a request-scoped copy of the target
configuration before managed tool policy or approval lookup. The copy includes
endpoint/transport, auth configuration, scopes and tool filters. The context
carries both that prepared target and its expected revision.

`Registry.ProxyMCPRequest` uses the bound registry/connector snapshot through
credential preparation and dispatch. It cannot resolve the same logical name
again onto a replacement registry after a slow policy or network step. Secrets
referenced by the pinned configuration still resolve using the existing
session-time logic; their values are not frozen into the revision. The existing
OAuth cache remains attached to that connector instance.

Configuration is deployment-owned. Build registries from configuration at
startup; concurrent mutation of exported Go config fields is not a supported
hot-reload API. Request snapshot tests use sequential replacement to verify the
boundary, not to promise safe arbitrary concurrent writes.

Both HTTP managed paths bind a target. The evidence path creates its immutable
`ConnectorAction.TargetRevision`; the unrecorded path uses the same target when
consuming an approved capability. Direct Go proxy users without a managed
binding retain their existing non-managed contract. They do not acquire managed
approval guarantees merely by calling a transport helper.

## Extend the existing SQL authority tuple

`PendingAction.TargetRevision` joins tenant, agent, run, generation, run
incarnation, connector, tool and exact argument digest. New managed rows require
a complete valid target revision. Their deduplication key has a structured,
versioned suffix so it cannot alias the older incarnation-only encoding.
Standalone expiring rows keep their historical key format and an empty target.

`approvePendingAction` is shared by Admin and tenant Review. It checks the current
registry target before policy re-evaluation and again after that potentially
slow step. A missing, historical or changed target returns `409` instead of
minting authority. Current explicit policy deny and required-audit failure still
refuse approval.

For recorded requests, `sqlcompletion.peekActionApproval` requires exact target
equality. `PrepareConnectorActionDispatch` repeats that match under the existing
run/action/approval locks, then commits approval consumption, dispatch intent and
timeline event together. A confirmed rollback leaves the approval unconsumed.
The callback sends no protected request without newly applied dispatch authority.
The legacy `ConsumeApprovedPendingAction` path has the same revision predicate,
so disabling capture does not disable target-bound approvals.

SQL equality is not a transaction with the external service or every replica's
configuration. Each request uses its own pinned descriptor. Consistent deployment
configuration and the maintenance-window upgrade remain required. This feature
does not globally retire an old endpoint still configured on an older replica.

A revision mismatch cannot consume the earlier approval. If current policy still
requires review, the caller obtains a new pending row and fresh review for the
new target. A current ordinary policy allow is still its own authority; an old
approval is not needed to bypass a hold that no longer exists.

## Do not claim to pin remote implementation

`mcp.revision` is an optional nonsecret operator contract label, at most 128 UTF-8
bytes without control characters. Advancing it changes the descriptor when the
endpoint stays constant. Coordinate that label with the remote deployment; the
plane does not verify a signed release, image digest or actual implementation.

JSON mode forwards the protected POST without requiring discovery. Negotiated
mode calls `findUpstreamTool` before execution to populate the SDK's tool cache
and derive protocol headers. That lookup checks the advertised name. It does
not compare stored schemas or prove that the advertised implementation is the
one executing. A schema digest alone would not establish that proof either.
Unchanged URL/label configuration cannot detect an upstream behavior change.
`ToolRevision`, policy revision and credential lifecycle remain separate work.

A human approval still does not execute a tool or resume a framework. A timeout,
disconnect or missing evidence response after dispatch remains uncertain. Never
replay an external write merely because an earlier approval is now stale.
Original dispatch receipts can still append late observations to their original
action after cancellation or reclaim; the new target rule does not rewrite that
history or authorize another effect.

## Upgrade historical authority explicitly

Postgres/SQLite migration 18 and MySQL migration 19 add `target_revision` with an
empty default. No target is inferred from the configuration present during
upgrade. Historical managed pending/approved rows remain inspectable and can be
denied, but cannot gain fresh approval or authorize a new call. Do not repair
that authority by populating the column manually. Submit a fresh operation only
after establishing that another external attempt is safe.

These migrations refuse downgrade. Stop/drain every writer, take a verified
backup, run upgrade using the reviewed new binary, replace every replica and
qualify the result before traffic. Rollback requires a reviewed backup and
explicit reconciliation of later records/external effects. Current migration
startup rejects a schema newer than it supports. Released older binaries did
not have that guard and may ignore the new column, so it cannot justify a
mixed-version rollout.

Custom Go pending stores add `targetRevision` after `argsDigest` in
`ConsumeApprovedAction`, and before `embedded` in `FindOpenPendingAction` where
provided. New managed pending/action creation must supply the trusted revision;
standalone expiring rows remain empty. There is no new runner/client header or
public Runner Protocol field. Never substitute an untrusted caller's revision
when the registry cannot resolve the target.

## Regression evidence

- `internal/models/pending_target_test.go` covers revision syntax and namespace
  separation, including the unchanged standalone key contract.
- `internal/connector/target_revision_test.go` checks descriptor changes,
  credential rotation, credential-bearing URL refusal and pinned request routing.
- `internal/state/conformance/pending_target.go` runs on all three SQL backends:
  distinct targets with identical arguments do not merge, mismatches do not burn
  approval, recorded capture and legacy consumption share the fence, and
  concurrent consumption has one winner. Invalid/historical rows cannot acquire
  new target authority.
- Each SQL backend's `target_revision_migration_test.go` checks preserved
  historical fields without a guessed target and refusal of unsafe downgrade.
  Historical migration/checkpoint tests build their original schema explicitly.
- `internal/api/target_revision_test.go` exercises fresh review after a target
  change, differing registries over shared storage, capture on/off, a target
  change during approval, and request routing through a delayed policy decision.
- `test/e2e/reviewha` starts two actual control-plane processes against shared
  Postgres/Redis. Its final target-drift case restarts one peer with a different
  endpoint, verifies Admin and Review rejection there, preserves the original
  approval and dispatches the changed target once after fresh review. Existing
  iterations also assert exact pending/action revisions to reject stale test
  binaries. Run `make test-review-ha`; repetitions are bounded by
  `RUNKITE_TEST_REVIEW_REPETITIONS`.
- Admin/Review tests cover malformed and absent bindings, standalone decisions
  and non-retrying server conflicts. The built UI retains historical rows with
  a fresh-review explanation, disabled Approve and available Deny on pending rows.

These are configured-target and approval tests. They do not establish remote
implementation attestation, schema compatibility, sandbox containment,
exactly-once effects or live-provider behavior.
