# Local HA validation: load and recovery are separate claims

This design record accompanies the unreleased validation changes based on
`53160fc`. Postgres and Redis remain the Supported state/transport pair. The
control plane stays one binary; runners, nginx and the webhook sink remain
separate processes. No new runtime service or persistence subsystem is added.

## What the existing tools already establish

[The multi-plane smoke](../../scripts/smoke-multi.sh) checks readiness, agent
registration, one echo run and Admin visibility. The
[Python](../../bench/soak/load_python.py) and
[JavaScript](../../bench/soak/load_js.mjs) loaders exercise concurrent fresh-thread
create/wait traffic. The [soak writeup](../../bench/soak/WRITEUP.md) defines a
30-minute local announce bar and requires manual grading of its artifacts.

Those checks do not inject process failures, deliberately replay one cached
result, or prove stream replay on a different replica. A loader finishing is not
evidence for those separate invariants. The new
[`Recovery` harness](../../bench/soak/recovery.py) makes the additional assertions
explicit and returns a failing exit status when one does not hold.

## Keep the fixture inside the checkpoint contract

The multi-plane fixture previously set `POSTGRES_DSN` on both Python runners,
selecting native direct checkpoints. Its bundled cron uses a default-tenant
thread named `cron:<schedule>`. The new native identity guard correctly rejects
that colon key, so the existing fixture could fail before its agent ran.

[The runner environment](../../docker-compose.multi.yml) now clears that DSN and
uses the already configured `RUNKITE_HTTP_URL` for proxy checkpoints and store
access. The control planes retain Postgres. This reuses the existing run-bound
HTTP/CAS checkpoint path instead of weakening the native guard or creating a
special cron exception. Existing volumes with native history need explicit
history reconciliation before a mode change. Empty proxy history is not an
automatic import of old native state.

The mode choice lives in
[`CheckpointerManager.start`](../../python/runkite_runner/checkpoint.py), the
native guard is
[`validate_native_checkpoint_identity`](../../python/runkite_runner/tenant_ctx.py),
and [`ProxyCheckpointSaver`](../../python/runkite_runner/proxy_checkpoint.py)
already serializes each blob with conditional HTTP writes. The fixture changes
select those existing paths rather than add another checkpoint implementation.

The soak sink also had a configurable listener port but fixed URLs in the graph
fixture. `SOAK_SINK_PORT` now reaches both the plane environment and
[hook URLs](../../examples/soak_multi/langgraph.json). Its default stays `9099`.
The loaders accept optional `SOAK_API_KEY`, allowing a disposable authenticated
overlay to exercise the same public API. Recovery can use a separate
`SOAK_ADMIN_API_KEY`; absent that, it uses the client key for Admin reads.

## Assert recovery through durable records

`Recovery.cache_case` warms one exact input through one plane, then submits
concurrent identical run IDs through two planes and reconciles through the third.
It requires one completed run, the cached output, a merged checkpoint retaining
the target thread's unrelated values, and one observed terminal-hook delivery
during the verification window. The
[metered fixture](../../examples/soak_multi/metered.py) supplies explicitly
synthetic 20-input/4-output usage. The source run must settle one ledger entry;
cached retries must add none. This tests metering behavior with positive values,
not a provider invoice or a refund. The fixture makes no model API call.

`Recovery.cancel_case` dispatches a slow run, cancels through another plane and
restarts workers. The durable interruption must remain unchanged, the thread must
be released as `interrupted`, and unfinished values must not replace its previous
state. Thread `interrupted` is the existing completion contract, not a failed
attempt to return it to `idle`.

`Recovery.reclaim_case` loses workers and one control-plane process, then requires
the same run ID to complete with a newer durable generation and a matching
completion generation. Postgres and Redis remain available. This checks process
recovery and attempt fencing; it does not promise exactly-once external effects
after an uncertain crash.

`Recovery.replay_case` disconnects a raw GET SSE consumer and reads complete
retained history through another plane. It requires the same attempt, terminal
success confirmed by the stored run, and replayed values matching durable output.
This is full retained raw SSE replay. It is not managed chat UI reattachment or
incremental `Last-Event-ID` cursor validation.

[`Server.StreamEvents`](../../internal/bridge/server.go) preserves legacy event
IDs at generations 0 and 1, adding a `:gN` suffix only above generation 1. The
replay assertion follows that format and checks the durable attempt separately;
requiring a suffix on the first attempt would reject valid retained events.

## Keep retry identity and leases intact on failures

The follow-up changes based on `7a198e9` address failures beside the measured
process-loss path. They reuse the existing queue lease, durable completion and
finalization contracts. They add no service or schema and leave the public
Runner Protocol unchanged.

### Poison records carry literal identities

[`Queue.ReclaimStale`](../../internal/transport/redis/redis.go) previously joined
the run ID, rejected generation and assignment JSON with `:` in a Lua response.
Caller-selected run IDs can themselves contain colons on Redis. Splitting that
response could truncate the ID, mistake part of it for the generation and direct
trusted terminal processing at a different run, including one in another tenant.
UUID-shaped test IDs hid that ambiguity. Matching the parsed ID to a live lease
was insufficient when that different run and generation also existed.

The script now returns three separate RESP fields. `parseDeadEntry` validates
their types and requires the original assignment's run ID to match the returned
ID. A malformed response returns an error instead of silently skipping a poison
assignment. The live lease stays recoverable. This is an ephemeral script result,
not a Redis key or stored-record format, so it needs no Redis data migration.
The fix preserves supported literal IDs instead of adding a Redis-only ban on
colons. NATS retains its separately documented identity restriction.
All plane replicas must be upgraded before relying on the protection. Existing
run outcomes and dead-letter records are not repaired automatically; affected
custom IDs and possible prefix-ID collisions need explicit historical review.

### Enqueue before removing the recoverable source

The same reclaim script now performs the ready-list `LPUSH` before advancing the
generation and removing the old lease. Redis runs Lua without interleaving other
commands, but a script error does not roll back earlier writes. Removing the
lease first could therefore lose the only recoverable payload if the ready-list
write failed, for example because the destination key had the wrong type.

The successful handoff still changes the generation and source tracking in one
script. A failed ready write leaves the old payload and fence available for a
later attempt. This changes command ordering rather than adding another queue or
transaction mechanism. Operators must investigate an incorrectly typed queue
key and reconcile its contents before repair; deleting work is not recovery.

### Cancellation retains finalization work

[`Server.cancelRunSingle`](../../internal/api/runs.go) already commits a durable
interruption before removing its queue lease. The remaining failure was after
that commit: [`CompleteRun`](../../internal/api/completion.go) can accept the
terminal result and still return an error from usage, hold, broker or hook
finalization. Removing the lease in that case stranded the bookkeeping until
another caller happened to retry.

The cancellation path now returns that error while retaining the lease and
tries to notify the runner. The stored interruption remains authoritative. An
ordinary reclaim can revisit the terminal run, retry idempotent finalization and
acknowledge it only after that succeeds. A runner notification can still fail
with the transport; this does not prove an external operation stopped or undo
one that already ran. No new finalization queue is introduced.

### Timeout uses the same atomic terminal transaction

[`Server.TimeoutOverdueRuns`](../../internal/api/timeout.go) previously marked a
run timed out, then released its thread in a separate store call. If that second
write failed, later sweeps skipped the already terminal run and its thread could
stay busy. The separate path also bypassed the completion generation and the
shared usage/hold finalization contract.

The sweep now calls `completeRunResult` in
[`completion.go`](../../internal/api/completion.go). The existing store completion
transaction commits timeout and thread release together. The trusted
`CancelCurrentAttempt` option now covers timeout as well as cancellation;
[`ResolveCompletion`](../../internal/state/completion.go) resolves the live
durable attempt under the transaction lock. A concurrent replacement cannot be
given output captured from an older observed generation. Runners still supply
their own exact generation and cannot opt into this trusted control-plane behavior.

Only delivered usage from the captured attempt can become timeout output. If
reclaim changed that attempt, its captured usage is dropped. Partial messages
and arbitrary values never replace committed conversation history. With no other
active run on the thread, its busy state becomes `idle` while retaining its prior
values. The run records `timeout` and may contain the matching attempt's usage.
This is observed token accounting, not an assertion that external work or spend
stopped immediately.

The same idempotent finalization handles usage, holds, broker closure and the
existing timeout error hook. A failure after commit retains the lease and tries
to signal the runner, allowing ordinary reclaim to retry finalization. The sweep
returns per-run errors rather than hiding incomplete bookkeeping. Its count and
transition metrics include only newly applied timeouts; retrying a committed
result does not increment them again. The public `CompleteRun` signature stays
unchanged; its private helper exposes the existing transaction's `Applied` flag
to the sweep rather than adding another terminal-state mechanism.

### Register tool-hook observation before dispatch

[`watchRunEventsForToolCallHook`](../../internal/api/runs.go) previously started
its entire subscription in a goroutine. A fast runner could publish an explicit
`tool_call` event before that goroutine registered with the broker, so the hook
observer missed an event that normal run delivery had already accepted. The
Linux race run exposed this ordering bug; delaying the runner would only hide it.

Run creation now attempts the broker subscription synchronously before returning
an assignment for enqueue. Once registration succeeds, only event consumption
and sink dispatch run asynchronously. Subscription failure is logged and run
creation continues. A cancellable two-hour context bounds an orphaned watcher;
normal stream completion cancels it sooner and releases the underlying broker
subscription. This reuses the existing broker instead of adding replay or another
hook queue. Only explicit runner `tool_call` events are observed. Hook sinks
retain their existing bounded queue, retry and drop behavior; this change does
not make tool-hook delivery durable, exactly once or an enforcement boundary.

The regressions are kept at the affected boundaries:

- [Redis queue tests](../../internal/transport/redis/redis_test.go) preserve
  literal poison IDs and the old lease on a failed ready write.
- [Redis/API identity test](../../internal/api/redis_poison_identity_test.go)
  exercises trusted poison completion with real Redis and SQLite, requiring an
  unrelated prefix run in another tenant to remain active.
- [Cancellation finalization tests](../../internal/api/cancel_finalization_test.go)
  fail usage and hold release after the interruption commits, then require
  ordinary reclaim to finish bookkeeping without redispatching agent work.
- [Timeout recovery tests](../../internal/api/timeout_recovery_test.go) cover the
  atomic release, concurrent sweep winner, attempt race and finalization failure.
  The [shared store conformance case](../../internal/state/conformance/completion.go)
  checks matching/stale captured usage, duplicate completion and preserved history.
- [Hook subscription test](../../internal/api/hooks_subscription_test.go) makes
  enqueue fail unless the subscription is already registered, emits tool and
  terminal events immediately, and checks subscription cancellation. The
  assertion tests ordering directly rather than waiting for a scheduling race.

## Isolate the failure exercise

The harness requires three distinct local HTTP replica URLs and an explicit
Compose project prefixed `runkite-ha-`. It verifies the target containers' project
labels before changing services. Only named workers and the named victim plane
are stopped, and restoration runs even after an assertion fails. It never runs
global Docker cleanup or removes databases. The caller owns the disposable
stack and its final teardown.

The optional [recovery overlay](../../docker-compose.recovery.yml) removes
database debug ports and exposes fixed loopback plane/LB ports. Compose must
support its `!reset` and `!override` tags. Fixed host ports matter because Docker
can reassign an automatically allocated port when the same container starts
again. Process restoration targets verified container IDs directly; Compose
dependency expansion must not silently restore the victim plane before the
outage assertion.

The webhook JSONL path is required because sink totals cannot prove per-run hook
uniqueness. Responses, subprocess calls and polling have bounds. Credentials are
read from environment variables and omitted from the report. Test runs should
keep output in the ignored soak output directory or another disposable location.

## Measured rehearsal and evidence boundary

The earlier 2026-10-31 rehearsal built the Go and Python images from their Dockerfiles
and used three planes, nginx, two Python workers, Postgres and Redis. A disposable
overlay enabled client API-key auth and the runner kind's `default` tenant
allowlist, without the insecure-serve bypass. The checked-in local demo still
uses its documented open-client convention; the measurement overlay is not a
new production deployment template.

The configured 120-second steady phase used eight Python and six JavaScript
workers. The Python loader completed 163 successful create/wait cycles and the
JavaScript loader 131, with no loader errors. These are loader counts, not a
subtraction from a pre-populated database's cumulative run count. Scheduled cron
traffic is separate.

A controlled fixture regression reproduced a cron-shaped run failing under the
old direct runner setting. The corrected proxy setting completed the run. The
retained error event identifies the native guard; a stored generic error string
alone need not contain the full diagnostic.

All four checks passed in the final bounded recovery run: concurrent cached
completion with no additional synthetic usage, cross-replica cancellation,
runner-plus-plane crash/reclaim, and retained raw SSE replay. The victim plane
was verified stopped both after the workers restarted and after reclaimed
durable success. That run completed at a newer attempt generation with a
matching completion generation. The final `recovery-report.json` retains each
case's assertions and evidence; the delivery manifest records commands, hashes
and exclusions. Earlier failed harness rehearsals remain separate artifacts.

This rehearsal does not meet the separate 30-minute announce bar. It is not a
cloud, Kubernetes, multi-day, Postgres-failure or Redis-failure soak. It does not
test real model providers, universal SDK HA, managed UI reconnect across planes,
or exactly-once external writes. Earlier single-plane SDK integration evidence
retains its original scope.

## Follow-up failure probes

The follow-up based on `7a198e9` rebuilt the Go image with the five fixes above
and repeated the four bounded checks against three planes, two Python workers,
Postgres and Redis. Cache retries, cross-replica cancellation, worker-plus-plane
crash/reclaim and retained raw SSE replay all passed. The previous 120-second,
294-cycle steady-load measurement was not rerun for this patch and remains the
earlier rehearsal's result.

Two separate task-local probes used a scripted Runner Protocol client, with no
model or tool execution:

- Redis was paused after dispatch and partial event delivery, retaining its
  memory. Stream acknowledgement failed during the outage. After unpause, the
  same run reclaimed at a newer generation, rejected stale completion and
  completed with one synthetic 20/4 usage entry and preserved prior history.
- Postgres was paused after its runner events had been acknowledged. Completion
  acknowledgement failed and the queue lease remained. Heartbeats continued
  through Redis; after unpause, the same-generation completion retry succeeded
  with one result, one synthetic 20/4 usage entry and preserved prior history.

Both probes observed `/readyz` return 503 while `/livez` returned 200. They
restored the same containers and retained state. These six bounded checks all
passed on the final image; the delivery evidence retains the separate raw case
reports and scoped commands. The Linux `-race` suite for `./internal/...` and
`./cmd` passed, and the focused hook-registration regression passed 20
consecutive runs. That hook result is a unit ordering proof, not a live-provider
or delivery guarantee.

The scripted probes do not establish that every framework succeeds through an
arbitrary backend outage. For example, a Postgres outage during a LangGraph
proxy checkpoint write can fail that execution; the Postgres probe isolates
completion after acknowledged events. Pausing Redis preserves memory and does
not test Redis data loss, snapshot recovery or a replacement Redis instance.
Neither case proves a cloud failover, a 30-minute or multi-day soak, managed UI
reconnect across replicas, live-provider accounting or exactly-once external
effects. Reports from earlier source images retain their separate scope.
