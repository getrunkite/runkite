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

The 2026-10-31 rehearsal built the Go and Python images from their Dockerfiles
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
