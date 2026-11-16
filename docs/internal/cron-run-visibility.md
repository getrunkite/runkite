# Cron retained-run visibility

Status: Unreleased, current source. This change adds Admin visibility and exact
tenant navigation. It does not change dispatch, overlap, catch-up, claims or
retry behavior.

## Stored history, not a fire ledger

`DispatchScheduledRun` creates ordinary managed runs on `cron:<schedule-name>`.
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

- `thread_id`: the existing deterministic thread ID.
- `thread_available`: a successful exact-tenant thread lookup.
- `history_status`: `available`, `empty` or `unavailable`.
- Optional `latest_run`: run/agent IDs, SQL incarnation when available, status
  and creation/update timestamps.

The list requires the existing global Admin authority. Each history lookup uses
`tenant.ScopedContext` for that schedule's tenant, masking inherited system
authority. Per-row history errors retain the definitions and produce a generic
unavailable state; detailed store errors are logged server-side. Admin Cron
polls this opt-in list 15 seconds after each completed request. Each schedule
adds one thread lookup and one latest-run query; no background worker or cache
is introduced.
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
each carries `expected_tenant_id` and `expected_incarnation_id`. Deleting a run
and creating a replacement with the same public ID cannot make the old link
display the replacement. A backend without an incarnation shows unlinked status
or run text instead of an ID-only fallback. Schedule table row keys encode the
tenant/name pair as JSON so colons in either identifier cannot collide.

SQL thread IDs are currently global primary keys, while cron schedule names are
unique within a tenant. Two tenants with equal schedule names can therefore
contend for the same deterministic thread. This change fails closed on reads
and navigation; it does not migrate IDs or make those schedules independently
dispatchable. Operators should use distinct schedule names across tenants.

## Scope and verification

No next-fire preview is added. It would need the actual backend cron parser,
timezone fallback and scheduling semantics, rather than a browser-side guess.
No schema migrations, new endpoints, per-fire receipt table, automatic replay
or scheduling behavior changes are included.

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
after deletion/recreation.

Validation in the feature worktree: focused `TestAdminCron*` route/SQLite tests
pass without a network listener, all 96 Admin tests pass, TypeScript/Vite builds
the embedded Admin bundle, and the 43 site tests plus local link checks pass.
Admin lint passes with the existing Fast Refresh warnings in shared component
modules. The full SQLite store suite passes after adding same-snapshot list
incarnations. Live Postgres/MySQL conformance and the broader socket-based API
tests require the parent task's local listener permissions; they were not
completed in this sandbox.
