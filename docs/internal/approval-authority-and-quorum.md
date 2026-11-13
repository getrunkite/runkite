# Approval authority, separation, and quorum

This note describes unreleased storage changes after v0.6.1. It supplements
[target-bound approvals](target-bound-approvals.md) and
[tenant-scoped review](tenant-scoped-review.md).

## What an approval authorizes

A managed approval is authority for one exact operation. Its binding includes the
tenant, agent, run ID, durable run incarnation, generation, connector, tool,
argument digest, connector target revision, policy revision, and optional observed
tool-schema revision. A matching identifier or argument digest alone is not
sufficient. Both legacy capability consumption and recorded dispatch use that
same revision tuple. A missing managed policy revision requires a new submission
and fresh review.

`target-v1-<sha256>` preserves the existing connector descriptor. New optional
connector contracts use `target-v2-<sha256>` rather than changing the meaning of a
previously approved v1 digest. `policy-v1-<sha256>` identifies the policy snapshot.
`tool-v1-<sha256>` identifies a pinned tool-schema contract when configured. An
empty tool revision means no schema contract was bound; it is an exact empty
value, never a wildcard. A schema digest does not prove that a remote tool's
implementation or external business behavior stayed unchanged.

The requester and approval plan are immutable fields on the pending operation.
Managed requester identity comes from the durable run manifest's principal.
Submitted tool arguments cannot replace it. Standalone governance has no run,
so its requester must come from the API's authenticated identity. Standalone
rows retain their separate namespace and clear managed revision fields.

Requester exclusion also requires the durable manifest principal's
`verified_identity: true`, captured by the server from an explicit authenticated
subject. Missing proof on a legacy run requires a fresh authenticated run; no
metadata patch or tool argument supplies it. A2A inherits verification only from
the durable parent manifest, so forwarded claims cannot promote a compatibility
identity. JWT/webhook fallback display names are not verified requester or
strict reviewer identities, even when authentication granted ordinary access.

Internal session serialization retains identity provenance across replicas.
Legacy browser sessions remain usable for ordinary Admin operations, but cannot
cast a strict vote until the operator authenticates again with an explicit
subject. This records the provider's subject assertion at authentication time;
it is not an identity directory, real-world identity check or instant revocation.

## Counting distinct reviewers rather than requests

The default `required_approvals` is one. Configured values range from one to five.
`exclude_requester` prevents the exact requester identity from voting. Either
requester exclusion or a quorum above one requires an explicit verified
reviewer; exclusion also requires a known requester.

Each vote is a row in `pending_action_approvals`, unique by tenant, pending
operation, and exact reviewer identity. Comparisons do not lowercase, trim, or
otherwise normalize identities. Identity-provider configuration must give the
same person a stable identity if the operator expects those accounts to count as
one person. The control plane cannot recognize that two independent identities
belong to the same human.

A transaction locks the current managed run and then the pending operation,
inserts a vote if that reviewer has not already voted, counts retained votes,
and changes status to `approved` only when the quorum is reached. Partial votes
leave status `pending`, keep the deduplication key, and do not mint dispatch
authority. Concurrent retries from one reviewer do not count twice or replace
that reviewer's first `reviewed_at`. `decided_by` identifies the reviewer whose
vote reached quorum; detail responses include the retained history.

The default-one local mode continues accepting an empty actor where existing
local operation did so. That records an empty identity honestly. It does not
satisfy strict separation or quorum configuration. Applications should use
stable authenticated identities for meaningful human separation.

## Failures and concurrency

Vote insertion and status transition share one SQL transaction. A failed final
update rolls back the new vote. Both dispatch paths additionally require the
stored vote count to meet the immutable quorum. Merely changing an operation's
status to `approved` through a generic status update does not manufacture votes.

Managed approval uses the same run-before-pending lock order as dispatch.
MySQL uses read-committed transactions so a contender sees a vote committed while
it waited for that lock. SQLite serializes through its existing database
transaction implementation. The SQL detail getter locks the pending row while
reading history so its `approval_count` and `approvals` agree.

A policy, tool-contract, target, incarnation, generation, or argument mismatch
cannot consume a capability. No review count makes a denied operation dispatch
without the existing policy and authorization checks. Denial, consumption, and
ordinary run deletion retain review history. A SQL administrator remains inside
the trusted storage boundary.

## Upgrade and extension points

Postgres and SQLite migration 19, and MySQL migration 20, add immutable approval
fields and the vote-history table. They preserve historical data and do not
invent requester identities, revisions, or votes from `decided_by`. Historical
managed approvals therefore require fresh review. Downgrade refuses to discard
these authority checks; recovery requires a reviewed pre-upgrade backup.

Stop and drain all writers, verify a backup, migrate, and replace every replica
before accepting traffic. An already released old binary does not understand
these columns and cannot be made safe by the new binary's migration guard.

Custom SQL-capable stores extend `ConsumeApprovedAction` and
`FindOpenPendingAction` with `policyRevision` and `toolRevision` immediately after
`targetRevision`. `SetPendingActionApproved` keeps its signature but now records
one exact vote. `GetPendingAction` includes retained votes, and
`SearchPendingActions` includes a derived count without loading full history.
The shared core is `internal/state/sqlcompletion/pending.go` and
`pending_review.go`; all three SQL backends use it.

The shared conformance tests exercise partial quorum, duplicate and concurrent
reviewers, requester spoofing and self-review, case-sensitive identities,
revision mismatches in both dispatch paths, standalone behavior, tenant
isolation, and retained history. SQLite adds injected vote-commit failure and
historical migration regressions. These tests qualify the storage contract;
they do not establish a person's real-world identity or provide exactly-once
external tool effects.
