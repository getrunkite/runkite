# Original-record identity through the runner boundary

Status: Unreleased. This extends the checkpoint snapshot fence described in
[atomic results and identities](atomic-results-and-identities.md). The earlier
fence protected a request already in progress. This change also rejects a fresh
message sent by a worker that still holds a deleted record's assignment.

## The failure that generation alone cannot distinguish

Generation identifies an attempt within a stored run. It does not identify the
stored run itself. Consider an old assignment for public ID `task-1`, generation
1. Delete that record and create another `task-1`; its first generation is also
1. Looking up the current assignment and copying its identity onto an old
message lends the replacement's authority to the old worker.

The existing `state.RunIdentity` supplies the missing coordinate. SQL already
stores a non-reused `incarnation_id`; Mongo privately derives identity from its
nonzero ObjectID. The dispatch captures it in
`transport.RunAssignment.RunIncarnationID`. The wire proof reuses this identity
without a new identity database or secret vault. The finalization changes below
add SQL accounting columns through migrations 23 (Postgres/SQLite) and 24 (MySQL).

The authority tuple is `(run_id, run_incarnation_id, generation)`. A record
replacement changes its second coordinate; reclaim changes its third. None of
the consumers repairs missing proof by looking up a newer record.

## Public protocol and worker behavior

`proto/runner.proto` adds `assignment_identity_version` to `GetJobRequest` and
`WatchCancelsRequest`. Version 1 is required before dequeue or subscription.
This deliberately rejects older workers before they take a job they cannot
execute safely. The protobuf fields are additive; the required behavior is a
coordinated compatibility change.

`RunEventProto`, `HeartbeatRequest` and `ReportStatusRequest` echo
`run_incarnation_id`. HTTP helpers send `X-Runkite-Run-Incarnation` alongside the
public run ID and generation. The proof is exactly 1 to 256 printable ASCII
bytes, without whitespace normalization. The plane still derives tenant, agent
and thread from the matched assignment. A supplied proof is never permission to
choose a different tenant or agent.

Python `assignment_identity.py`, `worker.py` and `generic_worker.py`, and
TypeScript `worker.ts`, validate the assignment before preflight, framework
factory or model/tool execution. Both capture the value so a mutable adapter
assignment cannot redirect completion. `run_status.py` and `runStatus.ts` stop on
401 and 403 even for an ordinary runner. Matching terminal status remains
readable after lease removal, with the same frozen kind, pool and workload-owner
checks. A network failure retains the existing unpooled preflight behavior.

This value is a continuity proof, not a secret. It adds no workload attestation
and cannot make a compromised worker with current authority trustworthy.

## Enforce at each mutation

`internal/bridge/server.go` checks the echoed tuple. Its `GetJob` requires durable
lifecycle callbacks and calls `BeginRunAttempt` with the captured original
record. Stores compare that identity under their run lock before advancing the
attempt. A delayed ready assignment cannot advance a replacement row.

Queue `Ack`, `Renew` and `Nack` compare original identity and generation at the
mutation. Redis uses Lua, NATS uses revision CAS, and the in-process queue uses
its lock. Dequeue must not replace a different record's current in-flight slot.
Cancellation tombstones and poison records carry original identity too.

`internal/api/completion.go` supplies `ExpectedIncarnationID` on regular,
reclaimed, poisoned and delivered-event recovery completion. A no-lease retry
uses the store's `ReplayOnly` transaction. It can confirm an existing completion
marker, but cannot create a completion or trust an earlier read across record
replacement. A fresh checked read is also required before finalization.

That read does not protect later side effects by itself. A paused finalizer
previously removed a replacement record's budget hold through the bare public
ID. A deterministic SQLite regression reproduced the loss of its 17-token hold.
The optional `state.RunUsageEventStore`, `state.RunUsageHoldStore` and
`state.RunTerminalHookStore` now provide explicit fenced operations. Each locks
the run row and checks its captured identity in the same transaction as its
mutation. API callers never fall back to their older unbound methods. Configured reservations validate the fenced store capability before insertion. A reservation write failure closes only its original pending row and returns before assignment delivery, leaving ambiguous legacy accounting visible for reconciliation.

Usage events retain distinct records through
`(run_id, run_incarnation_id, source)` uniqueness. Holds store their original
identity too. Terminal hook claims use the existing private claim storage with
record-scoped keys. The process-local span and finished-run maps use that same
record coordinate, so old cleanup cannot end or suppress the replacement's
observability or hook. Active-run timeout projections also retain original
identity; a partial row projection must not discard authority.

Historical usage, holds and bare hook claims cannot prove their original record.
The migration leaves them unbound rather than guessing from a current row or
timestamp. A colliding bound mutation fails closed and requires explicit
reconciliation and fresh operation IDs. This avoids silently double-counting
historical usage or attaching it to a replacement. Back up the database, stop all
writers, upgrade its schema, and deploy matching binaries before traffic. These
are coordinated upgrades, not mixed-version rollouts or automatic downgrades.

Queue renewal and event publication are separate operations. Therefore event
brokers namespace **Publish, Subscribe, Replay and Close** by the original
record pair, and check the stamped event identity. Filtering JSON alone would
not stop an old `Close` from closing a new record's stream. Durable terminal and
values scans require both original identity and generation. An old event can
remain in its own namespace without becoming another record's output.

Cancellation is record-wide, so its signal carries the original record but no
attempt generation. Workers fan a genuine user cancel out to that record's
attempt flags. A superseded heartbeat changes only its own flag. Record-pair
keys and conditional cleanup keep old cancellations and cleanup timers away
from a replacement with the same public ID.

An exhausted assignment whose authoritative durable record is definitively gone
or has a different valid immutable identity can be disposed of without finalizing
the replacement. The reaper acknowledges only that retained old tuple. Lookup
errors and absent or malformed identity are not evidence of retirement and keep
the lease recoverable. This prevents a retired slot from blocking unrelated new
work indefinitely while preserving exact acknowledgement fencing.

## Receipt continuity follows the same rule

AI SDK and the managed Microsoft, OpenAI Agents, Pydantic AI and Google ADK
adapters persist original identity in their committed-turn receipts. Reclaim of
that same record replays the result without another model/tool cycle. A known
different original record is a fresh turn, retaining conversation history.

A legacy receipt with the same public run ID and no original proof is ambiguous
and fails closed. A legacy receipt from a different public run can still supply
history. Google ADK and Pydantic AI use stable private native turn IDs derived
from the record pair so native invocation or usage grouping does not alias a
new record onto the old turn. Public Runkite run IDs remain unchanged.

This still cannot promise exactly-once external side effects. A process can
perform a tool side effect and crash before committing its receipt. Use the
tool's own idempotency key where the operation requires it.

## Kafka requires an explicit weaker mode

Kafka's local materialized queue view followed by a log append does not provide
cross-replica compare-and-set. Adding an identity field cannot create that
atomicity. Redis as event broker or reclaim leader does not repair the lease
mutation boundary.

`kafka.NewQueue` therefore returns `ErrNonAtomicFencing` by default, before
network access. An operator can explicitly accept the existing weaker tier with
`RUNKITE_KAFKA_ALLOW_NONATOMIC_FENCES=true`, or the Go option
`kafka.AllowNonAtomicFencing(true)`. Compatibility mode still carries exact
proofs, but its sequential conformance tests are not evidence of atomic HA
fencing. Postgres and Redis remain the supported HA pair.

## Qualification and operator handoff

Regression tests deliberately reuse both public ID and generation while changing
the underlying record. They cover delayed dispatch, fresh stale HTTP/RPC,
renew-to-publication and renew-to-completion races, event close/replay, reaper
acknowledgement, cancellation, receipt replay and missing capability. Each denial
needs a matching-current-record positive control so an unrelated auth failure
cannot make the test pass.

Upgrade all planes, runners and custom transport implementations together after
draining active work and queued assignments. Do not run old and new replicas as
a security rollout. Do not fill an old queued assignment or same-ID receipt from
the current row. Resolve those operations explicitly, then submit fresh runs
with fresh operation IDs. Follow the [operations runbook](../ops-runbook.md) for
the complete procedure and backend restrictions.

The work does not extend the decision-only governance API into managed runs,
proxy local framework database writes, or change Mongo's public SQL-only feature
boundaries. It closes an authority ambiguity inside the existing dispatch and
completion model.

## Local qualification, 2026-11-17

The combined cron and assignment-identity tree passed the complete
`go test ./internal/... ./cmd/...` suite and whole-Go lint with zero findings.
Provider-free native checks used disposable Postgres, MySQL, Mongo replica-set,
Redis and NATS fixtures. All three SQL stores passed record-finalization
conformance; all four stores passed record-scoped terminal-hook conformance.
The MySQL cleanup regression also verifies its test reset removes usage holds.
Kafka passed its full native compatibility suite and the additional retained-slot
fairness case, without an atomic HA claim.

The TypeScript runner passed 303 unit tests. Its two opt-in native tests were
qualified separately: the packaged mTLS case passed, and managed AI SDK plus
native chat bridge passed all 10 cases on SQLite and Postgres/Redis. All four
managed Python SDK profiles passed with freshly installed wheels and native
chat bridges on both backends. Model and tool fixtures are local and scripted;
these are not live-provider tests.

Linux race qualification passed the identity, completion, reservation, timeout,
checkpoint and Admin replay API cases, plus the complete auth, bridge,
in-process transport and SQLite packages. This is targeted API race coverage,
not a claim that the entire API suite passed under the race detector.

The external-runtime HA fixture passed three bounded recovery cycles against
two HTTP replicas, including original-proof propagation, installed runner,
receipt recovery, usage and authenticated gateway TLS checks. Its owned runtime
cleanup was confirmed. Site tests passed 43 cases; OpenAPI coverage matched all
170 registered Go routes.

Hosted qualification on the published commit and release-artifact verification
remain release gates. The checks above do not establish unrestricted production
soak, provider billing truth or exactly-once business effects.
