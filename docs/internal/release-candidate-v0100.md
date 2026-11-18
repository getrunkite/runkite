# v0.10.0 release candidate

Status: historical qualification record for v0.10.0. The merged runtime was
qualified at `7a7ae07`; the final tag identifies `33cf561`. The
[publication record](release-verification-v0100.md) separately records actual
registry uploads, tagged downloads, image verification and their limits.
This page preserves earlier candidate measurements and their original source
identities. The Runner Protocol and SQL accounting changes require a coordinated
upgrade; a version stamp alone is not publication evidence.

## Upgrade sequence

This sequence applies to the complete release, including the earlier candidate's
policy, connector and cron changes. Retain the prerequisites for the version
being upgraded in [the operations runbook](../ops-runbook.md).

1. Plan a maintenance window. Stop new submissions and cron dispatch, pause
   affected connector callers and app-owned continuations, and drain workers and
   queued/inflight assignments. Stop every database/queue writer. Verify
   restorable database, queue and native-receipt backups before schema changes.
   Do not mix old and new cron writers: named-tenant thread selection changed.
2. While writers remain stopped, run `runkite db upgrade` with the matching new
   plane binary. Postgres/SQLite migration 23 and MySQL migration 24 add
   original-record accounting/hold binding and usage uniqueness by
   `(run_id, run_incarnation_id, source)`. Startup also migrates. Allow for
   SQLite's usage-table rebuild and MySQL's implicit-commit DDL. If upgrade fails,
   keep writers stopped and inspect/complete it before traffic resumes.
3. Deploy matching plane and runner builds together, with matching generated
   protocol clients. Workers must advertise `assignment_identity_version: 1`
   before dequeue/cancel subscription and echo the assigned
   `(run_id, run_incarnation_id, generation)` on lifecycle RPCs and bound HTTP.
   HTTP uses `X-Runkite-Run-Incarnation` with the run-ID and generation headers.
   Old assignments without proof are refused before input delivery. Never fill
   their proof from a newer stored row.
4. Upgrade custom transport and state integrations too. Queue mutations must
   compare original identity and generation atomically; cancellation is
   record-wide. Broker publish/subscribe/replay/close use the record pair.
   Accounting, reservations and terminal hooks use
   `state.RunUsageEventStore`, `state.RunUsageHoldStore` and
   `state.RunTerminalHookStore` where configured. A separate read followed by an
   unfenced write is insufficient. Mongo uses its private ObjectID proof and
   retains its existing SQL-only public-feature limits.
5. Preserve legacy evidence. Historical unbound usage, reservations and bare
   run-ID terminal-hook claims cannot establish their original physical record.
   Same-public-ID native receipts without proof are ambiguous as well. Reconcile
   the original operation, charges, remaining holds, receipts and any delivered
   webhook against retained evidence before explicit repair. Use fresh operation
   IDs for intentional new work. Do not bulk delete evidence, rewrite revisions,
   infer identity from current rows/timestamps or repeat an uncertain effect to
   bypass a refusal. A terminal run can still have unconfirmed finalization.
6. Review exact tenant/agent/connector scopes and native tool names. The evaluator
   semantics fingerprint invalidates pending/approved managed connector
   authority from the old evaluator. Resubmit intended operations for fresh
   evaluation and review through the existing retry/retention contract. Preserve
   old rows as history. Already returned app-owned approvals and allow decisions
   are not revoked: their owners must discard old execution authority and
   resubmit saved proposals under the app's expiration/review contract. Work
   already executing is not undone.
7. Upgrade every HTTP replica before enabling optional tools-only connectors.
   Their changed target fingerprint requires fresh managed approval authority.
   Prefer the Supported Postgres/full-Redis profile. Kafka refuses startup unless
   `RUNKITE_KAFKA_ALLOW_NONATOMIC_FENCES=true` explicitly accepts non-atomic
   Compatible mode. Redis event/cancel delivery and reclaim leadership do not
   provide cross-replica atomic Kafka queue CAS. Resume callers only after the
   matching deployment and required reconciliation have been checked.

The accounting migrations refuse automatic downgrade. Rollback requires a
reviewed backup restore and reconciliation of recorded usage and uncertain
effects. Restoring an older binary onto the upgraded database is not the rollback
procedure. See [assignment identity rollout](../ops-runbook.md#assignment-identity-upgrade),
[policy identity rollout](../ops-runbook.md#unreleased-policy-identity-upgrade) and
[the engineering contract](assignment-identity.md).

## Completed local source qualification

The saved clean-source summary identifies
`19b9dff863f15103108228e7fb3fd83bd72eb9d3`, before candidate version preparation.
The [qualification record](assignment-identity.md#local-qualification-2026-11-17)
describes the corresponding boundaries. Its retained local summary is
`outputs/runkite-next/assignment-identity-20261117/qualification-summary.json`.

- Full internal/command Go suites and whole-Go lint passed. Native state and
  transport checks passed, including SQL finalization and four-backend terminal
  hook conformance. Kafka evidence qualifies its explicit weaker mode.
- TypeScript passed 303 unit tests, separately packaged strict-mTLS qualification
  and 10-case managed AI SDK/chat-bridge checks on SQLite and Postgres/Redis.
  Those 10 cases do not include the separately gated AG-UI native example.
- All four managed Python SDK profiles passed with installed wheels and native
  chat bridges on SQLite and Postgres/Redis using local scripted fixtures.
- Targeted Linux API race cases and the complete auth, bridge, in-process and
  SQLite race suites passed. This is not a full-API race-suite claim.
- The integrated external-runtime HA fixture passed three bounded cycles and
  shared evidence validation with confirmed runtime/backend cleanup. Site tests
  passed 43 cases, and OpenAPI checks covered 170 registered Go routes.

Those earlier source checks are separate from the new-version candidate checks
below. Neither establishes publication, live-provider behavior, unrestricted
production soak or independent assessment. Later source or qualification-harness
edits need their affected gates rerun and their exact source recorded.

## Completed local candidate gates

The following evidence is retained under
`outputs/runkite-release-v0100-20261117/`. The Go snapshot archives were built
from clean commit `6e8e72b1e02698125dc2ce8d0e69881a7f691a53` with version 0.10.0.
The subsequent source commit `71a98f1` changes only the TypeScript strict-mTLS test
fixture's npm-pack metadata handling. It does not change the packaged runtime;
the archives were not rebuilt or relabeled as that later commit.

- [x] The coordinated version gate passes for `v0.10.0`, including package/Helm
  versions and file-linked example lock snapshots. GoReleaser 2.20.2's actual
  configuration check passed, and its local `--snapshot` build completed.
- [x] Four snapshot archives and their checksums pass static inspection. Each
  contains exactly seven regular files with no unexpected or linked members.
  All 24 legal/README/notice copies match source, and every binary contains all
  19 tracked Admin assets byte-for-byte (76 comparisons). Mach-O/ELF headers
  match Darwin/Linux arm64 and amd64. The four actual archive binaries also
  pass version probes: native macOS arm64, installed Rosetta for macOS amd64,
  and cached Linux execution bases for both Linux targets. Every probe reports
  `0.10.0`, commit `6e8e72b` and build time `2026-11-17T17:58:36Z`. See
  `go-snapshot-static-audit.json` and `go-archive-runtime-smoke.json`.
- [x] Both npm tarballs and both Python wheel/sdist pairs were built as 0.10.0
  with retained integrity/member manifests. The Python runner's four artifact
  contract tests pass. The Python governance helper's three tests pass,
  including installed-wheel and rebuilt-sdist behavior outside the checkout.
  Use `logs/python-governance-artifacts-root.log` for that pass; the earlier
  socket-denied child attempt remains retained and is not counted as successful.
  The npm governance installed-artifact check and 20 AI SDK/example tests pass.
  Release-artifact helper regressions pass all 16 tests in
  `logs/release-artifact-unit-tests-final.log`.
- [x] Node 22 runner units pass 306 tests with two opt-in native skips. Separate
  packaged strict-mTLS runs on Node 22.19.0 and 26.11.0 pass four tests each,
  including exact package identity, owner-key denial and restored conversation
  behavior. Their receipts are `mtls-node22.json` and `mtls-node26.json`.
- [x] Both pinned Langflow native profiles pass with LFX 1.13.2 and MCP 1.30.0:
  baseline read/write and scoped run-only each produce two managed runs and two
  runner assignments. Both promptfoo 0.125.1 profiles pass with four distinct
  runs/threads and four reference-pinned waits each; the scoped profile also
  passes six denial probes. The AG-UI-enabled real-plane/built-runner fixture
  passes all 12 cases, including its separately enabled native AG-UI behavior.
  See `langflow-{baseline,scoped}.json`, `promptfoo-{baseline,scoped}.json` and
  `logs/ag-ui-native.log`. These remain checkout examples, not registry assets.
- [x] Admin passes all 126 tests and its production build. Rebuilding leaves no
  tracked Admin asset or lockfile drift; the Go archive inspection above checks
  the actual embedded asset bytes. See `logs/admin-test.log` and
  `logs/admin-build.log`.
- [x] All three actual local Linux arm64 candidate images built from `71a98f1`
  pass version/revision/license checks and source-matching legal files as UID
  65532, with read-only, network-disabled smoke checks. The TypeScript image
  also passes native LangGraph and AI SDK model/tool, stream and usage checks;
  this does not qualify managed image dispatch, HA or live providers. Trivy
  0.72.0, using its database updated at `2026-11-17T13:00:16Z`, reports no
  HIGH/CRITICAL fixable findings under `--ignore-unfixed --exit-code 1`.
  See `candidate-images{,-runtime,-scans}.json` and
  `typescript-image-runtime.json`. These are local arm64 results only.
- [x] Final non-mutating consistency checks pass all 14 commands at `71a98f1`:
  OpenAPI covers 170 routes, its 26 contract tests pass and all three regenerated
  specs match committed bytes; site tests pass 43 cases with 71 pages, 3,851 local
  references and no broken links; all 10 protocol fixtures and execute goldens
  pass. Version, GoReleaser and Helm checks also pass. Local Helm is 3.19.0;
  hosted CI pins 3.16.4, so the local pass does not substitute for that toolchain.
  See `final-consistency/summary.json`.
- [x] The OpenShell workflow and documented fixture invocation pass their
  checker and evidence paths as explicit quoted arguments across the sterile
  child environment. Two executable regressions reproduce the former failures
  and verify the corrected actual command excerpts with paths containing spaces
  and without ambient credentials. All 12 environment, 31 fixture and 16 policy
  qualification unit tests pass. See `logs/openshell-launchers-{before,after}.log`
  and `logs/openshell-{environment,fixture,policy}-tests.log`. This qualifies the
  launcher contract, not a new hosted native-policy execution. The workflow's
  path filters include its environment helper dependency. Actionlint with actual
  ShellCheck passes; use `logs/openshell-actionlint-shellcheck-root.log` for that
  result, separately from the child process's denied Docker access.

These are local candidate checks, not tagged release or published-image evidence.
The snapshot's metadata still names the existing `v0.9.0` Git tag as build
context. Cached Linux images used to execute archive binaries are not candidate
image qualification. Registry preflight receipts for all four package projects
report `needs_publish`; they establish neither upload authority nor availability.

<a id="current-upstream-audit-blocker-november-18-2026"></a>

## Admin dependency risk, November 18, 2026

[PR #93](https://github.com/getrunkite/runkite/pull/93) reached hosted checks.
The Admin and promptfoo audit steps stopped on
[GHSA-fc5q-39xc-vjpf](https://github.com/advisories/GHSA-fc5q-39xc-vjpf).
The separate hosted race regressions passed. The promptfoo follow-up removes
its affected watcher closure and passes a clean local low-threshold audit,
32 tests and both real-CLI managed-run profiles; see
[the dependency record](promptfoo-managed-provider.md#watcher-dependency-removal-november-18-2026).
The [hosted promptfoo run](https://github.com/getrunkite/runkite/actions/runs/44246706486)
verified that follow-up at `a5ed97e`, including both native profile receipts.
The original main `test` job stopped at Admin audit before its later suites ran.

The Admin audit still reports five propagated high findings originating
in picomatch used by Vite/tinyglobby. Live registry metadata reports picomatch
4.0.7 as latest, still within the advisory's affected range, and Vite 8.3.8
retains it. The advisory lists no fixed version and is not withdrawn. These
are build/dev dependencies; no corresponding matcher path was identified in
the shipped static Admin app. That scope does not fix the vulnerability or
establish eligibility by itself. The separately approved, qualified exception
below now accepts this exact build-tool risk until November 25 at 00:00 UTC.
All other audit, current functional and remaining release gates stay blocking.
The supporting receipts are under `outputs/runkite-pr93-picomatch-20261118/`.

### Independent CI qualification and the blocking summary

CI separates `Full test suite` from `Admin dependency audit`. Both retain their
existing install/tool versions and commands. Admin requires either a complete
zero-finding report or the exact approved exception case. Its guarded exception
requires verified full-suite qualification, recorded below. Other npm
callers retain their existing high/critical threshold and genuine audit-API
outage warnings; unreadable, duplicate-key and non-finite reports fail. Missing
or inconsistent Admin data always fails. The existing check name `test` is retained as a
summary that always evaluates both job results and passes only when both report
`success`. Failure, cancellation, skipping, missing results or an unknown result
cannot produce a green summary. Existing branch protection for `test` continues
to cover the audit as well as functional qualification.

This separation lets the backend/SDK/UI suites run while the audit reports an
unfixed advisory. This separation alone does not activate an exception,
lower advisory severity or make a successful functional job sufficient for
merge/release. Local validation
executes the actual summary command across 64 result combinations and verifies
that the old suite and total audit calls are preserved. The recorded hosted
qualification below passed every step of the full suite.

<a id="proposed-admin-advisory-exception-not-active"></a>
<a id="conditionally-approved-admin-advisory-exception-inactive"></a>

### Qualified Admin advisory exception

Sharan Harsoor conditionally approved the following terms on November 18, 2026
at `06:20:46Z`, requiring the full hosted CI suite to pass first. That condition
is now satisfied. The policy record is `active`, with verified qualification
for commit `47534f39fea69bcd162f7c07c5f3b07bd4a851d1`,
[CI run 44263244785, attempt 1, Full test suite job 134661476644](https://github.com/getrunkite/runkite/actions/runs/44263244785/job/134661476644).
All 53 recorded steps completed successfully, with no skipped steps. The job
completed at `2026-11-18T08:30:19Z`; the other 18 applicable PR checks passed.
The Admin audit and its `test` summary still failed while the policy was inactive.
The manual `combined-ha` gate was not run and remains separate release work.

The exact Admin manifest/lockfile, original audit response, source and serving
boundary were rechecked before activation. The original five HIGH findings
remain; this is accepted build-tool risk, not a vulnerability fix. It expires
at `2026-11-25T00:00:00Z` without automatic renewal. Both earlier hosted runs
described below failed and were excluded from the qualification. Saved run,
job, source, upstream and raw audit evidence is retained under
`outputs/runkite-pr93-ci-isolation-20261118/hosted-44263244785/`.

The reviewed terms are:

- Accept only `GHSA-fc5q-39xc-vjpf` originating at Admin's locked picomatch 4.0.5
  and its five propagated development/build findings. No other project,
  dependency/advisory root or production use is covered. This accepts known
  build-tool denial-of-service risk; it does not fix the vulnerability.
- Bind activation to these exact source inputs. The SHA256 for
  `admin-ui/package.json` is
  `589b51689b7fd8d0b7be89dfa6775cdf3eba53ee9f23fad771c49fd336951e84`;
  the SHA256 for `admin-ui/package-lock.json` is
  `d07e1958b224e724ec30dd498e2c02468976bb4949e1268e3bc64d84b65b3788`.
  A changed closure or a different/additional finding requires fresh review.
- Expire at `2026-11-25T00:00:00Z`, or seven days after approval, whichever is
  earlier. No automatic renewal. Revoke earlier when a verified upstream fix
  becomes available, the advisory is withdrawn or production reachability is
  identified. The reviewed
  build uses repository-controlled patterns; arbitrary untrusted patterns and
  exposed Vite development servers are outside this scope.
- Retain the raw audit evidence and visibly label the accepted risk. The guarded
  evaluator accepts only this exact case after activation and keeps other
  high/critical findings blocking. All functional, image, artifact and remaining
  release gates must still pass. Do not report zero vulnerabilities or a fixed
  package. Missing or inconsistent Admin audit data cannot establish eligibility.
- Before activation, record a verified successful `Full test suite` job from the
  hosted `CI` workflow for the reviewed source, including its exact commit, run,
  attempt, job and completion time. Check the other applicable hosted jobs too.
  This is maintainer-reviewed qualification evidence, not cryptographic
  attestation or a live API verification by the audit helper. The existing
  `test` summary still requires the current full suite and audit result on every
  later run; an older receipt never waives current tests. A missing or invalid
  prerequisite keeps the exception inactive and the known finding blocking.

### Hosted governance fixture follow-up, November 18, 2026

The first independent full-suite run reached the backend tests and exposed one
root failure: `TestGovernanceAnnounceBar/phase0_run_binding` returned
`run_incarnation_mismatch`. The fake in-flight assignment had no incarnation ID,
while its request obtained a nonempty proof from the unrelated durable-store
fixture. `bindInternalRun` correctly refused the mismatch. Postgres enabled the
parent test; this particular middleware check did not use the database.

The fixture now supplies an explicit captured assignment identity and obtains
its request proof from that assignment. The same check also runs as
`TestGovernanceRunBindingFixture` without a Postgres prerequisite, including
missing-record, superseded-record and stale-generation denial controls. The
existing Postgres phase name and durable audit/HITL checks are preserved.
Production authentication and generation fences are unchanged.

A local red/green reproduction and the complete three-phase governance smoke
passed against an owned loopback `pgvector/pgvector:pg16` fixture. All eight
`TestMiddleware_RunBinding*` functions passed alongside it, including exact
record, generation-zero and runner-kind ownership checks. Evidence is retained
under `outputs/runkite-pr93-governance-fixture-20261118/`. The hosted run's other
38 Go packages passed, but later SDK/E2E steps were skipped after the API package
failed. This failed run could not supply the exception's qualification receipt;
the later successful job above provides that separate evidence.

### Hosted ADK tracing fixture follow-up, November 18, 2026

The [next hosted full-suite job](https://github.com/getrunkite/runkite/actions/runs/44260049027/job/134650899585)
at `bfd7b0e` passed all backend tests, the separate Postgres governance smoke and
the shared Python runner tests. The isolated SDK stage then exposed an embedded
Python fixture in `test_native_sdk_spans_are_recorded_without_prompt_tool_or_output_content`:
its run context and assignment omitted the now-required original-record proof.
The identity guard rejected that setup before its tracing assertions executed.

Both fixture bindings now use the same explicit identity. The production
adapter and identity checks are unchanged. A local red/green reproduction,
the six provider tests and all 69 ADK tests passed with pinned Google ADK 2.13.0;
two tests retain their explicit native end-to-end prerequisites. The tracing
test still checks native spans while rejecting prompt, tool and output content.
Evidence is retained under
`outputs/runkite-pr93-ci-isolation-20261118/adk-tracing-fixture/`. This failed
hosted run could not qualify the conditional Admin exception either. The later
successful job above includes the subsequent SDK and end-to-end stages.

### Guard implementation and activation procedure

The trusted record is [scripts/admin-audit-exception.json](../../scripts/admin-audit-exception.json).
The wrapper captures one npm response and command status, then evaluates that
same evidence with [npm_audit_policy.py](../../scripts/npm_audit_policy.py).
The original report is printed in the CI log. An active acceptance emits an
explicit warning that five HIGH findings remain, with the advisory and expiry;
it does not rewrite the report or call a second audit endpoint for its verdict.
There is no CLI or environment switch to activate a policy or override its clock.

Eligibility binds the complete canonical npm v2 report, including all five
propagated entries, advisory source, paths, ranges, fix suggestions and metadata.
A changed report needs fresh review even when the difference looks harmless.
The fixed approval/expiry bounds, manifest/lockfile hashes and development-only
lock entries are checked too. Ordinary Admin success requires zero findings;
lower-severity-only and malformed reports cannot disguise an unapproved case.

Activation requires these separate maintainer actions. The recorded evidence
above completes steps 1 through 3; step 4 remains mandatory on the activated
commit and every later change:

1. Publish the CI separation and obtain a successful hosted `Full test suite`
   job for the reviewed source. An older local result, standalone race job or
   another job with a successful status is not a substitute.
2. Verify that job and the other applicable checks using GitHub's actual run
   and job records. Review the exact source, Admin application/build/serving
   boundary and original audit report again. Input hashes do not analyze
   production reachability or bind every source file. Changes to that boundary
   require explicit re-review and deactivation when outside the approved scope.
3. Record the reviewed repository/workflow/job, run ID/attempt/URL, head SHA,
   successful conclusion/completion time, verifier and input hashes in
   `qualification`; then set `status` to `active`. Do not invent a successful
   receipt, infer it from the approval, extend the expiry or use test fixtures.
4. Run CI again on the activated commit. The current `test` summary still
   requires both its complete functional suite and audit-policy job to succeed.
   Other image, artifact, manual qualification and release checks remain required.

The 33 policy regressions include exact acceptance in private temporary
fixtures with a frozen test clock, real-clock rejection of pending records and
validation of the checked-in activation's complete qualification. Source drift,
new/hidden findings, expired or missing authority,
malformed input and single-response evidence handling are covered. These tests
do not establish successful hosted qualification or activate the checked-in
record. Local guard evidence is retained under
`outputs/runkite-pr93-ci-isolation-20261118/`.

## Merged-main qualification, November 18, 2026

[PR #93](https://github.com/getrunkite/runkite/pull/93) merged as
`7a7ae079475a20da0d2fe20c9e7153be2f688d36`, tree
`7bbfbcbe4631e7d58281e3be2d6efc6037040be7`. The following fresh results identify
that exact source and version 0.10.0. They do not relabel the older local
archives, images or receipts above. Evidence is retained under
`outputs/runkite-release-v0100-qualification-20261118/`.

### Hosted source and integration checks

- [Main CI](https://github.com/getrunkite/runkite/actions/runs/44273439083)
  passed all 11 jobs, including the full suite, three image checks and both
  packaged mTLS Node profiles. Separate
  [identity race checks](https://github.com/getrunkite/runkite/actions/runs/44273439036)
  passed too. The Admin audit still retains
  five HIGH findings under the qualified exception above, expiring November 25
  at 00:00 UTC. Its green result does not mean the vulnerability is fixed.
- [Langflow](https://github.com/getrunkite/runkite/actions/runs/44273439025)
  passed its baseline and scoped native MCP caller profiles.
  [promptfoo](https://github.com/getrunkite/runkite/actions/runs/44273439081)
  passed both native CLI/provider profiles. Downloaded receipt archives match
  GitHub's SHA256 digests and the exact run, attempt, job and source records.
  These checks use scripted agents, not live model providers or visual editors.
- [AG-UI](https://github.com/getrunkite/runkite/actions/runs/44273439156)
  passed 116 shared bridge tests, 25 AG-UI unit tests and 11 native tests, with
  zero skips. A separate seeded macOS arm64 run passed 12 native tests, including
  Unicode run IDs and named-tenant isolation in the shared managed-run path,
  plus AG-UI render/replay/detach/Stop. That local run uses built checkout runner
  files and SQLite/in-process state. Traffic used loopback; the existing CLI
  listeners bound wildcard addresses. Its owned processes and observed ports
  closed without forced cleanup. This is not a published npm artifact test.
- [OpenShell effective policy](https://github.com/getrunkite/runkite/actions/runs/44273439217)
  passed its observed policy controls, TLS checks and cleanup.
  The separately dispatched [combined-HA run](https://github.com/getrunkite/runkite/actions/runs/44278453492)
  passed all nine managed cases and three requested cycles on native Linux
  amd64, with four cancellation controls, three live-generation controls,
  packaged TypeScript mTLS checks and confirmed runtime/backend cleanup.
  The uploaded source receipt names the full merge SHA. Ordinary push/PR runs
  skip this manual job; their green fixture guards are not equivalent evidence.

The hosted artifact/log validator rechecks the trusted workflow assertions and
the shared HA evidence validator. Its digest/member records and negative
controls are under `evidence-helpers/`. This is bounded maintainer qualification,
not a production soak or independent security assessment.

### Fresh local archives, packages and images

- Four Go snapshot archives were rebuilt with Go 1.26.11 and GoReleaser 2.20.2.
  Checksums, architecture headers, version/source stamps and legal/README bytes
  passed. All 19 embedded Admin assets matched in every binary, 76 comparisons.
  All four actual archive binaries passed version probes: native macOS arm64,
  installed Rosetta for macOS amd64, and the corresponding Linux candidate
  images. See `go-artifacts/`; these snapshots are not tagged release assets.
- The YAML release header was rendered offline with Go's `text/template` for
  both 0.10.0 and 0.9.0. The 0.10.0 coordinated-upgrade notice precedes downloads,
  retains the migration/protocol instructions and pins all download/install
  versions; the older version does not inherit that notice. See `release-header/`.
  This checks the header, not the complete auto-generated GitHub release body.
- Both npm tarballs and both PyPI wheel/sdist pairs were rebuilt from an exact
  source export. All 276 members, including 252 direct source comparisons,
  passed the artifact audit. Installed contracts passed: Python runner four,
  Python governance three, npm runner four strict-mTLS cases, and npm governance
  35 helper plus 20 AI SDK cases. All four public registry preflights report
  `needs_publish`. This establishes neither upload authority nor availability.
  See `artifacts/` for exact pinned build tools, manifests and retained logs.
- All three actual images were built separately for Linux arm64 and amd64 with
  version/full revision stamps, fresh bases and uncached package upgrade layers.
  All six passed UID 65532, legal-byte, architecture and version checks. Both
  Python images executed the native LangGraph echo; both TypeScript images
  passed the native LangGraph/AI SDK tool, stream and usage fixture offline.
  Trivy 0.72.0 with a November 18 database passed the existing HIGH/CRITICAL,
  `--ignore-unfixed`, fail-on-findings gate for all six immutable local images.
  It does not assess unfixed, LOW or MEDIUM findings. See
  `images-{arm64,amd64}-attempt1/`. On this engine, Docker's local image ID names
  a manifest while Trivy reports the config digest. The receipts bind the saved
  config and uncompressed layers to the inspected image before accepting a scan.

The local engine is arm64 Linux on macOS. Its amd64 images and archive probes
use cross-architecture userspace emulation; this is not native Ubuntu amd64
image execution. The separate hosted HA run is native Linux amd64. The local
runner revision labels are additional candidate-build metadata: the current
release workflow does not supply those labels on runners. Final GHCR provenance
must come from actual release job records and registry digests. No local image,
snapshot archive or package was uploaded during these checks.

### Publishing configuration and final proof

The personal maintainer account `sharanharsoor` owns both npm/PyPI package pairs.
Authenticated configuration review confirmed the npm runner's existing GitHub
publisher. With explicit maintainer approval, npm governance now also trusts
`getrunkite/runkite`, `release.yml`, with no environment restriction and both
direct/staged publishing permissions. The current workflow uses direct
`npm publish`. PyPI governance's existing publisher matches the same repository
and workflow. PyPI runner still uses its existing project-token path.

Configuration, ownership and secret presence cannot prove a fresh upload.
Neither npm OIDC path was exercised during qualification. PyPI runner's token
presence was confirmed, but its current validity is unproven; no token values
were read. Keep the
[actual v0.9.0 authentication record](release-verification-v090.md#registry-publication-and-authentication)
unchanged. Actual upload outcomes and immutable public bytes remain mandatory
release-time evidence. Do not publish dummy versions to manufacture that proof.

The package README preparation accompanying this record changes documentation
only. The results above remain attached to `7a7ae07`. Build final artifacts from
the selected tag, including its README bytes and actual source stamp; do not
publish or relabel these local snapshot artifacts as that later tagged build.

<a id="remaining-candidate-and-publication-gates"></a>

## Candidate and publication gates

- [x] Merge the reviewed runtime source and pass its hosted checks at `7a7ae07`,
  including the separate three-cycle combined-HA qualification and integration
  receipts above. Follow the
  [integrated contracts](ecosystem-integration-checklist.md#qualify-the-integrated-contracts).
- [x] Build and test version-stamped local candidates for all three images on
  both Linux target architectures, with the explicit emulation boundary above.
  Ordinary CI's `dev` plane images and older cached execution bases are not this
  evidence. Actual published image variants still require their own checks.
- [x] Rebuild and validate the four Go archives and all four registry package
  projects from the merged source; review each intended publisher configuration
  under the personal maintainer account. Retain the actual authentication limits.
- [x] Finish final-source preparation and pass its required CI before tagging.
  Verify the documented difference from the qualified runtime source. Keep the
  rendered coordinated-upgrade notice with the release body, and build final
  package READMEs from that source. [PR #94](https://github.com/getrunkite/runkite/pull/94)
  passed, its tested tree exactly matches the tagged merge `33cf561`, and that
  merge passed [main CI](https://github.com/getrunkite/runkite/actions/runs/44298168726).
  Availability promotion remained gated on actual artifact verification.
- [x] Establish actual package upload outcomes in the intended release context.
  A successful preflight, configured publisher, continued failed step or
  matching-existing recovery is not fresh OIDC publication proof. Do not expose
  credentials in evidence or silently rely on an unverified fallback token.
- [x] After authorized tagging/publication, inspect the real Release jobs and
  verify four Go archives/checksums, three OpenAPI assets, both npm packages,
  both PyPI wheel/sdist pairs and both runtime platforms of all three image
  repositories. Verify actual downloads, image identity/runtime checks, legal
  content and existing vulnerability gates. Record the true authentication path;
  neither an upload acknowledgement nor a matching existing package proves a
  fresh OIDC publication.
- [x] Record the immutable tag/source and artifact evidence in a publication
  verification note. Only then move the shipped changelog items into the
  published release and promote README/site/support availability together.
  Retain checkout-only labels for examples absent from registry artifacts and
  keep historical v0.9.0 records unchanged. Check Pages and protocol mirroring
  separately; neither workflow publishes runner packages. The
  [publication record](release-verification-v0100.md) records the completed
  artifact checks and actual authentication/recovery outcomes.

The qualification actions above did not create a tag or upload release
artifacts. Subsequent publication uses the immutable final source and its own
evidence in the [publication record](release-verification-v0100.md).
