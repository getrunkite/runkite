# Identity and distribution qualification

Status: maintainer qualification of the untagged 0.8.0 source candidate on
2026-11-14. Published v0.7.0 is unchanged. This record accompanies
[RFEs #45 through #52](identity-and-distribution-plan.md); it is not an
independent assessment or a promise that every deployment is safe.

## Executed checks

- The full Linux Go race run exercised `./internal/...` and `./cmd/...`, with
  disposable Postgres and Redis. It found a synthetic SQL timestamp test driver
  that did not recognize the new usage-snapshot read. The driver was corrected;
  the affected `internal/state/sqlcompletion` package then passed under Linux
  `-race`. The other packages passed the original full run. A failed aggregate
  invocation is not recorded as a green invocation.
- Separate live MySQL, Mongo and SQL completion package suites passed on the
  final production source. Strict JWT/JWKS transport, same-fleet certificate
  rejection, workload queue capture, connector delegation and credential
  reconciliation also passed focused real-network regressions.
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

Local reports are retained under
`outputs/runkite-next/isolated-ha-tls-final/runtime/`: `managed-evidence.json`,
`gateway-tls-evidence.json` and `runtime-cleanup.json`. Distribution evidence is
under `outputs/runkite-security-review/final080-distributions/verification.json`.
These workspace artifacts are separate from the repository. Reproduce them
using the commands in [assessment](../security-assessment.md),
[constrained execution](constrained-execution.md) and
[distribution](governance-distribution.md). Do not upload the fixture's private
certificate or recovery directories.

## Pending external gates

A prepared workflow is not evidence of hosted execution. Record actual GitHub
CI and manual qualification run URLs after the candidate is pushed. First
npm/PyPI helper publication, registry authorization and a genuine OIDC publish
remain unverified. Explicit registry 404 responses establish absence at the
check time, not name reservation or permission to publish.

Independent reviewer findings, external identity-provider/Vault deployment
qualification, production PKI rotation, hostile-code attestation, live-provider
billing correctness and a production multi-replica soak remain outside this
bounded run. Keep the external bridge experimental. Do not convert self-reported
usage into a provider-verified billing claim or call this independently assessed.
