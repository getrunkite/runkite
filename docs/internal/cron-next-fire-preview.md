# Preview the next cron calendar boundary

Status: Unreleased current source after v0.9.0. This adds an optional Admin
projection and read-only UI guidance. It adds no fire ledger, dispatch API,
schedule mutation, database migration or new scheduler service.

## The existing view and the caller

`cmd/cron.go` already parses configured schedules, polls for due work, claims a
fire and dispatches an ordinary managed run. Admin's existing
`include_history=true` projection shows retained rows on the schedule's thread,
not a durable receipt for every firing opportunity. Its tenant/incarnation
navigation and empty/unavailable states remain separate controls.

The caller is an operator who wants to check the expression and effective
timezone before waiting for a scheduled run. A browser-side cron calculation
could disagree with the Go scheduler, especially around timezone overrides,
daylight-saving transitions or relative intervals. The preview uses the shared
backend parser and reports a single future calendar boundary rather than a
promise of execution.

## Independent opt-ins and response contract

`GET /admin-api/cron` keeps its definition-only shape and read cost. The two
flags are independent:

- `?include_history=true` retains the existing history fields and reads. It
  does not calculate or add a preview unless requested.
- `?include_preview=true` adds `next_fire_preview` without history fields or
  thread/run/claim lookups.
- `?include_history=true&include_preview=true` adds both projections.

The response remains an array of schedule definitions. Each requested
`next_fire_preview` contains `computed_at` in UTC and a `status`. One request
uses one captured computation instant for all schedules. Depending on the
outcome, the object also includes `next_fire_at` in UTC, `effective_timezone`
and `timezone_fallback` when applicable. No input/output/history content is
copied into this object.

The statuses mean:

- `scheduled`: the parser returned a next occurrence strictly after
  `computed_at`. `next_fire_at` is a projected calendar boundary.
- `disabled`: the definition is disabled; no next execution is promised.
- `invalid_expression`: the configured expression could not be parsed.
- `no_future_fire`: the pinned parser returned no future occurrence. This is
  a parser result, not a proof about every future calendar instant.
- `relative_interval_unavailable`: an `@every` schedule needs an execution
  anchor that this definition-only projection does not read. No next time is
  invented from the current request time.

Neither opt-in changes the global Admin authorization boundary. A preview is
computed from the retained definition, not an independently verified live
worker or replica configuration. `/internal/cron` retains its existing
definition-only contract.

## Timezone and parser semantics

The preview and scheduler share `internal/cronschedule`, using the pinned
`github.com/robfig/cron/v3` parser at 3.0.1. Empty configured timezone means UTC.
An invalid configured IANA zone falls back to UTC when that configured location
is used, with `timezone_fallback: true`. A valid expression-level `TZ=` or
`CRON_TZ=` prefix can instead select its own location; a superseded invalid
configured zone does not produce a fallback flag. `effective_timezone` names
the location actually used, including the parser's input-location semantics.
When that returned name is `Local`, it means the control-plane server's local
location, displayed as **Local (server)** in Admin, never the browser's zone.
The parser's expression-level `TZ=Local` uses the location supplied to `Next`,
so an explicit configured IANA location can still determine that outcome.

Calendar descriptors and five-field expressions follow that parser's existing
semantics. DST gaps, folds and date constraints follow its `Next` computation;
the browser does not implement a second interpretation. A successful parse or
preview is not an admission, a lock, a claim or a worker-readiness check.

## Shared parser and scheduler fixes

The shared `cronschedule.Compile` rejects a timezone prefix such as `TZ=UTC`
without a following expression separator, avoiding the pinned parser's panic.
`cmd/cron.go:checkSchedule` now stops on a zero or non-advancing `Next` result
instead of looping indefinitely on an impossible schedule.

For `cron.ConstantDelaySchedule`, the parsed `@every` form, the scheduler keeps
the previous anchor on a not-due poll. After a handled fire, it advances to that
fire's timestamp rather than the later poll time. Handled outcomes include a
successful dispatch, a lost claim for the same fire and a deliberately skipped
busy-thread overlap. Retryable claim/dispatch errors retain the previous anchor.
Advancing on every not-due tick previously starved longer intervals; advancing
to poll time after a handled fire would instead drift their phase.

This preserves a common relative phase when replicas start with a common anchor
and poll at different times. It does not align staggered fresh anchors or create
a global relative epoch. Shared claims deduplicate equal fire timestamps, not
different local phases. Calendar bookkeeping, including backwards-clock
behavior, stays unchanged. For an HA deployment that needs a shared cadence,
prefer a calendar form such as `*/5 * * * *` with the same explicit timezone on
every replica. There is no promise of one fire per relative interval across
replicas that began with different anchors.

The existing tick interval, equal-fire claim mechanism, latest-only catch-up,
overlap handling and transient dispatch retry remain. These are specific
parser/liveness corrections alongside preview, not a promise that `@every`
has acquired globally synchronized scheduling or exactly-once effects.

## Operator use

Open Admin Cron and inspect the next-calendar projection alongside its UTC
computation time and effective timezone. Refresh through the page's existing
list flow when the snapshot is old or the definition changes. This is not a
live countdown, a new polling service or a history query disguised as preview.

An invalid expression requires configuration correction, not a run retry.
Review an indicated timezone fallback before relying on the displayed time.
A disabled schedule remains disabled, even when older retained runs are
visible. For `@every`, inspect the actual scheduling/claim history and logs;
the preview intentionally does not infer the interval's anchor. A missing or
failed projection must not be displayed as a computed next time.
Use an explicit-timezone calendar expression when multiple replicas need a
shared calendar cadence; local relative anchors are not a substitute for it.

When no run appears at the projected boundary, investigate actual scheduler
logs, retained runs, overlap, claims, admission, agent configuration and worker
availability. A busy thread, kill switch, budget, unavailable backend or process
loss can prevent dispatch. A claim can exist without a created run. The
projection cannot distinguish those outcomes or prove exactly-once effects.

## Qualification boundary

Local checks on November 17, 2026 passed the shared-parser and fixed-tick
scheduler regressions using Go 1.26.11. They cover DST, timezone fallback and
expression overrides, malformed timezone prefixes, zero/non-progressing next
times and the relative-only not-due anchor correction. Separate actual Admin
handler checks passed all four history/preview flag combinations, tenant-correct
retained history, preserved read-cost boundaries and a single UTC computation
instant across the response.

The complete Admin test suite and TypeScript/Vite build of the embedded Admin
bundle passed locally, including explicit **Local (server)** display and
unavailable-preview behavior. These focused local results do not claim hosted
CI, release publication, live-provider execution or a distributed relative
schedule epoch. Broader qualification must be recorded against the matching
source; a display snapshot does not establish actual dispatch or catch-up.
The handled-fire phase correction passed a fixed-tick regression against real
SQLite claims, starting two replicas from one durable phase and exercising
unequal poll times, alternating winners and missed-fire collapse. It verifies
one claim per shared fire and an identical following boundary. This deterministic
regression is not native relative-schedule HA qualification or evidence of a
global epoch.

Preview does not add skipped-fire receipts, dispatch guarantees, replay,
backfill, live-provider checks or distributed scheduling qualification. See
[retained-run visibility](cron-run-visibility.md), the
[operator runbook](../ops-runbook.md#cron-calendar-preview-unreleased) and
[public Cron guide](../../site/support/cron.html#next-fire-preview).
