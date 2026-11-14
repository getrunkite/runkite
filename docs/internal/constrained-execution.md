# Execution pools and an external workload boundary

Status: v0.7.0 candidate source (local, not published). This note distinguishes implemented
fleet routing, a rendered deployment profile and an experimental external bridge.
It does not declare OpenShell or a Kubernetes CNI qualified merely because their
configuration exists. No runtime is embedded in the Go binary.

## Two different callers

A platform operator can assign private tenant/agent work to a separately
credentialed fleet while continuing to use existing SDK workers. A different
operator needs to run arbitrary generated code without giving that code the
trusted worker's runner credential. Pool routing serves the first caller. The
external process bridge is a prototype for the second.

Shared kind tokens, exact generations, tool approvals and tenant-scoped HTTP
handlers already exist. They did not determine which same-framework fleet
receives input, nor prevent in-process code from reading the worker environment.
Those are distinct boundaries and must not be represented as one feature.

## Reuse exact queues rather than add another scheduler

`internal/execution.Catalog` maps operator-defined pool names to a framework kind
and explicit tenant and agent sets. The pair is the cross-product of those sets;
there is no wildcard, default-allow membership or caller-supplied pool authority.
Config uses `execution_pools` and file-level `execution_pool` alongside the
existing framework `runner_kind`. Bootstrap validates graph membership and
framework compatibility, then stores `pool-<name>` as the effective queue kind.
It retains the original `framework_kind` and selected pool in agent metadata.

The existing `JobQueue.Dequeue(kind)` and opaque `assignment_json` remain intact.
No second queue implementation or scheduler is added. All discovered definitions
must agree exactly. Unknown reserved pool kinds and missing pool credentials fail
closed. Pool names use lowercase ASCII letters/digits/hyphens, at most 31 bytes.

Each pool needs its own `RUNNER_TOKEN_POOL_<NAME>` credential or rotation
allowlist. Reusing any pool credential under a different pool or framework kind
fails startup. The catalog supplies the pool's exact tenant allowlist; a broader
`RUNNER_TENANTS_*` cannot widen it. This is independently authenticated fleet
authority, not a unique pod identity, SPIFFE identity or hardware attestation.
The same pool's workers remain trusted with all work allowed to that pool.

`createRunCtx` refuses unauthorized tenant/agent routing before thread creation,
cache results, connector pre-warm or enqueue. Agent metadata cannot move an
explicitly pool-scoped tenant/agent back onto an unrestricted framework queue.
The bridge independently checks kind and tenant/agent membership before returning
input or advancing an attempt. A misrouted assignment is Nacked at its original
generation without exposing its body. Subsequent gRPC and run-bound HTTP requests
repeat the scope check. A peer's pool credential is never an alias for this pool.

Pool HTTP access also removes legacy unbound metadata privileges. An active
assignment is required for data paths and exact-run status; the status path must
match the bound run ID. Outside a run, only schema publication for an explicitly
allowed agent is available. Broad connector/cron discovery and foreign run status
are refused. The ordinary trusted-fleet surface remains unchanged for agents
outside the configured pool scope.

Enable pools only after upgrading every HTTP/gRPC replica. Older binaries do not
acquire these scope checks because they can parse the kind string. Drain existing
unpooled assignments for a migrating agent, configure the same catalog and
independent credentials on every replica, then start its proxy-only pool fleet
and verify denied scopes before admitting private work. Configuration is a
restart-time snapshot; this is not a hot-reload or mixed-version isolation promise.

Python adapter CLIs and the TypeScript CLI accept `--execution-pool` or
`RUNKITE_EXECUTION_POOL`. This selects queue identity without changing the SDK
adapter. Pre-execution status checks include the exact run/generation. Pooled
workers refuse execution on an unavailable, denied or malformed status response;
ordinary workers retain their existing fail-open status behavior.
Pooled worker startup rejects known control-plane backend environment variables
and requires proxy mode. This does not detect arbitrary renamed credentials,
application secrets or unsafe operator mounts; those remain deployment duties.

## The Helm profile does not verify its CNI

The original optional NetworkPolicy still restricts ingress only. The separate
opt-in `networkPolicy.egress.enabled` profile restricts both roles' outbound
traffic. Each gets configured resolver pods on TCP/UDP 53. Runners also get this
release's plane HTTP/gRPC ports. Operators supply exact model/telemetry entries
for workers and DB/Redis/issuer/connector/webhook/collector entries for the plane.
No general internet or runner database exception is inserted automatically.

Egress mode requires `runner.proxyOnly`, as do named pools in this chart. The
runner then omits `POSTGRES_DSN`; explicit backend URLs in `runner.extraEnv` are
rejected. Pool/egress runner pods do not automount a service-account token.
The plane keeps its own required backend configuration. A compromised trusted
worker still has its fleet credential, so this profile does not isolate mutually
hostile code inside that worker.

NetworkPolicy requires an enforcing CNI. DNS rules are pod/namespace selectors,
not hostname allowlists. Extra policies in the cluster can add permission, and
host networking or an unsupported CNI can change enforcement. Rendering proves
manifest shape only. Qualification must first establish that allowed and blocked
fixture destinations are reachable without the policy, then verify DNS, plane,
model and telemetry paths remain allowed while unlisted destinations fail.
Metadata-service denial needs an independently reachable fixture/control, not a
timeout against an address that was unreachable before the test.

`scripts/qualify-execution-egress.py` applies only the chart's egress policies to
its own randomly named namespace on an explicitly selected disposable context.
It proves fixture reachability before applying policy, then checks resolver,
plane ports, model, telemetry, backend isolation and unlisted destinations. It
does not install or attest a CNI, mutate cluster policy, or claim that an actual
cloud metadata service was exercised. Invocation requires explicit opt-in:

```sh
RUNKITE_TEST_EXECUTION_EGRESS=1 python scripts/qualify-execution-egress.py \
  --context disposable-test-context --image your-approved-python-fixture-image \
  --output /path/to/egress-evidence.json
```

The local qualification passed all 10 recorded cases on kind 0.33.0 with
Kubernetes 1.37.0 and its default kindnet CNI. All five HTTP fixture destinations
were reachable before the chart policies were applied. Afterward, plane HTTP and
gRPC, DNS, model and telemetry paths remained available; runner database access
and unlisted runner/plane destinations were denied. The owned cluster was then
deleted. The retained report is
`outputs/runkite-security-review/egress-qualification/evidence.json` in the local
qualification workspace, not a fixture result generated by unit tests.

This qualifies that disposable cluster and chart profile. It does not establish
the behavior of a different CNI, cloud metadata service, host-networked pod,
additional cluster policy or live provider. Repeat the baseline and denial checks
on the intended deployment rather than treating the version numbers as a general
compatibility guarantee.

## A trusted worker outside an external sandbox

`python/runkite_runner/isolated.py` is an experimental managed adapter using the
external OpenShell 0.1.8 CLI. Its gateway endpoint is explicit and HTTPS, except
literal-loopback test fixtures. It pins an immutable image reference, command,
provider names, workspace and configured policy digest. It does not import a
framework module on the trusted worker. It starts a fresh named sandbox for the
attempt and uses non-TTY, non-login-shell exec for a bounded JSONL channel.

Construction reads and hashes the policy bytes once. Launch writes those same
bytes into a private temporary directory and a mode-0600 file for the CLI, then
removes that snapshot after the management command returns. Changing the original
policy file after construction cannot change the bytes submitted under the
captured descriptor. This binds the submitted configuration; it does not attest
runtime enforcement.

Only input, prior opaque state and safe run identifiers cross that channel.
Assignment config, user claims, manifests and environment do not. Application
input/state still contain the data the workload must process; callers must not
put bootstrap credentials in them. Provider names refer to existing external
runtime integrations. The bridge does not auto-create providers, upload host
files, mount sockets, copy a vault or send management credentials to the workload.

The child can emit incremental events, request one run-bound MCP call at a time,
and return status/state/output. The trusted parent constructs event identities
and tool proxy binding. Child custom events are labeled as workload-supplied.
Duplicate keys, duplicate call IDs, unexpected shapes, oversized frames and
output floods are refused. Configured `allowed_tools` also restricts bridge tool
requests; it does not govern arbitrary local computation in the child. A pending
MCP response remains a hold; the bridge does not approve, poll or replay it.

This experimental JSONL protocol has no model/token-usage frame or supported
spend-settlement contract. It does not extract provider usage from arbitrary
workload output. A zero recorded in Admin is not evidence that no model call or
cost occurred. Managed admission still applies, but token/USD caps cannot account
for unreported workload usage. Use external provider accounting and limits.
Existing native SDK adapters' usage reporting is independent and is not
inherited by an explicit JSONL workload.

The runtime supplies filesystem/process/network restrictions. Runkite supplies
business-tool policy and managed authority. Either side's denial remains a denial.
The example requires Landlock `hard_requirement`; it must not silently fall back
to a policy that permits running without those filesystem rules. Inspect the
effective runtime configuration and test it. A configuration hash is not proof
that the kernel applied the same policy or that an image implements a claimed
tool correctly.

Official references used for this external prototype are the pinned
[OpenShell architecture](https://docs.nvidia.com/openshell/v0.1.8/about/architecture),
[exec/lifecycle interface](https://docs.nvidia.com/openshell/v0.1.8/how-it-works/sandboxes/overview)
and [policy schema](https://docs.nvidia.com/openshell/v0.1.8/how-it-works/policies/schema).
The checked CLI reports `openshell 0.1.8`; its release archive was verified against
the official SHA-256 manifest. That checksum check alone does not establish
gateway enforcement. Actual runtime qualification is recorded below.

## Durable launch intent is not exactly-once execution

The assignment's additive `run_incarnation_id` is derived from the durable run
insert, not a request header or caller metadata. SQL insertion captures it inside
the transaction; reclaim preserves it. The isolated adapter requires it. It does
not claim support on an older plane or a store that does not supply that proof.

The parent loads versioned opaque state and commits a launch intent with CAS
before asking the runtime to start work. The intent contains the original run
incarnation, generation and owned sandbox name. Terminal state/output and a
receipt are committed with CAS before terminal events or completion are reported.
A committed receipt replays after a delivery failure without another launch.
Deleting/reusing a run ID cannot select a different incarnation's receipt.

An intent without a receipt is uncertain, including a lost runtime-create or
checkpoint acknowledgement. Reclaim refuses another execution. This deliberately
sacrifices automatic recovery when it cannot prove whether external work occurred.
Investigate the owned runtime and effects, confirm cleanup, then use a fresh
thread for an explicitly reconciled operation. There is no unsafe force-clear,
automatic tool replay or generic exactly-once promise. Receipts are capped at
128 turns and one bounded checkpoint; start a new thread before the capacity or
size ceiling. Existing SDK adapters do not inherit this incarnation receipt
contract simply because they can ignore the additive assignment field.

Cleanup requests target only the attempt's generated sandbox name. Delete
acceptance is distinguished from observing that its runtime record disappeared.
The parent emits cleanup evidence separately from agent output, before terminal
delivery when possible. Runtime outage or worker death can leave an owned sandbox
or unconfirmed cleanup; the retained intent/receipt is the investigation handle.
Remote deletion cannot undo an already completed business effect or prove a
provider credential was revoked. A parent-process crash still needs operator
runtime reconciliation; there is no distributed cleanup reconciler in this slice.

The disposable fixture wrapper fails closed on cleanup as well. It lists only
the owned gateway, refuses unexpected sandbox names, requests deletion of owned
workloads, confirms their absence, then tears down and confirms removal of its
Compose infrastructure. An accepted delete is not successful cleanup. An
unconfirmed step makes the wrapper exit nonzero even when the child test passed,
and retains its private runtime directory plus `runtime-cleanup.json` for
operator recovery. It removes generated management files only after confirmed
cleanup. The managed integration test also fails when runtime cleanup is
unconfirmed.

## Verification and remaining qualification

Pool tests cover exact scopes, credential independence, unknown kinds, metadata
routing escape, HTTP binding and misrouted assignment input rejection. Bridge
unit tests use fake runtime processes and cover strict frames, limited input,
CAS receipts, reused run IDs, ambiguous launch, cancellation and duplicate tool
requests. They are protocol tests, not runtime containment evidence.

The offline [example](../../examples/isolated_agent/README.md) includes a baked
forbidden file and negative probes. `scripts/qualify-isolated.py` accepts only an
explicit disposable loopback gateway. Its evidence states which real runtime
observations passed and that its checkpoint backend is a fixture rather than the
plane. An HTTP error response still proves a destination is reachable, so the
network probe counts it as reachability. Socket probes distinguish absence,
permission denial, a visible socket and an unexpected visible file; denial alone
does not prove the host socket was absent. Empty destination arguments do not
produce a network-containment claim.

The installed-wheel managed qualification also passed five lifecycle cases using
a real SQLite plane, independently credentialed pooled worker and external
OpenShell 0.1.8 gateway. The workload required Landlock `hard_requirement`. Its
forbidden-file control was first readable outside the runtime; inside, the
workload could not read it, did not see the host socket or parent bootstrap
environment, and ran as a non-root user. The parent supplied connector authority
to the synthetic MCP target without exposing it to the workload or result stream.

The five recorded cases cover:

1. Admin Threads/Runs, incremental streaming, confined workload and one
   server-injected MCP effect through the installed parent.
2. Opaque SQL state continuity after parent restart and no downstream effect for
   a denied tool.
3. Parent death after a committed SQL receipt, followed by later-generation
   reclaim without another tool effect.
4. Refusal to replay an uncertain launch intent, with stale connector authority
   rejected after reclaim.
5. Managed cancellation retaining interrupted durable status and the owned
   runtime investigation handle.

The local reports are
`outputs/runkite-security-review/isolated-qualification-final/managed-evidence.json`
(`passed: true`) and its `runtime-cleanup.json` (`confirmed: true`). The disposable
Helm egress qualification is separate and described above. These are actual
runtime observations, not fake-runtime unit results or a visual fixture.
The gateway was a disposable loopback fixture. This does not qualify a
production gateway's TLS, certificate rotation or external identity setup.

The external bridge remains experimental and opt-in. This run does not qualify
an unchanged arbitrary SDK, mutually hostile code in a shared trusted worker,
external-workload multi-replica behavior, every runtime/kernel/CNI combination,
cloud metadata service or live model/provider. Repeat the relevant qualification
on the intended deployment. Keep each unpassed property explicit rather than
inferring it from a configuration hash, rendered policy or this bounded run.
