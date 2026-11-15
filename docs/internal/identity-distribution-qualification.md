# Identity and distribution qualification

Status: maintainer qualification of the untagged 0.8.0 source candidate on
2026-11-15. Published v0.7.0 is unchanged. This record accompanies
[RFEs #45 through #52](identity-and-distribution-plan.md); it is not an
independent assessment or a promise that every deployment is safe.

## Executed checks

- The final full default Linux Go race run passed `./internal/...` and
  `./cmd/...` with Go 1.26.11. Separate live Postgres, Redis, MySQL and Mongo
  packages also passed under Linux race, and live MySQL/Mongo/SQL completion
  suites passed on the host. Optional external services were not silently
  treated as exercised by the default run.
- Qualification caught two test-harness defects: a SQL timestamp test driver
  missing the new snapshot read, and a gRPC tracing test reading its exporter
  before server finalization. The driver now models that read; the tracing
  test waits for graceful handler cleanup. That test passed 20 consecutive
  focused runs before the final full default race pass. Neither fix weakens a
  production check or extends an arbitrary timeout. Strict JWT/JWKS transport,
  same-fleet certificate rejection, queue capture, connector delegation and
  credential reconciliation passed focused real-network regressions.
- The TypeScript runner suite passed 299 tests with the actual local binary
  enabled, including managed AI SDK lifecycle and receipt recovery. The
  maintainer identity regression entrypoint also passed end to end; its ordinary
  invocation deliberately skips optional packaged end-to-end fixtures unless
  their binary inputs are supplied.
- A separate installed TypeScript npm fixture passed all four strict-mTLS
  cases on Node.js 26.11.0. A same-URI peer with a different key could not use
  HTTP checkpoint/status or gRPC heartbeat/completion authority; two managed
  turns preserved durable history over HTTPS. This was one SQLite/inprocess
  development plane, not a TypeScript HA test. Its report is
  `outputs/runkite-next/typescript-workload.json`.
- All eight installed Python managed profiles passed: Microsoft Agent
  Framework, OpenAI Agents, Pydantic AI and Google ADK on SQLite and on
  Postgres/Redis, each including native chat transport and lifecycle scenarios.
  The shared managed chat bridge passed 116 tests and its TypeScript check.
  Fixtures used real SDK APIs with scripted models and fictional prices.
- Admin passed 81 unit tests and its production build. Browser checks used the
  actual built bundle with read-only, clearly marked synthetic fixtures for
  issuer and worker identity, constrained exchange, Vault request references,
  credential cleanup, and unknown, partial and measured-zero usage. Desktop
  and 390-pixel mobile views were checked. This visual fixture does not qualify
  backend enforcement.
- Static site checks passed 43 tests. The local-link scan covered 68 HTML pages
  and more than 3,600 references without broken local links. Engineering
  catalogs now link the new design records and distinguish published controls
  from the next-release preview.
- All four distributions built as 0.8.0. Final Python governance wheel and
  rebuilt-sdist installed-behavior checks passed three tests, including optional
  Microsoft middleware. The installed npm governance archive passed 35 helper
  cases and 12 pinned real AI SDK integration cases. Artifact recovery passed
  nine tests. File manifests record content hashes and complete permissions.
- Version consistency, Python lint/format, OpenAPI route/contract validation,
  workflow lint and source whitespace checks passed. The rebuilt Admin bundle
  retains five inherited whitespace warnings inside third-party CSS templates. The API specification covers
  169 registered routes, including the two internal usage operations.

## Actual combined external-runtime qualification

`scripts/test-isolated-ha.sh` ran against two real Runkite TLS planes sharing
Postgres and Redis, an installed Python runner wheel and pinned external
OpenShell 0.1.8. The host was macOS with OrbStack Docker Engine 29.8.2. Three
bounded fault cycles passed; managed lifecycle execution took 45.568 seconds.

The run covered server-injected MCP calls, durable conversation continuity,
crash after receipt commit without repeating a tool, refusal to replay an
uncertain launch, cancellation, exact worker ownership and acknowledged partial
usage surviving reporting-replica loss. Recovery retained the actual original
usage reporter and generation. Five separate gateway probes verified authorized
TLS, rejection of wrong server trust and wrong client identity, refusal during
outage and authenticated recovery after restart. Owned cleanup was confirmed.

Earlier fixture attempts exposed an incomplete offline dependency cache, a
self-issued leaf certificate caused by a reused CSR subject, and an assumption
that every first useful report must have generation 1. These attempts failed.
The final run used installed dependencies, verified certificate chains and
captured the actual accepted receipt/report generation. TLS validation and
workload identity enforcement remained enabled. The TypeScript fixture also
exposed Node.js 26 rejecting the current gRPC client's IP-literal SNI. Runkite
now fails promptly with a DNS endpoint diagnostic instead of polling forever;
its passing fixture uses a real matching DNS SAN. No certificate-validation
bypass or dependency patch was introduced.

The final default race log is `outputs/runkite-next/full-default-final-race.log`;
the live backend run and eight managed-profile logs are retained alongside it.
Local external-runtime reports are retained under
`outputs/runkite-next/isolated-ha-tls-final/runtime/`: `managed-evidence.json`,
`gateway-tls-evidence.json` and `runtime-cleanup.json`. Distribution evidence is
under `outputs/runkite-security-review/release-copy080-distributions/verification.json`.
This final package set includes durable registry/source installation wording in
the packaged READMEs. It supersedes the earlier `final080-distributions` set;
runtime members are compared against that qualified set, and the changed helper
archives pass their installed behavior checks again. No publication is implied.
These workspace artifacts are separate from the repository. Reproduce them
using the commands in [assessment](../security-assessment.md),
[constrained execution](constrained-execution.md) and
[distribution](governance-distribution.md). Do not upload the fixture's private
certificate or recovery directories.

## Hosted qualification

The milestone merged through [PR #64](https://github.com/getrunkite/runkite/pull/64),
whose final PR CI passed. The
[main CI](https://github.com/getrunkite/runkite/actions/runs/43627518912), Pages,
Review HA and the protocol mirror also passed on the merge commit. The first hosted
[manual identity run](https://github.com/getrunkite/runkite/actions/runs/43627959919)
failed the Node 22 packaged HTTP preflight and did not reach external-runtime
HA. The [compatibility correction](node-fetch-tls-compatibility.md) merged in
[PR #65](https://github.com/getrunkite/runkite/pull/65). Its final PR and
[main CI](https://github.com/getrunkite/runkite/actions/runs/43633285942) passed,
including mandatory packaged mTLS jobs on Node 22.19.0 and 26.11.0. Main Review
HA and Pages also passed.

The next [hosted run](https://github.com/getrunkite/runkite/actions/runs/43633774499)
passed the TypeScript mTLS stage, then failed before executing the isolated
Python workload: its bridge supervisor could not reach the loopback-published
gateway on native Linux. Three gateway TLS preflight probes passed and cleanup
was confirmed. The later outage/restart probes and requested workload cycles did
not complete. The correction uses native host networking only for the trusted
Linux management companions, explicitly overrides the gateway image's bind
flags, and keeps the workload's network and Landlock policy unchanged. See
[local engine networking](constrained-execution.md#local-engine-networking).
The corrected fixture passed three bounded cycles on a local Linux arm64 VM
engine, all five gateway TLS probes and confirmed cleanup; the running gateway
bind was checked directly. This is separate from hosted Ubuntu amd64 evidence.
The separate hosted gate then passed in
[run 43684701615](https://github.com/getrunkite/runkite/actions/runs/43684701615)
on merged revision `96051cc5b9f88bd4bf9285a227ed4a357d0ea889` after
[PR #66](https://github.com/getrunkite/runkite/pull/66). Its retained
`build/source.json` identifies version 0.8.0, native Linux x86_64 and all three
requested repetitions. The host recorded kernel `7.0.0-1014-aws`, Docker Engine
28.0.4 and Landlock ABI 8.

The installed Python parent and external OpenShell 0.1.8 completed all three
bounded failover cycles against two actual TLS planes sharing Postgres/Redis.
The managed evidence records 33.325 seconds. All five gateway TLS probes passed,
including failed requests during outage and verified recovery after restart;
owned cleanup was confirmed. The separate packaged TypeScript profile passed
all four direct-mTLS cases on Node 22.19.0, including same-URI/different-key peer
rejection and two durable managed turns. That TypeScript profile remains
SQLite/inprocess, not TypeScript HA.

The safe workflow artifact is `identity-qualification-43684701615-1`. Its
source, installed dependency inventory and JSON reports are retained locally
under `outputs/runkite-next/hosted-pr66-qualification/`, outside the repository.
This closes the bounded hosted-qualification gate. It is neither a registry
publication nor an independent assessment or long-duration production soak.

<a id="actual-partial-publication"></a>

## Verified v0.8.0 publication

The immutable `v0.8.0` tag points to
`c63b8203123f6b7d032b0b5aca59abe9227fd973`. The original
[Release run](https://github.com/getrunkite/runkite/actions/runs/43695507859)
passed version-check, GoReleaser and Docker publication. Its two npm jobs failed
before publication, and both PyPI jobs uploaded successfully before failing
their public-index visibility checks. Preserve those original outcomes. See
[package recovery](release-recovery-v080.md) for the failure analysis and the
separate recovery controller.

Actual public artifacts were checked on November 15, 2026:

- All four Go archives passed checksum, architecture, packaged license/notice
  and source-file checks. All four downloaded binaries executed their version
  command and reported `0.8.0` at `c63b820`. The three downloaded OpenAPI files
  matched the tagged source.
- Both PyPI projects, `runkite-runner` and `runkite-governance`, exposed their
  wheel and source distribution. All four public SHA256 digests matched the
  original retained files, and public downloads passed the immutable artifact
  checker. The governance helper's original upload used its Trusted Publisher;
  the runner used its project token. This establishes a genuine helper OIDC
  publication, not an OIDC claim inferred from an existing matching version.
- Both npm packages, `runkite-runner` and `runkite-governance`, passed public
  SHA512 integrity, downloaded SHA256 and archive-content checks against the
  exact intended tarballs retained by the first package recovery. Both were
  published through the maintainer's personal npm session, after interactive
  account verification. This is not evidence of npm OIDC publication.
- All three public GHCR indexes resolved to exactly Linux amd64 and arm64
  runnable variants. All six variants passed offline version, UID 65532 and
  license/notice checks. Both TypeScript variants passed all nine native image
  qualification cases, including offline LangGraph and AI SDK execution.
- All six published image variants passed Trivy 0.72.0 with the database
  updated at `2026-11-15T07:02:44Z`: no fixable High or Critical findings.
  This gate excludes unfixed findings and lower severities. One report came
  from the remote digest scan; the other five used archives of the exact
  pulled digests, with manifest, configuration and layer hashes verified.

The first scan harness exhausted its bounded temporary workspace during
database download. A later remote attempt completed one scan, then was
cancelled before switching the remaining targets to verified archives. Prior
attempt logs remain linked from the final evidence; none is rewritten as a
passing attempt. All owned probe containers and temporary image archives were
removed. The final artifact evidence is retained outside the repository under
`outputs/runkite-next/verified-go-v080/`,
`outputs/runkite-next/v080-go-cross-platform-runtime/` and
`outputs/runkite-next/v080-ghcr-verified/archive-scans-20261115T093103Z-9e5fef77/`.

[PR #68](https://github.com/getrunkite/runkite/pull/68) merged the corrected
controller at `6c39349b83ca93f1324970ec8cccef9aff2ebca1`. Its ten PR checks and
[main CI](https://github.com/getrunkite/runkite/actions/runs/43701077721)
passed, including the actual npm 12 build/pack and installed-helper checks.
The [first package recovery](https://github.com/getrunkite/runkite/actions/runs/43701397992)
validated source and both Python packages, then failed npm authorization. Its
retained npm tarballs were the exact files used for personal publication.
The [final package verification](https://github.com/getrunkite/runkite/actions/runs/43702484102)
passed all five active jobs: source validation, both npm checks and both PyPI
checks. Publishing jobs were skipped when immutable registry contents already
matched. Go, container and PyPI upload jobs remained outside manual recovery.

<a id="pending-publication-and-external-assessment"></a>
<a id="pending-npm-publication-and-external-assessment"></a>

## Remaining external qualification

Before a future automated npm publication, configure and validate each
package's publishing authority when a genuine new version is ready. The
successful matching-existing checks above did not exercise npm OIDC or repair
the failed CI upload authorization. Keep the original failed Release and
recovery runs as historical outcomes; the successful final verification is
separate evidence.

Independent reviewer findings, external identity-provider/Vault deployment
qualification, production PKI rotation, hostile-code attestation, live-provider
billing correctness and a production multi-replica soak remain outside this
bounded run. Keep the external bridge experimental. Do not convert self-reported
usage into a provider-verified billing claim or call this independently assessed.
