# Cron retained-run visibility

Status: implemented in v0.10.0. The original projection adds Admin
visibility and exact tenant navigation. The tenant-routing follow-up below
also corrects dispatch thread selection. Overlap, catch-up, claims and retry
behavior remain governed by the existing managed-run path.

## Stored history, not a fire ledger

`DispatchScheduledRun` creates ordinary managed runs on a stable schedule thread.
Default-tenant schedules keep `cron:<schedule-name>`. Named-tenant selection is
described below; existing owned legacy threads retain their history.
Admin Cron now displays the latest retained run on that tenant's thread and
links to its existing Run Detail result when a SQL incarnation is available.
A retained thread has a link to Thread Detail. Disabled definitions retain
their history view.

The latest row is selected by the existing `SearchRuns` order (`created_at`
descending, then `run_id` descending) with `Limit: 1`. It is not filtered by the
definition's current agent, because editing a schedule's agent must not hide its
previous run. Other callers can create runs on that same thread; the UI does not
claim that every row was dispatched by cron.

No retained rows means `empty`, not "never fired". Deletion and retention can
remove rows. An error reading the thread or its runs means `unavailable`, not
"dispatch failed". A run that was persisted and then failed may expose its
ordinary durable error status. A failure before run creation and a skipped
overlap have no separate receipt here. Logs remain the operator evidence for
those paths. No output, input, conversation, metadata or provider error is
copied into the new latest-run summary.

## API and tenant boundary

The existing `GET /admin-api/cron` remains definition-only, with the same shape
and no history reads. `?include_history=true` adds:

- `thread_id`: the resolved schedule thread ID, shared with dispatch.
- `thread_available`: a successful exact-tenant thread lookup.
- `history_status`: `available`, `empty` or `unavailable`.
- Optional `latest_run`: run/agent IDs, SQL incarnation when available, status
  and creation/update timestamps.

The list requires the existing global Admin authority. Each history lookup uses
`tenant.ScopedContext` for that schedule's tenant, masking inherited system
authority. Per-row history errors retain the definitions and produce a generic
unavailable state; detailed store errors are logged server-side. Admin Cron
polls this opt-in list 15 seconds after each completed request. Each schedule
adds one latest-run query and bounded exact-tenant thread resolution; no
background worker or cache is introduced. Default threads need one lookup.
Named tenants need at most two, including the legacy continuity check.
SQLite and Postgres run-search projections now retain the SQL incarnation from
the same query snapshot, matching MySQL. No follow-up GetRun can silently bind a
replacement record to an already listed row. Public Run JSON still hides this
internal field; the existing Admin run views and narrow Cron summary expose it.

Admin thread GET, run-list and DELETE now honor an explicitly selected
`tenant_id`. Omitting the query preserves existing global operator behavior.
An explicitly empty selection scopes to the default tenant and never becomes
a system-wide lookup. The UI preserves exact tenant strings and places query
parameters after resource path segments. Thread Detail passes the selected
tenant on every request, including Delete.
Changing the selected tenant remounts Thread Detail's request and confirmation
state so the previous tenant's result cannot remain visible during navigation.
Cron and Thread Detail run links reuse the existing Run Detail identity guard:
each carries `expected_tenant_id` and `expected_incarnation_id`. Run Detail
forwards those selectors to its initial and manual-refresh Admin GET; the server
validates the same fetched snapshot before returning payload. Missing/duplicate
selectors return 400 and a mismatched retained record returns 409. Deleting a run
and creating a replacement with the same public ID cannot make that guarded read
display the replacement. Retained-reference views are read-only snapshots:
live events, cancellation and child-run navigation are disabled because those
existing bare-ID operations cannot remain bound to an incarnation after a read.
A guarded refresh can inspect the same retained record again. Ordinary Run Detail
without retained selectors keeps its existing live controls. A backend without an incarnation shows unlinked status
or run text instead of an ID-only fallback. Schedule table row keys encode the
tenant/name pair as JSON so colons in either identifier cannot collide.

<a id="named-tenant-thread-routing-unreleased"></a>

## Named-tenant thread routing (v0.10.0)

Thread IDs are globally unique, while schedule names and fire claims are scoped
to a tenant. Previously every schedule selected `cron:<name>`. A second tenant
could acquire its own fire claim but fail to create that already-owned thread.
This was an availability failure. Scoped reads already prevented adopting the
other tenant's conversation.

Dispatch and Admin history now share one resolver:

1. The default tenant keeps `cron:<name>` unchanged.
2. A named tenant first checks its canonical `cron-tenant:v1:<digest>` thread.
3. If absent, it reuses `cron:<name>` only when an exact scoped lookup finds an
   existing thread owned by that same tenant.
4. Otherwise it selects the canonical thread. Lookup errors other than not
   found stop resolution; a create conflict never triggers a legacy fallback.

The digest frames the exact tenant and schedule strings before hashing. This
keeps IDs bounded and prevents delimiter ambiguity. Its prefix is disjoint from
`cron:`, so a default schedule name cannot reproduce the generated namespace.
Canonical precedence remains stable if an owned legacy row appears later.
The entire dispatch context is explicitly tenant scoped, even for an exported
caller that supplied inherited system authority.

No thread, run, state or checkpoint is renamed or imported. Reusing an owned
legacy thread preserves its exact checkpoint key and prior conversation.
Named LangGraph tenants still require HTTP proxy checkpoints under the existing
identity rules. Unrelated manually created threads can still conflict with an
explicit ID; that remains a failure rather than permission to change history.

Stop all cron writers for a coordinated upgrade or rollback. An older scheduler
does not understand canonical routing and can split a new named schedule's first
history across legacy and canonical threads during a mixed-version rollout.
If both already exist, inspect both before choosing a conversation; the resolver
prefers canonical and does not merge state. Deleting a thread deletes the basis
for its continuity choice. Back up history before removing or replacing it.

This routing correction adds no schema migration. Older pre-tenancy SQLite and
Postgres databases can retain a single-column schedule-name primary key from
their original schema; this change does not remove that independent constraint.
Configuration-file discovery still bootstraps default-tenant schedules and
requires unique names across discovered files. The fix enables same-name
dispatch for named schedules that the tenant-scoped store already supports.

<a id="acknowledgement-loss-and-fire-claims-unreleased"></a>

## Acknowledgement loss and fire claims (v0.10.0)

A queue can accept a scheduled assignment, let it finish, and then return a
delivery error. Previously the scheduler released the occurrence's fire claim
for every non-conflict dispatch error. A second poll could therefore execute
the same occurrence again even though enqueue cleanup preserved the first
run's committed result. A real SQLite/API regression reproduced two synthetic
effects across two polls before this correction.

Scheduled dispatch now carries an internal retry disposition with the original
run and error chain. Its cleanup reuses canonical `FailUnstarted` completion
against that exact stored record. Only this call's `Accepted=true`,
`Applied=true`, nil-error outcome proves that the original pending run was
failed before any worker attempt could start. A completion replay is not a new
applied transition. Missing identity, replacement, store errors, ambiguous
commits and finalization errors provide no positive retry proof.

The scheduler retains the claim and consumes its anchor for admitted, completed
or uncertain outcomes. Calendar schedules advance to the poll time; relative
intervals retain the handled fire time, as before. Definite precreation errors
and explicit atomic admission-limit rejections still release the claim for
retry. Normal and cached run-write attempts are tracked privately, so an
ambiguous insertion error cannot masquerade as a precreation failure. Existing
busy-thread conflicts keep their overlap behavior. A typed scheduled error is
examined before its wrapped cause, so a queue error wrapping `ErrConflict`
cannot acquire busy-thread semantics by accident.

This uses no latest-thread-run lookup, new fire-to-run table or schema change.
Other callers can still use the same thread. The warning records the known
original run ID for explicit reconciliation; it does not turn claim history
into a dispatch ledger. Ordinary HTTP callers retain their original delivery
errors. Upgrade all cron-serving replicas before relying on the new retry
disposition. Claim retention, schedule edits, process crashes and downstream
effects retain their separate limits; this is not cross-system exactly-once
execution or automatic retry of uncertain work.

The regression covers two polls and a peer claim probe across default,
exact named and owned legacy threads, with calendar and relative anchors.
It checks completed/active/uncertain delivery and proven never-started retry,
including a queue cause wrapping `ErrConflict`. API SQLite cases additionally
cover original-record replacement, missing identity, finalization failure,
ambiguous normal/cache commits, atomic admission rejection and unchanged HTTP
error responses. These bounded provider-free tests are separate from live
transport failures, multi-plane deployment qualification and hosted results.

## Scope and verification

The original retained-run projection does not calculate future fire times.
The separate v0.10.0 [calendar preview](cron-next-fire-preview.md) now uses
the shared backend parser through independent `include_preview=true`, without
adding history reads to preview-only requests or preview work to history-only
requests. It projects one future calendar boundary and cannot prove dispatch,
overlap outcomes or a prior firing. Its parser/scheduler guards are recorded
separately rather than attributed to this original history change.
Neither view adds a schema migration, new endpoint, per-fire receipt table or
automatic replay.

Regression coverage exercises same-named schedules in different tenants,
latest-run ordering after an agent change, disabled schedules, summary payload
minimization, failed reads versus empty history, the definition-only query cost,
and exact-tenant thread GET/list/delete with a foreign globally unique ID.
Admin path tests cover query and path injection, whitespace-preserving tenant
selection, explicit empty scope, colliding colon pairs and the real Run Detail
guard's rejection of a different tenant/incarnation. A SQLite regression captures
the summary, deletes the run and creates a foreign-tenant replacement to verify
that stored link identity differs from the replacement's actual Admin response.
Shared SQL insertion-identity conformance also checks list results before and
after deletion/recreation. A regression validates guarded Admin GET before and
after a real cross-tenant replacement, refusing the replacement's payload while
ordinary global Admin reads remain compatible. UI route tests prove complete and
incomplete retained selections cannot produce stream or cancel request paths.
This closes an observed sequence where initial validation of A was followed by
a bare-ID cancellation of replacement B.

Validation in the feature worktree: focused `TestAdminCron*` route/SQLite tests
pass without a network listener, all 96 Admin tests pass, TypeScript/Vite builds
the embedded Admin bundle, and the 43 site tests plus local link checks pass.
Admin lint passes with the existing Fast Refresh warnings in shared component
modules. The full SQLite store suite passes after adding same-snapshot list
incarnations. SQLite/Postgres run searches now propagate iteration failures and
return no partial rows. A real SQLite view expression yields one valid ordered
row followed by a driver error, proving partial results cannot become successful
history. Live Postgres/MySQL conformance and the broader socket-based API
tests require the parent task's local listener permissions; they were not
completed in this sandbox.

The tenant-routing follow-up passed focused `TestCanonicalCron*`,
`TestCronTenant*` and `TestAdminCron*` cases plus the complete API, shared cron
parser and command package suites on the prepared Go 1.26.11 macOS toolchain.
Real SQLite cases verify exact padded/Unicode/delimiter identities, durable
conversation and checkpoint continuity, legacy/canonical precedence, foreign
collision refusal, busy-thread overlap and backend lookup failures. Definition
and preview-only requests still perform no history lookups. The 43 static-site
tests and local-link checks passed. Hosted Linux and live backend results must
be read separately from these local measurements.

<a id="mysql-cron-claim-insertion-unreleased"></a>

## MySQL cron claim insertion (v0.10.0)

MySQL fire claims use a normal atomic `INSERT` into the existing composite
primary key `(tenant_id, schedule_name, fire_time)`. A native MySQL duplicate-key
error (1062) means the claim already exists and returns `won=false` without an
error. Other backend errors remain errors. A losing claim does not update the
existing row. There is no schema migration or rewrite of retained claims.

The previous no-op `ON DUPLICATE KEY UPDATE` inferred a winner from affected
rows. With the valid driver option `clientFoundRows=true`, an existing claim
also reported one affected row. Both callers could therefore claim the same
occurrence. A busy thread could reject a second overlapping dispatch, but a
quickly completed first run did not prevent a second run for that occurrence.
The insert contract works with either setting; no DSN option is rejected.

`TestCronClaimClientFoundRowsSequentialScopes` and
`TestCronClaimClientFoundRowsConcurrent` exercise the real driver with the option
both disabled and enabled. They cover duplicate losses, twenty contenders,
exact padded tenant scope, separate schedules and fire times, UTC instant
equivalence, release/reclaim and last-fire preservation. The same fixture checks
the existing terminal-hook and busy-thread claim shapes without changing them.
`TestCronClaimClientFoundRowsPreservesBackendErrors` checks that a cancelled
request remains an error and leaves no claim behind. An explicitly configured
unavailable fixture fails; the optional default fixture may skip and cannot
count as a qualification pass.

Maintainer qualification ran the preserved pre-fix and fixed regression binaries against
an owned MySQL 8.4 fixture. The pre-fix binary exited 1: sequential duplicate
claims with `clientFoundRows=true` incorrectly returned `won=true`, and all
twenty concurrent contenders won. The fixed binary exited 0 with all three
tests and five profile cases passing. Both concurrent driver profiles produced
exactly one winner. This records actual native before/after execution, separate
from the broader MySQL suite.

These are store-level claim regressions. They do not establish a complete
multi-control-plane deployment profile, exactly-once business effects or a
per-fire dispatch receipt. Scheduler retry, overlap, retention and latest-only
catch-up behavior retain their existing boundaries.
