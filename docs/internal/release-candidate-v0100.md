# v0.10.0 release candidate

Status: prepared locally, unpublished. The selected version is 0.10.0 because
the Runner Protocol and SQL accounting changes require a coordinated upgrade.
Version metadata is not publication evidence. Published v0.9.0 remains the
verified baseline in [the release procedure](../releasing.md).

## Upgrade sequence

This sequence applies to the complete candidate, including earlier Unreleased
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

## Remaining candidate and publication gates

- [ ] Publish the reviewed final source and qualification changes through the
  approved publication path. Hosted qualification must run on that exact
  published candidate before tagging.
- [ ] Qualify the exact assembled candidate in hosted CI, including the pinned
  OpenShell native-policy check, Langflow baseline and run-only scoped caller,
  promptfoo baseline and scoped native CLI, and AG-UI native
  delivery/detach/reattach/Stop workflows. The local passes above do not replace
  these hosted gates. Retain their separate receipts.
  Follow the [integrated gates](ecosystem-integration-checklist.md#qualify-the-integrated-contracts)
  and the explicit manual external-runtime qualification boundary.
- [ ] Complete Linux amd64 candidate image qualification for all three images.
  The local arm64 passes do not qualify amd64 or published multi-platform
  indexes. Verify both actual published variants after release as listed below;
  archive execution inside an older cached image does not satisfy this gate.
  Ordinary CI image scans currently use the default `dev` plane build metadata;
  they do not establish the identity of a version-stamped release image.
- [ ] Verify each package's genuine publication authority before tagging.
  Previous matching-existing npm checks did not establish successful npm OIDC;
  preserve the [actual v0.9.0 authentication record](release-verification-v090.md#registry-publication-and-authentication).
  Do not publish dummy versions or expose credentials in evidence.
- [ ] After authorized tagging/publication, inspect the real Release jobs and
  verify four Go archives/checksums, three OpenAPI assets, both npm packages,
  both PyPI wheel/sdist pairs and both runtime platforms of all three image
  repositories. Verify actual downloads, image identity/runtime checks, legal
  content and existing vulnerability gates. Record the true authentication path;
  neither an upload acknowledgement nor a matching existing package proves a
  fresh OIDC publication.
- [ ] Record the immutable tag/source and artifact evidence in a publication
  verification note. Only then move the shipped changelog items into the
  published release and promote README/site/support availability together.
  Retain checkout-only labels for examples absent from registry artifacts and
  keep historical v0.9.0 records unchanged. Check Pages and protocol mirroring
  separately; neither workflow publishes runner packages.

Preparing this checklist does not create a tag, push source, publish a release
or upload registry artifacts. Those actions remain separate release gates.
