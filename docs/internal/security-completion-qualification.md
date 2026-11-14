# Security completion qualification

Status: local v0.7.0 candidate (not published), qualified on 2026-11-14.
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

The TypeScript image scan additionally found vulnerable packages in global npm
and native development compilers. They are unnecessary for agent execution.
Production-only installs now retain local `tsx` and share the example's locked
AI SDK dependency, while global npm/npx and development compilers are absent.
Custom agent dependencies belong in a matching Node builder stage. Contributor
package manifests and locks are unchanged. CI now builds, exercises and scans
all three images instead of scanning only the control plane.

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

For the built TypeScript image, run the offline native probe explicitly:

```sh
RUNKITE_TEST_RUNNER_IMAGE=1 python3 scripts/qualify-runner-image.py \
  --image your-built-runner-image --output runner-image-qualification.json
```

It resolves an existing local image to its immutable ID, never pulls implicitly,
and removes only its uniquely named test container. A failed or uncertain cleanup
fails the command and its evidence, even if native execution passed. Four
fault-injection tests guard these reporting and cleanup paths without Docker.

## Release and operator gates

All recorded local security-implementation gates passed. The three final Linux/arm64 images
built and passed Trivy 0.72.0 vulnerability scans with
`--severity HIGH,CRITICAL --ignore-unfixed --scanners vuln`: zero fixable findings
at those severities. This is not a claim of zero vulnerabilities at every
severity or complete secret scanning. Repeating the CI command with its default
vulnerability and secret scanners also passed for all three images, with no
reported HIGH/CRITICAL secret findings. The exact tested local image IDs are:

- Control plane: `sha256:7cfffa9544c3f58fa0552dbc93a7268c5ebb94de8a356dc605bbe4be3b5d9958`.
- Python runner: `sha256:949ddbc9d237ca6b7de2247287d4ba66c7deec0a27886dcb971f8c3a62a8b7f4`.
- TypeScript runner: `sha256:49bfb30a25dfff0115f36bdc055ebb9ade3c7bbe64a497e42b1f8dfb02576df7`.

Control-plane version and non-root Python runner help checks passed. The final
TypeScript image ran with networking disabled and a read-only root filesystem:
non-root identity, absent npm/npx and development compilers, local TypeScript
loading, config-free help, native LangGraph echo and a two-step AI SDK model/tool
stream with usage all passed. This is an offline dependency-pruning regression,
separate from the managed control-plane qualification above.

Docker was responding again when the approved recovery was attempted, so no
restart was necessary. Owned SQL fixtures were removed, and the initial runtime
recovery directory's generated keys were removed after confirming that its
containers and network were absent. Final runtime/CNI cleanup remains confirmed.
A local test record does not establish remote CI or package publication. Rebuild
and repeat the relevant gates for the tagged release and its target platforms.

Version metadata now targets v0.7.0 locally. This candidate is not tagged or
published; v0.6.1 remains the published release. The image IDs above identify the
actual tested artifacts before the final versioned release build. Rebuild and
repeat the relevant artifact gates for the candidate/tag instead of treating old
image or wheel identifiers as newly published v0.7.0 artifacts. No existing tag
or published artifact is rewritten. New configuration is opt-in and the external
bridge remains experimental, even though its bounded fixture now runs.

Candidate Helm deployment requires matching locally built plane and runner
images. Its `image.tag` and `runner.image.tag` defaults remain `latest`, which
does not select an unpublished candidate. Override both deliberately and follow
the [release preparation guide](../releasing.md).

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
