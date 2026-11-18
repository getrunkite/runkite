# 3. Authority, credentials and evidence

[Notebook home](README.md) · Previous: [recovery and original identity](02-recovery-and-original-identity.md) · Next: [integrations and operator truth](04-integrations-discovery-and-operator-truth.md)

A useful starting question is: what fact authorizes the next effect? An agent
name, an authenticated connection, a policy allow, a human vote, and a successful
HTTP response answer different questions. Treating them as interchangeable creates
authority that none of them actually granted.

This chapter describes the v0.10.0 implementation. Earlier notes record
milestones: [approval authority](../approval-authority-and-quorum.md) describes
v0.7.0, and [delegation and correlation](../delegation-and-audit-correlation.md)
describes v0.8.0. Those are evidence of the recorded design history. The
explanation below connects the current boundaries retrospectively; it does not
reconstruct undocumented author intent. Compared with an early architecture
walkthrough, the important change is the precision of the authority carried
through retries, review, credential issuance, and uncertain outcomes.

## Identity has several jobs

The **caller** is the authenticated person or application asking for work. The
**agent** is the configured execution definition. The **workload** is the worker
process holding an assignment. A fleet credential authenticates a kind or pool of
workers; it does not identify an individual process, prove which person requested
a run, or authorize every agent that process can name.

For managed execution, the server binds authority to `(run_id, run_incarnation_id,
generation)`. The public run ID is a lookup address. The persisted incarnation
identifies its original physical record. Generation identifies an attempt within
that record. Reclaim changes generation; deleting and recreating the same public
ID changes incarnation. Neither a timestamp nor a public ID is sufficient to
distinguish all these cases.

[`bindInternalRun`](../../../internal/auth/runbind.go) requires the
original-record proof and exact generation, checks the live assignment, applies
kind and pool scope, and derives the tenant from that assignment. A
runner-provided tenant header cannot substitute a different tenant on those bound
paths. Missing proof, including a historical unbound assignment, is refused.
Generation zero is an exact value rather than a wildcard. The incarnation is still
not an independent bearer credential: authentication, assignment ownership, scope,
and liveness checks remain necessary.

Optional workload mTLS adds another fact.
[`AuthenticateWorkloadTLS`](../../../internal/auth/workload.go) uses the verified
TLS chain and an allowed URI identity, while ownership also binds the public-key
fingerprint. A different key with the same URI cannot borrow an assignment.
Forwarded certificate headers and a certificate merely present on the request are
insufficient. [`RequireWorkloadAuthority` and
`RequireWorkloadOwner`](../../../internal/auth/workload.go) also recheck
deployment policy and expiry. This limits what a shared fleet token alone can
prove without claiming hardware attestation or protection from a compromised
trusted worker.

The regressions make these distinctions concrete:
[`TestMiddleware_RunBindingRequiresExactOriginalRecord`](../../../internal/auth/runbind_test.go)
covers missing, duplicate, superseded, and exact proofs;
`TestMiddleware_RunBindingGenerationZeroIsNotWildcard` covers attempt identity.
Operationally, planes and workers must agree on this protocol. An old worker
cannot be made safe by accepting an empty proof, and replacing a run row is not a
way to continue an old assignment.

## Delegation must narrow a particular caller's authority

A user claim is not automatically a downstream credential. In the constrained
exchange profile, strict JWT identity and explicit token forwarding are both
required. The assertion comes from the authenticating bearer token, through a
typed private field.
[`bindConnectorDelegation`](../../../internal/api/connector_delegation.go) selects
it from the bound assignment rather than accepting a worker's replacement token, a
tool argument, or a convenient `Extra` claim.

The connector fixes audiences, resources, scopes, maximum TTL, and any
actor-secret reference. Exchange-response validation refuses widening and
ambiguous protected fields. This is attenuation relative to the registered
exchange contract. The issuer and resource server still enforce the token's actual
authority. An issuer-supplied actor chain records an assertion, not possession of
every actor's key. Explicit forwarding also means the trusted worker receives the
authenticating token; calling the design server-only would conceal that exposure.
These limits are documented in [delegation and audit
correlation](../delegation-and-audit-correlation.md) and exercised by
[`TestConstrainedExchangeSubjectCannotBeSubstitutedBeforeNetwork`](../../../internal/connector/token_exchange_response_test.go).

Exact identity matters inside policy too. Trimming an agent ID, lowercasing a
reviewer, or joining arbitrary fields with a delimiter can alias two distinct
authorities. Positive grants and cache keys therefore preserve exact scopes and
tuples. Restrictive matching retains conservative compatibility behavior where
normalization prevents a padded restriction from becoming ineffective. This is a
deliberate asymmetry, not a universal rule that every string receives identical
treatment. [`TestPositiveGrantScopesAreExact`,
`TestRestrictivePaddedPredicateAndReviewCoverageRemains`, and
`TestDecisionCacheExactTupleDoesNotAliasSeparators`](../../../internal/policy/exact_identity_test.go)
are useful counterexamples to an overly simple normalization rule.

Argument identity needs similar care. A rounded large number can change what a
reviewer approved. Strict binding preserves numeric distinctions and rejects
ambiguous JSON rather than constructing a convenient approximate digest.
[`TestBindArgsDistinctWideNumbers`](../../../internal/policy/numeric_security_test.go)
captures that consequence. Friendly display formatting must not become
authorization identity.

## An approval is a constrained capability

A managed approval binds tenant, agent, public run ID, incarnation, generation,
connector, tool, argument digest, configured target revision, policy revision, and
an optional observed tool-schema revision. Empty schema revision means exactly
that no schema contract was bound. It never means any future schema is acceptable.
A target or schema digest identifies a contract observation; neither attests a
remote implementation or its business behavior.

[`approvePendingAction` and
`checkPendingRevisions`](../../../internal/api/admin_pending.go) recheck policy,
approval requirements, target, and optional tool contract before recording
approval. Review cannot turn a continuing policy denial into permission. Changing
a target or policy requires fresh review rather than silently expanding an old
vote. Consumption later matches the same authority tuple and remains one-shot.

Requester identity and the approval plan are immutable. For managed work,
requester identity comes from the durable run manifest, not submitted tool
arguments. Requester exclusion requires captured verified identity. A legacy
display name cannot be repaired into verified authority by editing metadata.
Standalone governance is different: it has no managed Thread or Run, uses
authenticated API identity, and leaves continuation to the application.

Quorum counts distinct exact reviewer identities, not repeated requests.
[`ApprovePendingAction`](../../../internal/state/sqlcompletion/pending_review.go)
locks current managed authority and the pending operation, inserts a unique vote,
and changes status only after the retained count reaches the immutable threshold.
A partial quorum grants no dispatch capability. Reviewer authorization is also
tenant-scoped; knowing an approval ID is insufficient. The [review-scope
note](../tenant-scoped-review.md) explains the authenticated scopes around these
decisions. Two accounts may still belong to one human. Stable identity-provider
subjects and account governance are prerequisites for a meaningful
separation-of-duties policy.

This rejects the simpler “approved run ID plus matching arguments” design. That
design cannot distinguish replacement records, reclaimed attempts, changed
destinations, or changed review requirements. The cost is explicit operational
friction: legacy approvals need fresh submission, identity-provider configuration
matters, and writers must be drained and upgraded together when authority-bearing
storage contracts change.

## Evidence stops at the boundary it observed

A decision audit says which decision the plane made. With required audit enabled,
an unavailable durable write prevents allow or pending authority from proceeding.
Default best-effort audit preserves a different availability tradeoff.
[`TestRequiredAudit_UnavailableBlocksEveryStage` and
`TestRequiredAudit_PendingCannotSurviveFailedWrite`](../../../internal/policy/audit_required_test.go)
document the stronger mode. An audit write that failed cannot supply its own
retained row explaining that failure, so operational logs and traces still matter.

Managed action evidence extends this without pretending to transact with the
external world. As [connector action evidence](../connector-action-evidence.md)
explains, SQL commits dispatch intent and approval consumption together
immediately before the protected tool call. A rollback sends no protected call. A
lost commit acknowledgement can leave committed intent even though the callback
refuses to send. Initialization, discovery, and credential preparation can already
have contacted the upstream before this protected-call boundary.

The SQL transaction ends before the external tool executes. If a write reaches the
tool and its reply is lost, the correct observation can be unknown. Replaying the
tool because evidence persistence failed risks duplicating the business effect. An
action UUID identifies an attempt, not a downstream idempotency contract. The
receiver or business system must provide that stronger contract when required.

The [delivery outbox](../action-delivery-outbox.md) solves a different problem.
Evidence transitions and delivery records commit together, then a background
sender delivers immutable events. A lost collector acknowledgement causes the same
delivery ID to be resent; the collector deduplicates it. It does not cause the
business tool to run again. Synchronous delivery inside the SQL transaction was
rejected because the collector could accept an event before SQL rolled back.
Best-effort post-commit delivery alone loses events after a crash. The outbox
trades those failures for retained queues, retries, receiver deduplication, and an
operator capacity obligation. It provides neither exactly-once network delivery
nor proof of external effect.

## Credentials have a separate lifecycle

Ordinary `vault:` secret resolution reads KV material. Dynamic database
credentials use a separately registered lease binding and a credential-aware
trusted consumer. [`Manager.Execute`](../../../internal/credentials/manager.go)
records issuance intent before contacting the provider and refuses automatic
reissuance after an uncertain outcome. The distinction matters because asking
again can create a second live credential while losing the first cleanup handle.

The database stores metadata and an encrypted protected handle, not the issued
username and password. [`protect.go`](../../../internal/credentials/protect.go)
binds encryption to owner, record, and binding identity. Cleanup keys and
historical provider configuration must remain available outside that database.
Losing a key does not prove revocation. The [credential lifecycle
note](../credential-lifecycle.md) explains the resulting backup, rotation, and
reconciliation duties.

Renewal requires current exact ownership. Cleanup can intentionally revoke a known
old owner's handle after reclaim, without gaining access to a replacement owner's
credential. A callback deadline reflects the actual granted TTL; a later manual
renewal does not extend an already supplied deadline. Trusted consumers must honor
cancellation and expiry. This is not automatic interruption of arbitrary code, and
secrets briefly exist in trusted process memory.

[`TestUnknownIssuanceSurvivesRestartWithoutRetry`,
`TestReclaimBetweenIssuanceAndUseCannotReleaseCredential`, and
`TestProtectedHandleCannotMoveAcrossOwnerAndTenant`](../../../internal/credentials/manager_test.go)
expose the essential failures.
[`TestVaultLookupNeverTreatsArbitrary404OrErrorsAsRevoked`](../../../internal/credentials/vault_test.go)
prevents an outage from masquerading as cleanup. If issuance succeeded but no
handle was observed, local bookkeeping cannot discover the credential by optimism.
Provider audit, the registered role's own TTL ceiling, and explicit reconciliation
remain necessary.

## Walking through an uncertain operation

Consider a hypothetical invoice-refund agent. Alice requests a refund, and a
worker receives record R, incarnation I, generation 7. The connector requires two
reviewers and excludes Alice. The pending operation captures Alice from the
durable manifest and binds the exact refund arguments and current destination.
Repeating one review request cannot supply two votes.

Before dispatch, the connector target changes. The old approval cannot follow the
new destination; the operator resubmits for fresh review. If generation 8 later
owns the assignment, generation 7 cannot recover authority by presenting the same
public run ID. Its original proof is necessary but no longer sufficient.

After valid review, suppose SQL commits intent and the external refund accepts the
request, but the reply disappears. The retained evidence must permit an unknown
outcome. An outbox retry can repair delivery of that observation, not safely
refund again. An operator needs the downstream system's reconciliation or
idempotency contract before creating another attempt. If a dynamic credential's
revoke also times out, that uncertainty remains separately visible. One green
policy decision cannot collapse all three outcomes into success.

These are layered controls. Cooperative SDK tool hooks govern supported invocation
paths; arbitrary in-process HTTP or SQL can bypass them. Execution pools route
authorized work to separately credentialed fleets, but workers within a fleet
remain trusted. The [external workload boundary](../constrained-execution.md)
separates generated code from worker credentials. Its OpenShell profile remains
experimental; the [effective-policy qualifier](../openshell-effective-policy.md)
observes loaded policy around fixture execution, not continuously enforced kernel
state or an atomic check-to-exec guarantee.

The architectural consequence is to reuse the existing policy decision point,
Vault issuer, and external isolation runtime rather than build another vault or
policy engine. That is a retrospective interpretation of the boundaries above,
supported by the design notes' explicit reuse choices. Stronger requirements would
change the integration contract: hostile code needs independently qualified
isolation; distinct-human quorum needs identity governance; exactly-once refunds
need business-system support. Adding another local allow boolean cannot supply
those guarantees.

## Study questions

1. Why can neither a fleet token nor an incarnation alone authorize checkpoint access? Trace [`bindInternalRun`](../../../internal/auth/runbind.go) and its exact-record regression.
2. Which changes invalidate a human approval, and why is an empty schema revision not a wildcard? Follow [`checkPendingRevisions`](../../../internal/api/admin_pending.go) and [approval authority](../approval-authority-and-quorum.md).
3. After a lost response, which retry repairs evidence delivery and which might duplicate a refund? Compare [action evidence](../connector-action-evidence.md) with the [outbox](../action-delivery-outbox.md).
4. Why can a known old lease be cleaned up while unknown issuance remains unresolved? Read [`Manager.Reconcile`](../../../internal/credentials/manager.go) alongside `TestUnknownIssuanceSurvivesRestartWithoutRetry`.
