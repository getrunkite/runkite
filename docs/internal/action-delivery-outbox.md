# Durable action evidence delivery

These storage changes are in the v0.7.0 candidate (not published). The outbox extends
[connector action evidence](connector-action-evidence.md). It delivers metadata
about an action that already exists; it never authorizes running a tool.

## Commit evidence before exporting it

A best-effort HTTP call after an action transaction leaves a gap. A process can
commit dispatch intent and crash before an external collector receives it. A
synchronous collector call inside that transaction creates a different gap: the
collector can accept a message while the database later rolls back.

`saveConnectorAction` therefore inserts an outbox row in the same SQL transaction
as the action snapshot and event. This happens only when the action's immutable
`export_sink_id` is nonempty. A failed insert rolls back the full action
transition. During protected dispatch, that includes approval consumption and
dispatch intent. Export-disabled deployments create no delivery rows.

The delivery ID is deterministic for the tenant, action, event sequence, and
logical sink ID. A replay of a previously committed action mutation returns its
existing result and creates no second delivery. The payload is the typed action
and event metadata in the `runkite.action-event.v1` envelope, bounded at 64 KiB.
There are no tool arguments, response bodies, credential values, transport URLs,
provider error strings, or delivery claim tokens in that contract.

The effective sink ID is SHA-256 over the configured logical name, exact URL
and envelope schema. Only the derived ID enters action/delivery metadata, not
the endpoint. Changing URL creates another queue identity, so the worker cannot
silently route the old queue to that new endpoint. Removed destinations leave
their old queue held; restore the exact original destination contract to resume
its delivery. Rotating the HMAC key does not change that destination identity.

## Claim delivery, not execution authority

Only a system worker context can claim, acknowledge, or fail deliveries. A claim
transaction locks eligible rows and assigns each a new random claim token and
lease deadline. Postgres and MySQL use `FOR UPDATE SKIP LOCKED`; SQLite uses its
existing serialized transaction. Batches are limited to 100.

Acknowledgement and failure require the current unexpired claim token. A stale
worker cannot acknowledge or fail a replacement worker's lease. Attempts count
claims, including recovery after a worker disappears. A retriable failure clears
the claim and sets a future `available_at`. Terminal failure leaves a
`dead_letter` row for investigation. Failure metadata accepts a bounded fixed
reason code, not raw HTTP or provider text.

If a receiver accepts a delivery and its acknowledgement is lost, a later claim
sends the same delivery ID and immutable payload again. This is at-least-once
delivery. Receivers must deduplicate on delivery ID. Concurrent delivery workers
can observe different action-event sequences out of order; consumers must use
the action ID and event sequence when reconstructing state. The outbox does not
provide exactly-once remote processing or exactly-once tool effects.

## Investigation, retry, and retention

Tenant-scoped search returns metadata and payloads without claim tokens. Paging
uses an explicit offset, with a maximum offset of 10,000 and a maximum page of
100. It does not claim stable cursor pagination under concurrent updates.
Postgres applies the existing tenant RLS policy to `action_deliveries` as well
as application predicates.

An explicit retry changes an owned dead letter back to pending. It retains the
delivery ID, payload, and cumulative attempt count. It does not call the tool,
change action state, consume an approval, or reset the original review.

The outbox has no foreign key to the action, run, or thread. Pruning those
records cannot erase an undelivered export or its investigation history.
Delivered rows are retained by this storage contract too. Operators enabling
export must plan storage capacity and an explicit retention policy; disabling
export stops new rows but does not erase the existing queue.

## Migration and checks

Postgres and SQLite migration 20, and MySQL migration 21, add the outbox and the
immutable action sink column. They preserve existing actions without inventing
historical deliveries. Downgrade refuses to discard queued delivery state.
Follow the stop-writers, verified-backup, migrate-all-replicas procedure before
traffic. An already released old binary does not enforce this new contract.

The shared SQL conformance tests cover disabled export, mutation replay,
immutable sink identity, metadata bounds, concurrent claims, stale
acknowledgements, expired-lease recovery, dead-letter retry, backoff, tenant
isolation, and delivery retention after action pruning. SQLite injects failed
outbox writes to prove action creation and approval-consuming dispatch roll back
atomically. Postgres also runs the contract with tenant RLS and checks that
queries missing an application tenant filter cannot expose foreign deliveries.
These tests qualify persistence and worker ownership. Real collector HTTP
behavior is a separate dispatcher integration check.
