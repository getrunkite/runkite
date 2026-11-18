# 2. Recovery and the identity of the work

[Notebook home](README.md) · Previous: [who owns the model loop](01-who-owns-the-loop.md) · Next: [authority and evidence](03-authority-credentials-and-evidence.md)

This chapter reconstructs the engineering reasoning visible in the current
implementation, regression tests and earlier design records. It is a
retrospective explanation, not a transcript of the founder's private thoughts.
The reference is the published v0.10.0 contract. Historical release labels come
from the [changelog](../../../CHANGELOG.md); older observations do not become
new qualification merely because they are explained here.

The early control-plane story was straightforward: accept a run, assign it to
a runner, stream progress, store the result. Recovery changes the question.
After a lost connection, which process still owns the work? After a database
commit whose response disappears, should the plane run it again? After someone
deletes a run and reuses its name, whose conversation and budget may an old
request change?

The answer developed through several distinct identities. Mixing them makes a
small compatibility shortcut look harmless until a delayed message arrives.

## Start with the worker, then distinguish its attempts

A runner token must constrain the kind of work its holder can request. Otherwise
a Python runner authenticated for one kind could ask for another kind's jobs.
The v0.5.6 fixes bound the authenticated kind to the context. Today,
[`authenticateRunner`](../../../internal/bridge/interceptors.go) does that
binding, and [`requireRunnerKind` and `requireAssignmentAuthority`](../../../internal/bridge/server.go)
check both the requested kind and the assignment actually dequeued. Checking
only the request would trust a queue routing mistake with another agent's input.
Optional workload-owner checks add another authority boundary; a kind alone is
not a unique process identity. Token-free local mode retains its separate
behavior.

Correct kind still does not identify the current attempt. Imagine runner A
pauses long enough to lose its lease. Reclaim dispatches generation 2 to runner
B. A resumes and sends a successful result from generation 1. Both runners have
the correct kind, but only B's attempt may complete that assignment.

Generation therefore accompanies heartbeat, events and completion, and is
checked at the mutation. Zero cannot mean "whichever generation is current."
Copying the live generation onto a message that omitted it manufactures
ownership for a stale sender. In
[`Server.StreamEvents`](../../../internal/bridge/server.go), the outer RPC's
generation is retained and stamped onto the event after queue renewal succeeds;
a generation inside `event_json` cannot override it. `ReportStatus` follows the
same exact-match rule. An explicitly zero-generation assignment can match zero
in the compatibility contract, but zero never borrows a positive attempt.

Read
[`TestStreamEvents_ZeroGenerationCannotBorrowLivePositiveAttempt`](../../../internal/bridge/generation_zero_test.go).
Its useful feature is the positive control: the exact generation succeeds after
the zero request fails. A denial caused by unrelated authentication would not
prove this fence. This is a recurring method throughout the later tests.

## A delivered outcome is different from a delivery acknowledgement

The next temptation is to acknowledge the queue quickly and write the database
afterward. That removes the recovery handle before establishing what survived.
If the write fails, the job is gone while its run can remain unfinished.

The v0.5.6 correction made persistence precede acknowledgement. The current
sequence is more precise. [`ReportStatus`](../../../internal/bridge/server.go)
asks the durable completion callback to finish first. The store transaction
accepts only the appropriate original record and attempt. It commits run status
and thread release alongside any applicable output, merged state and completion
checkpoint.
[`Driver.CompleteRunInTx`](../../../internal/state/sqlcompletion/completion.go)
shows the SQL boundary. `CompletionResult.Accepted` distinguishes a valid retry
from rejection; `Applied` distinguishes the first transition from confirmation
of an existing winner. A no-lease `ReplayOnly` request may confirm a committed
winner, but cannot create a fresh terminal transition.

After commit, [`finalizeCommittedRun`](../../../internal/api/completion.go)
settles usage, releases the hold, handles cache and broker closure, and performs
terminal-hook bookkeeping. Only a successful callback permits the bridge's
fenced `Ack`. These are retryable stages, not one transaction spanning SQL,
Redis and HTTP.

Consider a narrower failure: values and a terminal event reach the broker, then
the stream-close acknowledgement disappears. Reporting a new `error` would
replace the agent's outcome with a transport diagnosis. Python
[`finish_run`](../../../python/runkite_runner/completion.py) and TypeScript
[`finishRun`](../../../typescript/runkite-runner/src/completion.ts) instead raise
`CompletionDeliveryError` without sending a replacement status. Completion
retries resend the same outcome, not the model execution.

Before delivering a reclaimed attempt,
[`BeginRunAttempt` and `recoverDeliveredCompletion`](../../../internal/api/completion.go)
look for the matching retained terminal event. The regression
[`TestPoisonPillRecoversPersistedSuccessAfterLostStreamAcknowledgement`](../../../internal/api/completion_recovery_security_test.go)
keeps an actual success successful even at the retry ceiling. Missing history
and a failed replay are different: an unavailable broker cannot be interpreted
as proof that no successful outcome exists.

## Blob concurrency does not establish execution authority

Opaque adapter state existed by v0.3.0. The managed AI SDK path in v0.5.8 added
native history and committed-turn receipts using conditional writes. An ETag
answers a specific question: has this blob changed since I read it?

It does not answer whether its writer still owns a run. Suppose generation 1
reads blob version 7, begins a PUT, and stalls while its body is uploaded.
Generation 2 becomes current without changing that blob. The old writer still
knows the correct ETag. Its `If-Match: "7"` can succeed while its assignment is
already superseded. Authentication at request entry has the same timing hole.

The v0.6.0 attempt fence moved the decisive check into the mutation transaction.
[`handlePutOpaqueCheckpoint`](../../../internal/api/checkpoints.go) supplies the
captured binding to `PutBoundOpaqueCheckpoint`.
[`LockRunCheckpoint` and `WithRunCheckpoint`](../../../internal/state/sqlcompletion/checkpoint.go)
retain the durable run lock through the blob mutation. A changed generation or
terminal run fails independently of ETag comparison. The
[`checkpoint fence regression`](../../../internal/api/checkpoint_fence_test.go)
pauses the body after authorization and exercises unconditional, CAS,
create-only and terminal cases.

A second counterexample survived that improvement. Delete physical record R1
for public ID `task-1`; create R2 with the same ID. Both can have generation 1.
An old request now matches the public ID and attempt number again. Neither a
timestamp nor the current row can reconstruct which record the worker originally
received.

v0.10.0 carries `(run_id, run_incarnation_id, generation)` across the boundary.
It reuses SQL's persisted incarnation and Mongo's private ObjectID identity,
rather than creating another identity service.
[`GetJob`](../../../internal/bridge/server.go) requires
`assignment_identity_version: 1` before dequeue, and
[`bindInternalRun`](../../../internal/auth/runbind.go) validates the echoed
original identity. `LockOpaqueCheckpoint` checks that same identity under the
mutation lock. Missing proof is not repaired from a newer row. See
[`TestOpaqueCheckpointWriteRejectsRecreatedRunAfterAuthorization`](../../../internal/api/checkpoint_incarnation_test.go)
for a delayed request meeting a replacement with the same generation.

The incarnation is a continuity proof, not a secret or attestation. It cannot
make a compromised current holder trustworthy. It also does not replace ETags:
one fence selects the authorized record and attempt; the other protects the
blob's revision.

Event delivery needs the same distinction. Renewal and publication remain two
operations, so a successful lease check is not an atomic broker commit.
[`RunRecordKey`](../../../internal/transport/run_record.go) supplies record-scoped
namespaces for publish, subscribe, replay and close. Filtering only event JSON
would leave an old `Close` able to close a replacement's stream. Durable terminal
and values scans also match the attempt generation; the
[`completion reconciliation`](../../../internal/api/completion.go) protects the
stored outcome. Raw nonterminal chunks remain advisory during reclaim and must
not be merged indiscriminately across attempts.

## Recover committed turns without claiming to recover external effects

A managed adapter can commit conversation state and then lose its process before
reporting success. Re-running the model and its tools would discard valuable
knowledge the checkpoint already contains.

[`AiSdkAdapter`](../../../typescript/runkite-runner/src/aiSdkAdapter.ts) saves
native history together with a completed-turn receipt before publishing final
values. `AiSdkState.save` uses the existing ETag or create-only precondition.
The managed Microsoft adapter's
[`AgentFrameworkState`](../../../python/adapters/agent_framework_adapter/state.py)
uses the corresponding versioned Python client. These are adapter-owned formats;
the plane stores opaque bytes rather than becoming another framework runtime.

On reclaim, the AI SDK adapter validates the receipt's original record and
input, then replays the committed result without invoking the factory again.
A known different incarnation means a new turn, preserving conversation history.
A legacy same-public-ID receipt without proof is ambiguous and is refused.
The native end-to-end case
["crash after native state commit reclaims the receipt without repeating model or tool work"](../../../typescript/runkite-runner/src/aiSdk.e2e.test.ts)
places the crash after the PUT committed, before the runner received its response.

Move the crash earlier and the conclusion changes:

1. A tool commits its downstream write.
2. The runner dies before committing its receipt.
3. Recovery finds no committed receipt proving that write.

No local receipt design can resolve that window by itself. A downstream
idempotency key, queryable operation handle or explicit reconciliation is needed.
The same distinction explains why an uncertain connector response is not a
safe automatic replay. Better recovery of Runkite's records does not establish
exactly-once business effects.

## Cancellation and exhaustion must retain the right unfinished work

A superseded attempt should stop itself. A genuine user cancellation should
reach every overlapping attempt of the same original record. Those are different
signals. If they share one mutable `cancelled` flag, generation 1's superseded
heartbeat can stop generation 2 inside the same worker process.

The v0.6.0 fix separated attempt-local flags. Current Python
[`register_run` and `record_cancel_signal`](../../../python/runkite_runner/cancellation.py)
and TypeScript
[`registerRun`, `recordCancelSignal` and `unregisterRun`](../../../typescript/runkite-runner/src/worker.ts)
group user cancellation by record but allocate independent attempt flags.
Cleanup removes only its own registration. Cancellation preserves only eligible
usage already observed from the matching attempt, not unfinished conversation
values. A bounded tombstone remembers a cancel that arrives before registration;
record-scoped keys keep it away from
a replacement run with the same public ID.

Exhausting retries is also not permission to delete recovery state.
[`CompleteReclaimedRun`](../../../internal/api/completion.go) first tries to
recover a delivered outcome and finalize any committed result. A poison record
names the rejected next generation; the retained lease still belongs to the
previous one. [`finishPoisonPill`](../../../cmd/reclaim.go) acknowledges that
retained tuple only after completion/finalization permits disposal. Lookup
failure or malformed identity retains uncertainty. A definitively deleted or
replaced durable record can retire only its old lease, without finalizing the
replacement.

[`TestPoisonCompletionPostcommitFailureRetriesFinalizationBeforeAck`](../../../internal/api/poison_completion_ordering_test.go)
interrupts broker closure after success is stored. The next tick retries
bookkeeping while preserving the successful result and single checkpoint. The
lease is deliberately useful even when no agent execution remains to perform.

## FinOps and notifications inherit the identity problem

The v0.3.0 FinOps design added daily budgets, metering and reservation holds.
Terminal usage alone misses work still running, so configured optimistic holds
count toward USD/token caps before settlement. They are estimates, not provider
quotas. Wrong sizes, expiry and concurrent work can still overshoot. Locked
`COUNT + INSERT` admission limits are a separate guarantee from estimated spend.
The [configuration contract](../../configuration.md#langgraphjson) and
[limitations](../../limitations.md) retain these distinctions.

Original-record identity must survive beyond the completion transaction. An old
finalizer can pause, a run can be replaced, and then the finalizer can release a
hold addressed only by public ID. In
[`TestFinalizationCannotReleaseRecreatedRunsHold`](../../../internal/api/finalization_identity_test.go),
the replacement's 17-token hold must survive that schedule.

The current [`usage.go`](../../../internal/api/usage.go) therefore calls fenced
`WriteRunUsageEvent`, `UpsertRunUsageHold` and `ReleaseRunUsageHold`, with no
unbound fallback. SQL migration 23 on Postgres/SQLite and 24 on MySQL binds
accounting to the original record and makes usage uniqueness
`(run_id, run_incarnation_id, source)`. Legacy rows remain unbound. Guessing their
owner from a reusable ID could either erase a charge or bill it again. Upgrade
requires backups, stopped writers and matching planes/runners; ambiguous old
accounting needs reconciliation, not automatic backfill. These accounting
capabilities remain SQL-only; Mongo's private record proof does not supply them.

Settlement is still observed usage and pricebook or reported cost, not an
invoice. [`persistTerminalUsage`](../../../internal/api/usage.go) skips cache
replay billing and preserves unpriced/unmetered distinctions. It never calls a
provider billing API.

Terminal-hook claims follow the same record fence through
[`TryClaimRunTerminalHook`](../../../internal/state/sqlcompletion/terminal_hook.go).
However, [`finishRun`](../../../internal/api/server.go) commits the claim before
placing an observational hook on the bounded in-memory dispatcher. A crash in
that gap, or a saturated queue, can lose notification. A winning claim is not a
durable outbox or proof of receiver delivery. Picked-up webhook jobs have their
separate retries and optional dead letters.

By contrast, v0.7.0's
[action-evidence outbox](../action-delivery-outbox.md) commits configured delivery
rows with action events. Its
[`Worker.RunOnce`](../../../internal/actionexport/worker.go) leases a delivery,
sends its stable ID and acknowledges the current claim. Lost acknowledgement
can resend the evidence; receivers deduplicate it. This exports evidence, never
re-executes a tool. If durable terminal notifications become a requirement, that
transactional-outbox distinction would change the design; renaming the existing
terminal claim would not satisfy it.

## Choose atomicity at the backend boundary

The same check-then-change rule reaches the queue.
[Redis `ackScript`](../../../internal/transport/redis/redis.go) checks identity
and generation and removes tracking without an intervening client round trip.
NATS uses revision-guarded KV mutations; the in-process queue uses its lock and
assignment copies. But atomic execution is not rollback: Redis Lua writes made
before an error remain made. Redis Nack/reclaim therefore writes the ready
destination before discarding the recoverable source. The
[`Nack retention regression`](../../../internal/transport/redis/nack_retention_test.go)
forces a wrong-type destination and verifies the lease survives.

Kafka's local materialized state followed by a log append cannot supply
cross-replica CAS. A process mutex or Redis reclaim leader does not repair that
mutation boundary. v0.10.0 therefore refuses queue construction by default;
[`TestDefaultConstructorRefusesNonAtomicFencingBeforeNetwork`](../../../internal/transport/kafka/strict_fencing_test.go)
proves the refusal occurs before broker access. Explicit
`RUNKITE_KAFKA_ALLOW_NONATOMIC_FENCES=true` accepts the weaker Compatible mode.
Postgres and Redis remain the Supported HA pair.

An authoritative cross-replica conditional-write mechanism, with demonstrated
race and failure behavior, could justify revisiting that Kafka boundary. More
happy-path retries would not. Likewise, receipts, ETags and durable completion
solve their particular uncertainty windows; none supplies the missing
transaction with an arbitrary external service.

## Questions to work through

1. Why does a correct ETag fail to prove that a writer is current? Revisit the
   paused PUT timeline and `LockOpaqueCheckpoint` above.
2. What must a replacement change when its public ID and generation repeat?
   Trace `RunIncarnationID` from `GetJob` to the checkpoint incarnation test.
3. Why retain a lease after success is durable? Follow the poison postcommit
   regression through `finalizeCommittedRun` and `finishPoisonPill`.
4. Which crash can committed-turn replay handle, and which needs downstream
   reconciliation? Compare the native AI SDK test with the three-step effect
   window above.
5. Does a terminal-hook claim prove eventual delivery? Compare `finishRun` with
   `actionexport.Worker.RunOnce` and the outbox's receiver-deduplication contract.
