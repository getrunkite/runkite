# Atomic results and exact runtime identities

This design record covers the v0.6.0 follow-up to `4c56ca0`. It complements
[Runtime hardening](runtime-hardening.md): the earlier pass fixed normal runner
completion and argument authority, while this pass follows the same invariants
through cache hits, thread replacement and runner identity transport. A source
change or unchanged package version does not establish published availability.

## A cache hit is still a durable completion

The normal completion transaction already committed run output, merged thread
state and a matching state checkpoint together. `tryServeCachedRun` instead
inserted a successful row, separately wrote output, then saved a checkpoint and
updated the thread. It logged or ignored later failures. A cache hit could return
success with missing output, stale state or no checkpoint. Its checkpoint also
contained only the cached result patch, losing unrelated prior thread keys.

The existing completion transaction is now reused inside cached creation.
Built-in stores implement optional `state.CachedRunCreator`: admission, exact
parent ownership, the busy-thread check, run insertion and ordinary completion
share one transaction. A pending intermediate row is never committed by itself.
`run.output` stays the cached result patch; the checkpoint contains merged thread
state. A known transaction failure rolls back the cached run and thread changes.
A lost commit acknowledgement can leave a complete committed result; a same-ID
retry reconciles it instead of assuming the transaction never ran. A custom
store without this optional boundary treats the cache lookup as a miss and uses
ordinary runner dispatch. The required `state.Store` interface is unchanged.

An ordinary pending run followed by a separate completion would be smaller code,
but a failure between them could strand a run that was never queued. A durable
cache intent or a second recovery queue is unnecessary when the stores already
have the transaction needed for this result.

The old path also ingested provider usage from the original cached output.
Replaying 20 input and 4 output tokens could therefore charge those tokens again
without a model call. Terminal accounting now skips the plane-owned `cache_hit`
marker. Original usage metadata remains in the returned output for inspection,
but the new cached run creates no usage hold and no additional usage-ledger row.
It still counts as a newly created run for daily run quotas. Reading a cache entry
does not extend its TTL or write another copy of that result. Existing usage
ledger rows are not rewritten. Review earlier cache-hit accounting before
correcting historical estimated spend; this is not a provider billing refund.

Cached results use normal durable finalization and terminal-hook claims. A
same-ID retry can finish bookkeeping after a transient failure or lost response;
a matching concurrent retry reuses its committed winner even if that winner used
the last daily quota slot. This does not make external side effects idempotent.
Whole-run caching remains opt-in for suitable stateless agents and unsupported
for the managed AI SDK profile.

Sources: [cache creation](../../internal/api/runs.go),
[cache contract](../../internal/state/cached_run.go),
[SQL completion](../../internal/state/sqlcompletion/completion.go),
[Mongo completion](../../internal/state/mongo/completion.go),
[usage ingestion](../../internal/api/usage.go).
Regressions in [cache completion](../../internal/api/cache_completion_regression_test.go)
and [shared store coverage](../../internal/state/conformance/cached_run.go) cover
merged state, exact numbers, injected output/checkpoint/thread write failures,
usage, TTL, concurrent retry, quota and cross-replica finalization.

## Child ownership belongs in the write transaction

The API already read and claimed a thread under the caller's tenant. That proved
ownership at the time of the claim. It did not protect the later child insert.
Another request could delete the busy thread, and a different tenant could reuse
its globally unique custom ID before the delayed insert. An ID-only foreign key
then accepted the old tenant's run against the replacement parent's ID. Ordinary
checkpoints and opaque writes had the same race.

Every built-in run insertion now uses the existing admission transaction, even
when no admission caps are configured. The transaction verifies and locks the
exact parent tenant before inserting. Checkpoint writes hold that same parent
fence through mutation. Bound opaque writes retain their durable attempt fence
as well. Scope locks retain their existing order before parent locks.

Mongo snapshot reads alone cannot fence concurrent deletion. A private parent
revision write causes the conflicting transaction to retry; it does not alter
public thread values or the optimistic-concurrency version. Mongo thread deletion
also commits the parent and its actual tenant's child deletions in one existing
transaction. A failed child deletion rolls back the parent. A late cascade can
no longer remove children belonging to a replacement tenant.

No schema migration or new thread-incarnation protocol is introduced. This
prevents new malformed relationships, not automatic repair of old ones. MySQL
migration 17 already preflights its existing references. Same-tenant ID reuse
keeps its previous semantics, and native LangGraph tables remain separate from
these control-plane child stores.

Sources: [SQL parent fence](../../internal/state/sqlcompletion/thread.go),
[Mongo parent fence](../../internal/state/mongo/thread_ownership.go),
[Mongo deletion](../../internal/state/mongo/mongo.go).
The [actual API regression](../../internal/api/thread_replacement_test.go)
replaces a parent after its claim and requires 404 with no child or replacement
status change. Shared [ownership cases](../../internal/state/conformance/thread_ownership.go)
cover ordinary/admitted runs and checkpoint writes. The
[Mongo abort regression](../../internal/state/mongo/thread_delete_atomic_test.go)
requires both parent and child to survive a failed cascade.

## Transport identities must retain the submitted value

The TypeScript pre-execution guard omitted the assignment tenant when querying
`/internal/runs/{id}/status`. A named-tenant run could appear absent in `default`
and be discarded until reclaim. The guard now uses the existing task-local tenant
headers. Both language helpers encode a custom run ID as one URL path component.
The guard remains advisory: transport and non-404 HTTP failures retain their
existing fail-open behavior; a known missing or terminal run is discarded.

Task-local binding no longer trims tenant or run identities. The plane derives
bound tenant identity exactly from the assignment and compares tenant allowlists
exactly. Empty tenant identity still means `default`. An identity with spaces
must not borrow another tenant or run by normalization.

HTTP header values are not an unrestricted Unicode transport. For printable
ASCII identities without outer whitespace, helpers retain the existing raw
headers. Other identities use unpadded base64url of their UTF-8 bytes in
`X-Runkite-Run-Id-B64` or `X-Runkite-Tenant-Id-B64`, omitting the corresponding raw
header. For the identity used by a route, the plane rejects malformed/noncanonical
encoding, invalid UTF-8, duplicate headers or both representations at once. Generation remains
mandatory for a bound request. Run/agent/thread authority still comes from the
in-flight assignment, never from an encoded tenant claim on a bound route.

Upgrade the plane before runners that emit encoded headers. An older plane
cannot interpret that representation and must not be expected to support these
identities. New run creation rejects controls and leading/trailing whitespace
with HTTP 400, preventing older trimming helpers from aliasing newly created
IDs. Unicode and ordinary custom path characters remain supported. Encoded
bindings can address an exact existing legacy ID without renaming stored rows.

Sources: [HTTP binding](../../internal/auth/runbind.go),
[HTTP middleware](../../internal/auth/auth.go),
[tenant allowlists](../../internal/auth/runner.go),
[Python context](../../python/runkite_runner/tenant_ctx.py),
[TypeScript context](../../typescript/runkite-runner/src/tenantCtx.ts),
[status guard](../../typescript/runkite-runner/src/runStatus.ts).
Tests include [exact Go binding](../../internal/auth/exact_binding_test.go),
[new-ID rejection](../../internal/api/identity_contract_test.go) and both SDKs'
identity, status and connector override cases.

## Legacy native checkpoint keys need an explicit safe domain

LangGraph memory and direct Postgres savers use the bare thread ID for `default`,
and `{tenant}:{thread}` for named tenants. The plane's opaque proxy uses exact
scoped rows instead. Concatenation was ambiguous: `default` / `acme:target` and
`acme` / `target` selected the same native conversation. A colon in a named tenant
created further aliases.

Rejecting only ambiguous new inputs is insufficient for persisted history.
Suppose an older default-tenant run wrote `acme:target`. After an upgrade, a new
named-tenant `acme` / `target` run would pass a delimiter-only guard and still read
that old default-tenant conversation. An operator-audit caveat cannot enforce
ownership at the read boundary, and legacy saver rows contain no trusted tenant
ownership attestation.

**Direct Postgres LangGraph now rejects every non-default tenant before graph
factory, state access or execution.** Named tenants must use HTTP proxy
checkpoints and explicitly resolve any prior native history. Default direct
threads without `:` retain their existing keys and history; default thread IDs
containing `:` are refused. There is no owner-marker scheme, automatic import,
new blank namespace or silent memory fallback.

Shared memory starts fresh with the runner process, so its guarded domain can
still include named tenants. Memory execution rejects default-tenant thread IDs
containing `:`, named tenant IDs containing `:`, and tenant IDs with outer
whitespace or ASCII controls. Named-tenant memory thread IDs may contain colons.
HTTP proxy checkpoints continue to use exact scoped rows for all tenants and do
not impose these native saver restrictions.

**Default cron threads use `cron:<name>` and therefore need proxy checkpoint
mode too.** Unset runner `POSTGRES_DSN` and set `RUNKITE_HTTP_URL` to select it.
Changing modes does not import native history: inspect ownership and explicitly
resolve existing history before continuing, rather than treating a blank proxy
thread as a migrated transcript. Native history already mixed under a legacy key
cannot be assigned back to an owner automatically. Python and TypeScript still
cannot resume each other's serialized histories.

Managed AI SDK, Microsoft, OpenAI Agents, Pydantic AI and Google ADK profiles
already use opaque HTTP state; this direct LangGraph restriction does not change
their persistence mode or conversation ownership.

Sources: [Python validation](../../python/runkite_runner/tenant_ctx.py),
[Python execution](../../python/runkite_runner/worker.py),
[TypeScript validation](../../typescript/runkite-runner/src/tenantCtx.ts),
[TypeScript execution](../../typescript/runkite-runner/src/executeRun.ts).
Real LangGraph saver regressions are in
[Python](../../python/tests/test_checkpoint_identity.py) and
[TypeScript](../../typescript/runkite-runner/src/checkpointIdentity.test.ts).

## Dispatch must verify the job returned by the queue

The bridge already authenticated the requested runner kind. A queue-routing bug
could still return an assignment for a different kind. `GetJob` now checks the
actual dequeued assignment against the authenticated kind before
`BeginRunAttempt`, serialization or payload delivery. A misrouted assignment is
returned with a generation-bearing `Nack`; a failed return is reported as
unavailable rather than delivering the job. Local mode without authenticated
runner tokens retains its trusted behavior.

One concrete source was NATS durable-name sanitization: both `a.b` and `a_b`
map to `jobs-a_b`. Updating that durable from a second replica could retarget its
subject filter while the first replica still used it. Consumer creation now
reuses an identical configuration but rejects a conflicting existing filter,
instead of changing another kind's durable. Existing names stay unchanged.
Choose configured kinds with distinct derived durable names, and reconcile
queued work before renaming a conflicting kind across config, tokens and runners.
The create-only call follows the [upstream Go client contract](https://github.com/nats-io/nats.go/blob/v1.52.0/jetstream/stream.go#L341): identical configuration reuses a consumer and conflicting configuration returns an error.

There is no automatic consumer or retained-history migration. Do not delete or
retarget a durable merely to suppress the collision error.

Sources: [bridge dispatch](../../internal/bridge/server.go) and
[NATS consumer creation](../../internal/transport/nats/nats.go).
Regressions in [bridge kind checks](../../internal/bridge/dequeued_kind_test.go)
and [consumer collisions](../../internal/transport/nats/consumer_collision_test.go)
cover rejection before durable attempt advancement, safe return, local mode,
preserved original filters and identical same-kind replica reuse.

## A failed local requeue must retain its lease

The in-process queue already fenced `Nack` by generation, but removed its
in-flight entry before enqueueing the returned assignment. A full 1,000-slot
ready channel or cancelled caller could then block or fail after the only
recoverable lease was gone. Reclaim had the same delete-before-enqueue shape.

Generation/context checks, a nonblocking ready write and lease removal now share
the existing queue mutex. A full buffer or known cancelled context reports an
error and retains the original lease and generation. The queue never waits for
channel capacity while holding that mutex. Once capacity becomes available, the
normal retry or reaper can return the assignment. Reclaim advances a generation
only for a successful ready handoff; a partially completed batch reports only
those committed handoffs and retains the others.

This reuses the existing ready queue and lease map. It is recoverability inside
a live local process, not durable storage or a new HA guarantee. Process exit
still loses in-memory state.

Source: [in-process queue](../../internal/transport/inprocess/inprocess.go).
[Requeue retention regressions](../../internal/transport/inprocess/requeue_retention_test.go)
cover full buffers, cancelled calls with and without free capacity, stale
Nacks, partial reclaim and successful retry without duplication.

## NATS must treat a run ID as a literal partition

The NATS broker built a subject filter by appending the caller-chosen run ID.
A valid run record named `*` or `>` could therefore authorize an SSE request for
one run while JetStream replay or subscription interpreted the ID as a wildcard
for other runs. Tenant filtering of the run row did not constrain that broker
filter. Cancellation subscriptions used the same subject construction.

The Compatible NATS backend now validates run IDs before queue, broker or cancel
operations. It keeps the existing JetStream KV alphabet: nonempty ASCII letters,
digits, `-`, `_`, `/`, `=` and single internal dots. Leading/trailing dots, empty
dot segments, wildcards, whitespace, colons and Unicode are rejected. Event and
cancel consumers also require the exact received subject before yielding data.
The API uses optional `transport.RunIDValidator` before preflight, thread, run
or cache mutation, returning 400 for unsupported NATS IDs. The Supported
Postgres/Redis profile does not acquire this NATS-only restriction.

Valid existing keys and subject names do not change. Encoding every ID into a
new namespace would require migration of retained event history and queued work;
this patch instead closes unsafe interpretation at the boundary already used by
the backend. An unsupported legacy run may remain readable in durable metadata,
but its NATS stream/cancel/queue operations fail closed. A queued legacy message
with an unsupported ID is negatively acknowledged with a delay and an explicit
error, retaining it for operator investigation instead of acknowledging or
terminating uncompleted work. Inspect and reconcile the stored run and possible
external effects before deciding whether to resubmit under a supported ID.

Source: [literal partition validation](../../internal/transport/nats/run_identity.go)
and [NATS operations](../../internal/transport/nats/nats.go).
[Live regressions](../../internal/transport/nats/run_identity_test.go) cover
wildcard replay/subscription rejection and preservation of valid literal
partitions. Existing retention, `Close` no-op and backend support tiers remain.

## Scope and evidence

These fixes reuse completion, admission, tenant context, transaction adapters and
saver selection already present in the repository. They do not add a second
control plane, vault, policy engine or model loop. The handoff manifest records
executed commands, backend profiles, optional skips and artifact hashes. An
unexecuted image build, external provider or HA soak is not implied by a unit or
local integration pass. The product's [documented boundaries](../limitations.md)
still apply. No finite review proves absence of every bug.
