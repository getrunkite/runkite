# Security completion qualification

Status: local release-candidate source after v0.6.1, qualified on 2026-11-14.
This records measured evidence and rollout requirements. It is not a claim that
every configuration, provider or possible failure has been tested.

## What changed and why

The existing managed run, connector and Review paths remain the foundation.
The changes close five specific gaps without adding another scheduler, policy
service, credential vault or embedded runtime:

- [Approval authority and quorum](approval-authority-and-quorum.md) records
  distinct verified reviewer identities and optional requester exclusion.
  Compatibility display identities cannot become strict approval authority
  through a session round trip or delegated run.
- [Configured target binding](target-bound-approvals.md) now works with effective
  policy revisions and opt-in observed MCP tool definitions. Exact raw schema
  numbers and unknown fields survive streamable HTTP discovery. A configuration
  or schema hash does not attest the remote implementation.
- [Credential lifecycle](credential-lifecycle.md) registers an external Vault
  database issuer and trusted MCP consumer. Issuance, use and cleanup retain
  original ownership, actual granted TTL and uncertain outcomes. Secret values
  are not a new database of credentials in Runkite.
- [Durable action delivery](action-delivery-outbox.md) commits metadata export
  intent with action events. Fenced delivery retries cannot execute a tool.
- [Constrained execution](constrained-execution.md) separates pool routing,
  deployment egress policy and an experimental external workload bridge.
  A trusted SDK worker and a hostile workload are different trust boundaries.

Review also fixed issues found while assembling these paths: detached credential
operations lost the tenant needed by Postgres RLS; SDK-decoded schemas rounded
large JSON numbers; a mutable policy file could differ from its receipt digest;
and qualification cleanup could report success without proving disappearance.
The runtime fixture now checks the real external name and networking contracts.

Container checks found fixable Debian zlib advisories in the upstream Python
base image. Both Debian runner builds now refresh vendor packages, following the
same rationale as the existing Alpine control-plane refresh. This does not
replace rebuilding and scanning images for each release. See Debian's
[first advisory record](https://security-tracker.debian.org/tracker/CVE-2026-76844)
and [second advisory record](https://security-tracker.debian.org/tracker/CVE-2026-85091).
The compiled TypeScript CLI also gained config-free help and strict concurrency
validation after a real image smoke test exposed the missing help behavior.

## Qualified behavior

- The complete Go suite passed with the Linux race detector. Live Postgres,
  MySQL and SQLite storage suites passed, including migrations, approval votes,
  outbox claims, run insertion receipts and Postgres RLS. The live API suite used
  Postgres and Redis. `golangci-lint` reported zero issues.
- The two-process Review/action-evidence qualification passed on disposable
  Postgres and Redis, including cross-replica review, process-loss recovery and
  connector target drift. This is a bounded scenario, not an indefinite soak.
- A real Vault database engine issued PostgreSQL credentials to the external
  MCP consumer fixture. Read-only access, provider TTL, renewal, confirmed
  revocation and lost issuance/revocation acknowledgements were exercised.
  API authorization and failure paths have separate synthetic-provider tests.
- All four packaged Python managed SDK profiles passed lifecycle and native
  chat-bridge tests on SQLite and Postgres/Redis: 16 profile/backend combinations.
  Each consumer installed the current wheel in a fresh environment and checked
  its module bytes and entrypoints. Models and business data were synthetic.
- Managed AI SDK tests passed on SQLite and Postgres/Redis. Runner unit tests,
  the 116-test chat bridge, governance helper/example checks and Python core
  checks passed. Package artifact tests checked the new isolated entrypoint.
- The installed isolated worker passed five lifecycle scenarios against a real
  SQLite plane, pooled worker and OpenShell 0.1.8. These cover streaming, real
  connector effects, restart continuity, committed-receipt reclaim without a
  second effect, uncertain launch, stale authority and cancellation. The baked
  forbidden file was readable outside confinement and denied inside it; the
  workload was non-root and its Docker socket was explicitly absent. Owned
  workload and gateway cleanup were confirmed.
- A separate disposable kind 0.33.0 / Kubernetes 1.37.0 default-kindnet cluster
  passed ten chart egress checks. Baseline destinations were reachable before
  policy. Required DNS, plane HTTP/gRPC, model and telemetry paths stayed
  reachable; runner database and unlisted destinations were denied. The owned
  cluster was removed after the probes.
- Admin tests, type checking and production build passed. Browser checks used
  clearly labeled synthetic records at desktop and mobile sizes. A partial
  review stayed pending, credential pages showed metadata and Review omitted
  privileged lifecycle controls. This visual fixture is not backend auth proof.
- OpenAPI covers all 167 Go routes and has 26 contract checks, including typed
  manifest provenance and private-field exclusion. Public HTML and Markdown
  destinations were checked separately.

## Reproduce the relevant gates

Run `go test -race ./...` on a supported Linux toolchain. Supply dedicated test
backend URLs for the live suites; an unset URL is a skip, not a live pass.
Use `scripts/test-review-ha.sh` for the owned two-process fixture. Follow the
credential example's opt-in live test instructions and the constrained-execution
note's explicit disposable-runtime/CNI commands. Never point these fixtures at
an application database or a shared production runtime.

`make test-python`, `make test-ts`, `make test-python-package`, the four managed
Python targets, `make test-managed-chat-bridge`, `make test-managed-ai-sdk`,
`make test-site` and the OpenAPI checks provide the corresponding language and
contract gates. Managed fixtures install fresh consumers; they must not silently
resolve adapters from an old checkout. The Python isolated tests use unittest
discovery so a successful command cannot mean that zero tests were collected.

## Release and operator gates

The three container builds completed locally. The initial control-plane scan
passed the CI policy of no fixable HIGH/CRITICAL findings. After the runner
vendor-package refresh, the local Docker engine became unresponsive before the
final runner-image scans and TypeScript image help smoke could start. Those
final image checks are pending, not passing, and must complete before publishing.
A local test record also does not establish remote CI or package publication.

The version remains v0.6.1 until the coordinated release cut. No existing tag or
published artifact is rewritten. New configuration is opt-in and the external
bridge remains experimental, even though its bounded fixture now runs.

Before upgrade, verify a backup and stop or drain every writer. Upgrade through
Postgres/SQLite schema 21 or MySQL schema 22, replace every HTTP/runner replica
and then admit traffic. Do not mix older writers with these authority checks.
Historical managed approvals need fresh review. Requester exclusion additionally
needs a newly captured verified principal; legacy session and run records cannot
invent that proof. See the [upgrade procedure](../ops-runbook.md#security-completion-upgrade).

Retain retired issuer bindings and cleanup keys until their leases are resolved.
Retain or drain original export destinations before changing their URL identity.
An unknown issuance or business effect is an investigation, never permission to
replay it. Rollback requires a reviewed backup and external-effect/lease
reconciliation, rather than starting an older binary against the new schema.

## Explicit limits

Qualification did not exercise paid/live model providers, every cloud/CNI,
actual cloud metadata endpoints, mutually hostile code in one trusted SDK
worker, every CPU architecture or a long-duration external-runtime HA soak.
The isolated JSONL profile has no model-usage settlement contract. Zero recorded
spend there does not establish free execution or complete provider billing.

Distinct subjects do not prove distinct humans. NetworkPolicy needs an enforcing
CNI. A runtime policy digest does not attest the kernel or image. Revoking a
database credential need not terminate every established session. Export is
at least once and can reorder. Cancellation cannot undo committed side effects.
These boundaries remain part of the product contract after release.
