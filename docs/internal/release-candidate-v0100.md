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

These checks establish neither new-version artifact identity nor publication,
live-provider behavior, unrestricted production soak or independent assessment.
Later source or qualification-harness edits need their affected gates rerun and
their exact source recorded.

## Remaining candidate and publication gates

- [ ] Publish the reviewed final source and qualification changes through the
  approved publication path. Hosted qualification must run on that exact
  published candidate before tagging.
- [ ] Run the [coordinated version gate](../releasing.md#version-consistency-gate)
  for `v0.10.0`, including package/Helm metadata and file-linked example lockfile
  snapshots. Update immutable package README wording before the final build.
- [ ] Qualify the exact assembled candidate in hosted CI, including the pinned
  Langflow baseline and run-only scoped caller, promptfoo baseline and scoped
  native CLI, and AG-UI native delivery/detach/reattach/Stop workflows. Retain
  their separate receipts; the earlier AI SDK cases are not AG-UI qualification.
  Follow the [integrated gates](ecosystem-integration-checklist.md#qualify-the-integrated-contracts)
  and the explicit manual external-runtime qualification boundary.
- [ ] Build fresh 0.10.0 candidate artifacts and inspect what actually ships:
  rebuilt embedded Admin, generated specs, Python wheel/sdist runtime modules,
  TypeScript dist/proto/receipt code, package entrypoints and legal files.
  Requalify installed artifacts and matching image builds. Source qualification
  with previous version metadata does not replace this step.
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
