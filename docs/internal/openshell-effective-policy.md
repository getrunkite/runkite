# Observe effective OpenShell policy around a qualification workload

Status: Unreleased checkout qualification after v0.9.0. The external JSONL
profile remains experimental. This work changes the disposable qualifier, not
the production worker, authorization, checkpoint CAS, RPCs or Admin behavior.

The caller is an operator or reviewer who needs more than the authored policy
file's hash. An external gateway can return a different effective configuration,
or a revision that has not loaded. The qualifier now compares a captured effective
policy with the original configured boundary and checks the observed loaded
revision around real fixture execution. It does not install a production gate or
make a runtime attestation.

## Existing behavior and pinned upstream contract

`python/runkite_runner/isolated.py:OpenShellRuntime` already snapshots the
authored policy bytes, binds their SHA-256 into its configured descriptor and
submits that private snapshot to a fresh owned sandbox. `verify` checks the CLI
release. Those production paths are unchanged. Immutable submitted bytes do not
alone establish which effective policy the gateway used or what the kernel
enforced. Existing launch receipts, uncertain-intent refusal, cleanup and
[durable usage reports](isolated-usage.md) remain independent.

Both official release tools are pinned to OpenShell **v0.1.8**, source
[`f3d976590d3eb1379690a2abaf0f0e26c21d487d`](https://github.com/NVIDIA/OpenShell/tree/f3d976590d3eb1379690a2abaf0f0e26c21d487d).
The tagged release already contains the standalone checker; no newer main
branch or upstream fork is needed. The relevant contracts are the tagged
[CLI output](https://github.com/NVIDIA/OpenShell/blob/f3d976590d3eb1379690a2abaf0f0e26c21d487d/crates/openshell-cli/src/run.rs),
[revision states](https://github.com/NVIDIA/OpenShell/blob/f3d976590d3eb1379690a2abaf0f0e26c21d487d/proto/openshell.proto),
[checker](https://github.com/NVIDIA/OpenShell/blob/f3d976590d3eb1379690a2abaf0f0e26c21d487d/crates/openshell-prover-cli/src/main.rs)
and [policy identity](https://github.com/NVIDIA/OpenShell/blob/f3d976590d3eb1379690a2abaf0f0e26c21d487d/crates/openshell-core/src/policy_identity.rs).

## What the optional qualifier does

`scripts/qualify-isolated.py --prover /absolute/path/to/openshell-prover`
adds `scripts/isolated_policy_qualification.py:PolicyQualification` to its
qualification-only `ObservedRuntime`. With no flag, the existing baseline can
still pass its own controls, but records
`effective_policy: {"passed": false, "status": "not_requested"}`. Baseline
success must not be described as a policy comparison.

For each of the two created fixture incarnations, the optional path:

1. Creates its owned sleeping sandbox, then inspects it before workload exec.
   It uses the explicitly selected gateway and workspace, never global CLI
   discovery: `policy get NAME --full -o json`.
2. Fetches `policy get NAME --rev N --full -o json` for that exact current
   version. It requires a loaded, active revision whose hash and complete policy
   match the current sandbox-scoped effective policy. Pending, failed,
   superseded, missing or malformed revision state cannot pass.
3. Runs the release checker against the complete captured policy and original
   authored boundary: `openshell-prover check candidate.json --boundary
   boundary.yaml -o json --timeout 10s`. Only exit 0 plus a consistent schema-1
   `within_boundary` result with all five expected domains is accepted.
4. Re-reads the current snapshot after checking. Before deleting the sandbox
   after workload probes, it re-reads both current and loaded snapshots and
   requires equality with their earlier observations. Drift fails qualification;
   an inspection failure still permits the owned deletion attempt.

The strict parser refuses unknown response metadata, duplicate JSON keys,
unsupported shapes and inexact/nonfinite numeric values. This first profile
requires a fresh provider-free sandbox policy source with no global policy
composition. Explicit providers, known provider network-rule markers,
provider-composition provenance or unequal current/loaded policies are rejected,
rather than removing contributions to make a comparison pass.
Timeout, nonzero exit, malformed output, unsupported or inconclusive checker
results are failures, not skipped success. Temporary checker input files are
private and removed after use; upstream diagnostic bodies are not copied to the
public summary.

## Keep the identities separate

Evidence records the configured descriptor and authored-byte hash, the captured
candidate's canonical JSON hash, the gateway-reported policy hash/version,
configuration revision, loaded timestamp/provenance hash, checker binary hash,
checker version and covered domains. It does not publish raw policy bodies,
credential handles, checker input paths or management secrets.

The gateway's hash is its canonical policy identity. It is not the SHA-256 of
the authored YAML or the captured JSON; do not compare those unlike values.
`config_revision` is an opaque positive uint64 identity, not a monotonically
increasing counter or a timestamp. Preserve it exactly without JavaScript
floating-point coercion. The checker executable is rehashed around each call
and its release version is checked; operators must first verify their release
archive against the official checksums.

## Reproduce on a disposable local engine

Install the existing Python runner requirements in a prepared environment. Use
Docker Engine 28 or newer with Compose, a supported Landlock-capable Linux
engine, and the checksum-verified official v0.1.8 CLI and prover archives for
your host architecture from the [release assets](https://github.com/NVIDIA/OpenShell/releases/tag/v0.1.8).
Do not use paid providers, an existing application gateway or customer policies.
The fixture builds the existing provider-free workload and uses its pinned
external gateway/sandbox/supervisor images.

From the repository root, choose an unused absolute evidence directory:

```sh
export PYTHONPATH=python
export RUNKITE_TEST_ISOLATED=1
export RUNKITE_TEST_OPENSHELL_PROVER=/absolute/path/to/openshell-prover
export RUNKITE_TEST_POLICY_OUTPUT=/absolute/path/to/new-policy-evidence
mkdir -p "$RUNKITE_TEST_POLICY_OUTPUT"

python -m unittest discover -s python/tests -p 'test_isolated_policy*.py' -v
RUNKITE_TEST_OPENSHELL_PROVER_REPORT="$RUNKITE_TEST_POLICY_OUTPUT/prover-controls.json" \
  python -m unittest discover -s python/tests -p test_openshell_prover_native.py -v

python scripts/start-isolated-fixture.py \
  --cli /absolute/path/to/openshell \
  --output-dir "$RUNKITE_TEST_POLICY_OUTPUT" --tls -- \
  bash -ceu 'python scripts/qualify-isolated.py \
    --cli "$RUNKITE_TEST_ISOLATED_CLI" \
    --gateway "$RUNKITE_TEST_ISOLATED_GATEWAY" \
    --image "$RUNKITE_TEST_ISOLATED_IMAGE" \
    --prover "$RUNKITE_TEST_OPENSHELL_PROVER" \
    --output "$RUNKITE_TEST_POLICY_OUTPUT/policy-evidence.json"'
```

Use `--docker-socket /absolute/path/to/local/docker.sock` on the wrapper when
the local engine does not use its default socket. Remote Docker endpoints and
conflicting contexts are unsupported. The trusted runtime fixture receives that
socket, never the workload. The wrapper generates task-local mTLS material;
keep failed recovery directories private. Inspect `policy-evidence.json`,
`gateway-tls-evidence.json` and `runtime-cleanup.json` together. Accepted deletion
alone is not confirmed cleanup. If cleanup is unconfirmed, reconcile only the
owned sandbox and infrastructure before removing recovery files.

No blocked destination is supplied in this command. It makes no runtime network
reachability claim. A separate `--blocked-url` probe needs an independently
reachable fixture/control before a denied connection can support that claim.

## Measured result, November 17, 2026

The actual local run passed against OpenShell 0.1.8 on macOS with OrbStack Docker
Engine 29.8.2 and the pinned ARM64 runtime images. Both created incarnations had
matching current/loaded policy observations and passed the release checker's
five-domain configuration comparison. Real fixture execution retained the
readable-outside/forbidden-inside file control, non-root execution, absent
bootstrap environment and host socket, streaming, receipt replay and opaque
state continuity. Gateway mTLS probes passed and owned cleanup was confirmed.
The checkpoint/usage clients were in-memory fixtures, not a Runkite plane.

Separate real release-checker controls returned `within_boundary`/exit 0 for
the configured policy, `exceeds_boundary`/exit 1 for a root-user expansion,
`error`/exit 2 for an unknown field and `unsupported`/exit 3 for an unresolved
path. Reports are in the maintainer workspace under
`outputs/runkite-next/openshell-policy-20261117/native-final/` and
`outputs/runkite-next/openshell-policy-20261117/prover-native-controls.json`.
They contain synthetic evidence, not customer policies or model calls.

The separate [hosted native Linux job](https://github.com/getrunkite/runkite/actions/runs/44027024047)
also passed on source commit `778476f4846ded982b98bce7d26e739a59c741be`.
Its Ubuntu 24.04 runner reported Linux `7.0.0-1017-azure`, Docker `28.0.4`
and Landlock ABI `8`. The downloaded summaries confirmed both policy attempts,
all four real checker controls, gateway TLS and owned cleanup. This is evidence
for that native job, separately from the pull request's overall CI status.

The automatic [OpenShell policy workflow](../../.github/workflows/openshell-policy.yml)
adds actual native Linux execution on relevant pull requests/main pushes and
manual dispatch. It checks Docker and Landlock prerequisites and requires
policy comparison, TLS, checker controls and cleanup together. Unsupported hosts
fail rather than skip to success. Inspect the actual current Linux workflow
result separately; the measured result above is local Darwin/ARM64 evidence.
Existing hosted HA evidence in the
[constrained-execution record](constrained-execution.md#hosted-linux-evidence-november-15-2026)
does not retroactively qualify this new checker path.

## Scope and reviewer handoff

The result is containment of observed configuration under the pinned checker's
model of filesystem, process, Landlock, L4 and REST domains. It is not formal
proof of arbitrary workload behavior or continuous kernel enforcement. The
snapshots do not atomically fence exec, cannot exclude a change-and-restore
between observations and do not include every runtime-only grant. Keep actual
runtime probes, deployer trust and runtime/kernel-specific qualification.
The CLI's authored-schema policy JSON omits protobuf-only provider credential,
token-grant owner and current provider-environment revision fields. Marker and
composition checks do not attest the absence of credentials or hidden provider
state. This result applies only to the owned fresh provider-free fixture.
Global/provider composition and unsupported image-dependent comparisons need a
separate qualified contract rather than relaxed checks.

This adds reproducible evidence to [RFE #47](https://github.com/getrunkite/runkite/issues/47)
and [RFE #52](https://github.com/getrunkite/runkite/issues/52), with the
[reviewer handoff](../security-assessment.md). Both public RFEs remain separate
tracking items; this note does not close them. Independent
assessment is still pending. There is no new control-plane HA, live model,
provider-accounting, CNI/cloud-metadata, arbitrary-SDK, signed-attestation or
universal-isolation qualification. No embedded runtime, vault, policy engine or
new production enforcement feature is introduced.
