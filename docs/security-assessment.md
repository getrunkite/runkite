# Security assessment and independent reviewer handoff

Status: v0.8.0 reviewer handoff for
[RFE #52](https://github.com/getrunkite/runkite/issues/52).
Independent assessment remains **pending**.
Maintainer tests, automated scans and a second engineering agent's review do not
constitute an independent security assessment or certification.

The [identity and distribution plan](internal/identity-and-distribution-plan.md)
tracks source implementation, reproducible fixtures, actual publication and
external acceptance separately. This document gives a reviewer a concrete
starting boundary, rather than claiming that a large feature count proves
security. Report vulnerabilities through [SECURITY.md](../SECURITY.md), not a
public issue containing credentials or customer data.

## Scope and assets

Runkite is a self-hosted control plane. The agent framework owns the model loop
inside its runner; an app using only the decision API retains its own loop.
Protected assets include tenant conversations/checkpoints, runner assignments,
tool authority and approvals, credential lifecycle handles, action evidence,
usage records and export delivery state.

This review should include both published v0.7.0 and v0.8.0 behavior. Record the
exact revision and local changes used for
each result. Do not project a result from one version or backend onto another.

- [RFE #45](https://github.com/getrunkite/runkite/issues/45): installed governance
  helpers and package boundaries. [Distribution design](internal/governance-distribution.md).
- [RFE #48](https://github.com/getrunkite/runkite/issues/48): immutable artifact
  verification and publication recovery. A mocked lookup or existing-artifact
  verification is not evidence of a first trusted OIDC publication.
- [RFE #46](https://github.com/getrunkite/runkite/issues/46): durable cumulative
  isolated usage, unknown versus reported zero, partial reports and terminal
  settlement. Reports remain workload assertions, not provider billing proof.
- [RFE #49](https://github.com/getrunkite/runkite/issues/49): verified workload
  holder and attempt binding. [Identity design](internal/workload-identity.md).
- [RFE #50](https://github.com/getrunkite/runkite/issues/50): registered delegation
  targets and bounded issuer-asserted actors. [Delegation design](internal/delegation-and-audit-correlation.md).
- [RFE #51](https://github.com/getrunkite/runkite/issues/51): external Vault/action
  correlation using nonsecret metadata. It does not provide network discovery
  or continuous outside-plane observation.
- [RFE #47](https://github.com/getrunkite/runkite/issues/47): a combined disposable
  multi-plane, SQL/Redis, TLS and external-worker qualification. Record the
  measured runtime, network implementation, faults and duration.
- [RFE #52](https://github.com/getrunkite/runkite/issues/52): this reproducible
  handoff and the separate, still-pending external reviewer assessment.

## Threat model

An authenticated client may supply adversarial inputs, IDs, run configuration,
JWT claims or tool arguments. A scoped reviewer may guess another tenant's
approval/action IDs or attempt to escalate into global Admin. A runner may be
stale, compromised, or a same-fleet peer that knows another run ID and
generation. An upstream connector, issuer or tool may fail, redirect, return
oversized/malformed content, or reveal sensitive strings in an error. A process
or connection can disappear between any two storage/network operations.

The control-plane process, SQL/broker administration, configured external
issuers, global Admin, Vault provider and registered configuration are trusted
authorities. Compromise of one of those authorities exceeds the worker/client
boundary. A copied private key and certificate identify the same holder; TLS
cannot distinguish their copies. The ordinary SDK worker remains a trusted
process that can access its own credentials. Individual certificate ownership
is not process attestation or hostile-code isolation.

Strict workload identity requires direct verified mTLS on both HTTP and gRPC,
SQL state, Redis/inprocess queues, and an exact URI-to-kind/pool allowlist.
It rejects other backends at startup. The shared HTTP listener also applies
client-certificate requirements to client/Admin callers. Forwarded certificate
headers and direct runner SQL connections are outside this profile. Drain and
replace all replicas before enabling it. Do not mix strict and legacy replicas.

Short-lived certificate expiry is checked on operations and original attempt
bindings. Removing a URI or denying one public-key fingerprint is complete only
after every replica is replaced/drained with the new startup configuration.
Editing a file alone is not live revocation. There is no CRL/OCSP polling or
live Workload API claim. Keep infrastructure clocks synchronized.

Opaque checkpoint, cumulative usage and completion writes check the durable
attempt owner inside their SQL transaction. General KV/vector proxies instead
check owner at request admission, and external dispatch is not a transaction
with the remote service. Previously admitted operations can finish after
reclaim. Treat that as an explicit in-flight-effect boundary when designing
negative tests, rather than claiming instantaneous rollback of all operations.

## Acceptance cases to reproduce

1. **Holder possession and retirement.** Use two externally issued short-lived
   client certificates under one allowed URI and fleet token, with different
   keys. Dispatch to A. B cannot heartbeat, publish, complete, obtain protected
   HTTP authority or borrow a newer generation. Reject missing/unverified TLS,
   CN-only, multiple/unmapped URI SANs, excessive lifetime and expired proof.
   Deny A's SPKI fingerprint in a fresh replica configuration while B remains
   allowed. Verify retirement only after all old replicas/streams are gone.
2. **Atomic durable ownership and recovery.** Same-generation SQL ownership is
   immutable. Peer, absent and expired actors cannot write checkpoints, report
   usage or complete an owned attempt. A higher reclaimed generation can bind
   B. Owned Nack advances generation. Only explicit control-plane recovery can
   acknowledge retained poison work without a live TLS actor, and it still
   cannot acknowledge a different generation.
3. **Tenant and human authority.** Cross-tenant Review IDs/query/cursors do not
   expose data or mutate approvals. Read-only reviewers cannot approve or enter
   Admin. Preserve CSRF, absolute session expiry, distinct cookie purposes,
   requester exclusion, distinct-identity quorum, one-shot consumption, expiry,
   run incarnation and target/policy/tool revision checks.
4. **Constrained delegation.** Reject unregistered or widened audience/resource
   targets, unsupported subject/actor credentials and ambiguous exchange
   responses. Verify configured issuer/audience/expiration/subject when strict
   JWT identity is enabled. Keep issuer assertions separate from verified
   workload key possession, and keep all bearer/actor token values out of
   manifests and action evidence.
5. **Usage and cancellation.** Distinguish unknown from an explicit measured
   zero, partial from complete, and old from current reporting generations.
   Reject reused sequence with changed payload, decreasing totals, invalid
   numbers and reports after terminal completion. Lose acknowledgements,
   reclaim, cancel and replay without charging the same cumulative usage twice.
   Unreported calls remain unmetered; a cooperative report is not a provider
   attestation or a guarantee of hard external spending limits.
6. **Credentials, tools and evidence.** Reclaim/cancel between credential issue
   and dispatch cannot release stale authority. Preserve provider TTL bounds,
   private lifecycle handles and cleanup reconciliation. Verify safe error
   taxonomy without URL credentials/provider bodies. Required audit/export
   intent failures prevent tool dispatch. Retry an evidence delivery ID without
   executing the tool again. A response observation is not proof of a remote
   business effect or exactly-once execution.
7. **Distribution integrity.** Build and import helpers outside the checkout,
   verify archive/runtime contents and independent version domains, and reject
   an existing registry version with different artifacts. Registry outage,
   authorization failure and explicit missing version must remain distinct.
   No qualification command should publish or require real provider secrets.

Principal entry points are `internal/auth/`, `internal/bridge/`,
`internal/transport/`, `internal/state/sqlcompletion/`, `internal/api/`,
`internal/connector/` and `internal/credentials/`. The specific regressions live
in `workload*_test.go`, `strict_identity*_test.go`, `token_exchange_test.go`,
`credential_identity_reconcile_test.go`, `run_usage_test.go` and shared SQL/queue
conformance suites. Python TLS/heartbeat/status/isolated usage and the TypeScript
runner suites exercise the corresponding client behavior.

## Reproducible maintainer entrypoint

Install the pinned existing project dependencies first. No install, registry
write, paid provider or external identity service is invoked by this entrypoint:

```sh
RUNKITE_TEST_PYTHON=/absolute/path/to/the/test/venv/bin/python \
  scripts/test-identity-security.sh
```

Set `RUNKITE_TEST_GO` or `RUNKITE_TEST_NODE` when using task-local toolchains.
The Go module cache must be populated; downloads are disabled. The command
uses an environment allowlist for test subprocesses and ignores ordinary
database/provider variables. It runs adversarial Go plus SQLite gates, Python
identity/client/isolated usage checks, offline publication-recovery tests, and
the complete TypeScript runner suite with type checking. It requires local
loopback sockets for real TLS and HTTP fixtures. A restricted execution sandbox
that forbids sockets cannot establish those cases.
Optional SDK subprocess tests in that suite still report skips unless their
explicit binary flags are supplied. This entrypoint does not treat those skips
as packaged-worker qualification; the manual gate below requires its separate
measured report.

Live storage qualification must use a disposable dedicated Postgres database
whose name starts `runkite_identity_`, and a disposable dedicated Redis instance.
Never point this at production, a shared developer database or a shared Redis
database. The script validates loopback endpoints and requires an explicit
operator acknowledgement; those checks cannot prove that infrastructure is
actually disposable.

```sh
export RUNKITE_TEST_IDENTITY_DISPOSABLE=1
export RUNKITE_TEST_IDENTITY_POSTGRES_DSN='postgres://fixture:fixture@127.0.0.1:15432/runkite_identity_review?sslmode=disable'
export RUNKITE_TEST_IDENTITY_REDIS_URL='redis://127.0.0.1:16379/0'
scripts/test-identity-security.sh --live
```

The live phase additionally runs Postgres state, cross-replica Redis queue and
API tests. It does not automatically provision or delete services. Native Linux
with a C compiler can add `--race-linux` for Go race instrumentation. This is
not a cross-compiled Mac race result. MySQL qualification remains a separate
existing store gate; this entrypoint does not silently assume a MySQL service.

Each invocation retains `qualification.log` and `result.json` in a unique
directory below `RUNKITE_TEST_IDENTITY_OUTPUT` or the temporary directory.
Metadata records the revision, dirty-tree flag, selected profiles and result.
Retain the exact source tree with the evidence if it was dirty. A failing gate
or failed result write fails the command. Synthetic fixture logs may include
synthetic arguments. Do not substitute customer data or real credentials.

The external OpenShell/runtime/CNI and two-real-plane soak remain separate
explicit fixtures. Use their documented owned-infrastructure wrappers and
retain their actual evidence. A passing local unit suite does not qualify an
external runtime, another CNI, a different TLS proxy or an untested deployment.

## Manual combined qualification in GitHub Actions

`.github/workflows/identity-qualification.yml` provides an explicit
`workflow_dispatch` gate with one through 20 failover/partial-usage cycles. It
builds the checked-out candidate Go binary and Python wheel, checks artifact
hashes, and invokes `scripts/test-isolated-ha.sh` on native Linux amd64. It uses
Docker-owned disposable Postgres/Redis, two real Runkite mTLS replicas, an
installed parent worker and the external OpenShell profile. The external
gateway must also provide measured mTLS evidence; plaintext fallback is refused.
Qualification alone does not establish a release or registry publication.

The CLI is pinned to the official OpenShell v0.1.8 Linux archive and its
[published SHA-256](https://github.com/NVIDIA/OpenShell/releases/expanded_assets/v0.1.8).
The workflow uses pinned existing checkout/setup/artifact actions and uv
0.12.23, with read-only repository permissions. It requires public dependency
and container downloads, but no cloud, registry-publishing or model keys.
It does not publish Runkite artifacts or alter a remote deployment.

The host must provide Docker Engine 28 or newer, Compose and Landlock. The
workflow checks host prerequisites and executes the actual `hard_requirement`
policy. Unsupported hosts or runtime failures fail the job; they do not become
skipped qualification success. The real fixture's passing evidence, requested
cycle count and confirmed cleanup are required before success. A configured
workflow is not evidence that its GitHub runner has passed. Record the actual
run URL, source revision, versions, elapsed time, faults and retained outputs
when it is first executed.

Run local qualification against the local Unix Docker socket selected by
`RUNKITE_TEST_DOCKER_SOCKET` (default `/var/run/docker.sock`). The wrapper pins
both backends and runtime infrastructure to that endpoint and rejects remote
hosts, conflicting `DOCKER_HOST` values and nondefault `DOCKER_CONTEXT` values
before creating containers. Native Linux management uses a loopback-bound
gateway and host-network supervisor; macOS keeps the bridge management profile.
The workload's isolated network policy is the same in both. See
[the platform rationale and failure evidence](internal/constrained-execution.md#local-engine-networking)
when diagnosing a supervisor that cannot reach its gateway. Do not fix that
failure by disabling TLS or exposing the gateway on every host interface.

The job has a 45-minute bound and does not cancel an older run on the same ref.
An inner timeout leaves time for owned cleanup and artifact collection. Safe
JSON/text evidence is retained for seven days with an `always()` upload step;
private recovery directories, certificate/key files and provider handles are
excluded. Runner loss can still prevent artifact collection, so missing cleanup
or evidence must remain unconfirmed. This manual gate complements ordinary PR
tests. It does not establish independent assessment, production soak, another
CNI/runtime version or universal SDK isolation.

The same manual workflow separately packs the current TypeScript runner,
installs its actual npm archive into a fresh temporary consumer, and runs a
scripted AI SDK model through a real strict-mTLS SQL plane. It checks that a
different key under the same URI cannot heartbeat, complete or use protected
HTTP paths, and that native conversation state survives a second managed turn.
It records this distinct single-plane SQLite/inprocess `dev` fixture profile in
`typescript-workload.json`, with safe plane/worker logs alongside it. Strict
mTLS and ownership checks remain enabled; this does not claim a production HA
deployment from the single-plane fixture.
No external model call is used. For a local prepared binary and built runner:

```sh
cd typescript/runkite-runner
RUNKITE_TEST_WORKLOAD_BINARY=/absolute/path/to/runkite \
  node --import tsx --test src/workloadIdentity.e2e.test.ts
```

The test uses a public-registry npm install with its own empty npm configuration
and no inherited publishing credentials. Set `NPM_CONFIG_CACHE` for a prepared
cache, or `RUNKITE_TEST_WORKLOAD_NPM_OFFLINE=1` to require all dependencies
already cached. It is opt-in outside the manual gate and does not establish a
TypeScript multi-replica or live-provider result.

## UI layout qualification with synthetic records

The developer-only preview serves the actual built `internal/adminui/dist`
bundle with explicit read-only synthetic responses. It runs no agent and
connects to no Vault, issuer, database, queue or production backend. Every page
shows **Synthetic UI fixture, no agent execution**. This is layout evidence,
not API, authentication, runtime or backend qualification.

```sh
# Build the actual frontend for the revision under review first.
cd admin-ui && npm run build && cd ..
python3 scripts/preview-security-ui.py --port 8786
```

Open `http://127.0.0.1:8786/` in a clean browser context without existing
loopback cookies. The fixture refuses credential-bearing requests and never
creates an authenticated session. Its catalog links to separate delegated
identity and Vault correlation actions, a provider-confirmed lease cleanup,
an unknown action outcome, and isolated usage with partial, unknown and
explicit complete-zero reports. Separate connector fixtures avoid implying
that one authentication configuration uses both token exchange and Vault
database credentials.

The listener is fixed to `127.0.0.1`; no public-host option or backend proxy
exists. Only GET/HEAD work. Mutations return 405, unknown API routes return
404, responses are not cached, and the injected preview banner leaves the
built JavaScript/CSS unchanged. The script does not alter the public site's
unavailable Admin link or publish a live demo. Stop it with Ctrl-C.

`python3 scripts/test_preview_security_ui.py -v` checks routing, metadata,
traversal/symlink refusal and mutation/credential rejection without opening a
socket. Record separate browser evidence for the new layouts at desktop and
mobile widths, including long identity/revision strings and incomplete usage.
A passing router test is not proof that the browser rendered those layouts.

## Handoff and independent acceptance

Give the reviewer the exact source/version, build and lockfile hashes, threat
model, enabled feature configuration without secrets, schema upgrade procedure,
commands, test logs, host/runtime/architecture versions and all failed or skipped
cases. Include observed publication provenance separately from offline package
tests, and measured multi-plane/runtime evidence separately from unit fixtures.
The [v0.7.0 qualification record](internal/security-completion-qualification.md)
is historical evidence for that release, not certification of these additions.

Agree the review scope and reporting channel with a reviewer who did not author
the implementation. Track findings through a fix, regression case and reviewer
retest. Record any accepted risk with its deployment consequence. Only then
publish the reviewer's actual scope, revision, methodology, date and permitted
report. Keep independent assessment pending until that reviewer has completed
the agreed work. Keep broader production soak and untested-provider claims
pending even after a bounded independent review.
