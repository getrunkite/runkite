# Isolated workload usage and durable settlement

Availability: v0.8.0, not a change to published v0.7.0 artifacts. The external
JSONL profile remains experimental. This contract
records claims from a configured workload; it does not verify a provider bill.

Unreleased current source additionally fences terminal accounting and hold
mutations against the original stored record. Apply Postgres/SQLite migration 23
or MySQL migration 24 after backing up and draining all writers, and deploy
matching planes and runners. Existing unbound ledger rows are retained; a
same-public-ID collision requires explicit accounting reconciliation and fresh
operation IDs for intentional new work. See the
[finalization contract](atomic-results-and-identities.md#original-record-finalization-unreleased)
and [upgrade procedure](../ops-runbook.md#assignment-identity-upgrade). The
historical qualification below does not qualify this later fence.

## The caller and the boundary

An operator using the external workload profile needs to distinguish a measured
zero from an unavailable measurement, retain acknowledged partial usage after a
cancel or process crash, and replay a committed result without billing twice.
The ordinary `output.usage` convention was insufficient: cancellation can commit
before a runner's final values arrive, and a workload could supply a dict with
the same spelling as trusted runner metadata.

The trusted parent now accepts a bounded versioned usage frame, commits it to
the existing run's SQL row, and only then acknowledges it to the child. The
normal completion transaction reads that same private snapshot while holding
the same run lock. Existing output parsing, usage events and hold finalization
perform settlement. There is no second billing service or provider-side ledger.

`isolated_workload` identifies the configured profile and reporting channel. It
does not prove that a particular kernel, sandbox or provider produced a count.
An optional verified worker identity records who submitted the claim. A valid
certificate does not make a compromised worker's claimed counts accurate.

## Versioned cumulative frames

A child emits an object such as:

```json
{"type":"usage","schema_version":1,"sequence":1,"status":"reported","complete":false,"prompt_tokens":10,"completion_tokens":4,"model":"example-model"}
```

The parent replies only after durable acceptance:

```json
{"type":"usage_ack","sequence":1}
```

The child should wait for that acknowledgement before treating its report as
recorded. A report which was sent but not acknowledged remains uncertain. The
plane does not delay cancellation waiting for every in-flight measurement.

Each frame reports cumulative counts for this logical execution, rather than a
delta. Child sequences start at one and advance by one. The parent reserves a
separate first SQL sequence for the initial `unknown` snapshot, before launching
any workload. Retries of the exact same frame are idempotent; reuse of a sequence
with changed contents, gaps and regressions fail closed. Reclaim does not reset
the cumulative counts of that execution.

Reported usage requires both `prompt_tokens` and `completion_tokens`, including
explicit zero. Values are nonnegative integers whose sum is at most
9,007,199,254,740,991. Booleans, fractions, negative values and nonfinite costs
are invalid. Optional `cost_usd` is finite and between zero and one trillion;
its presence preserves an explicit reported zero. A model label is at most
256 UTF-8 bytes and remains exact across cumulative reports.

`complete: true` asserts that the workload's measurement is final. It does not
assert that the agent succeeded. No later usage frame or protected tool request
may follow it. A repeated identical final frame can receive the same ack.
Missing reports remain `unknown`, not a reported zero. `unknown` cannot carry
counts, costs or a model and cannot claim completeness.

This version has one aggregate model label. A multi-model workload must provide
its own aggregate cost if it wants such a claim recorded, using a consistent
aggregate label rather than switching labels midway. Unpriced or unreported
cost remains unavailable. There is no automatic provider extraction from
arbitrary tool results, logs or output. `result.output.usage` is reserved and
rejected; send usage through this channel.

## Durable authority and cancellation

The internal HTTP route is `GET` or `POST /internal/runs/{runID}/usage`.
The middleware requires the current assignment; the path must match it. Run,
tenant, thread, incarnation, generation and optional worker ownership come from
that server-verified binding. None can be supplied in the report body. The
durable manifest must select `python-isolated`, directly or through a registered
pool with that framework. This is profile eligibility, not runtime attestation.

`state.RunUsageStore` is an optional SQL capability. Postgres/SQLite migration
22 and MySQL migration 23 add nullable `runs.usage_snapshot`, with no historical
backfill or table rewrite of existing payloads. Normal run metadata updates
cannot replace it. The column is private run state, not a caller-editable map.

The report transaction locks the current run and checks tenant, incarnation,
generation, active status and any stored worker owner before updating it.
Completion, cancellation and reclaim use that same run lock. Therefore a
successfully acknowledged report which committed before terminal completion is
available to that completion, even if the receiving plane has died. Reports
which lose the race to terminal completion are rejected. A stale generation or
same-pool peer cannot overwrite the current report.

The final output's usage is constructed from this private snapshot, overriding
broker values and supplied output fields. Without a snapshot, supplied new
provenance markers become `unknown`/`unverified_output`; they cannot manufacture
a durable-report source or an explicit zero-cost override. The older native SDK
usage convention remains compatible when it does not claim these reserved fields.

Cancellation retains only usage, not an unfinished conversation. The latest
acknowledged cumulative report can be partial. Unreported calls or work after
that report remain unknown. A final receipt replays the committed output and
same report after reclaim, without starting the model or tool again. The
original reporting generation and optional worker proof survive that replay.
An unresolved launch intent still refuses automatic execution retries.

## Settlement and presentation

Canonical `output.usage` adds `reporting_status`, `reporting_source`, `complete`,
`sequence` and `reported_generation`. Optional `reported_workload` preserves the
server-verified reporter's URI, public-key hash and expiry. Unknown or incomplete
measurements set `unmetered: true`. Reported zero and unknown both create durable
observations, with different source labels: `isolated_reported` and
`isolated_unknown`. The settlement ID includes the immutable run incarnation so
retaining an old observation cannot suppress a replacement run with the same ID.

Counts and costs are workload claims. A pricebook can produce an estimate when
the report lacks a cost. These observations can inform normal cooperative
budgets, but they cannot prove a hard provider-spend ceiling against a dishonest
or compromised workload. Keep independent provider quotas and accounting for
that purpose. Passing a clock deadline, cancelling a run or revoking a worker
credential cannot undo a provider charge already incurred.

## Upgrade and qualification

Drain parents and control-plane writers, verify a backup, apply the SQL upgrade
and deploy matching code before using this contract. An old plane fails the new
parent's initial report rather than silently selecting unmetered compatibility.
Mongo does not implement this SQL capability. Old isolated receipts remain
replayable as unknown usage; their arbitrary output is not promoted into a
historical measurement. Do not run mixed versions while claiming complete report
coverage. Downgrade requires a reviewed backup and usage reconciliation.

Unit and SQL conformance tests cover unknown/zero, invalid frames, sequence and
binding conflicts, owner separation, lost acknowledgements, receipt replay,
cross-generation cumulative counts and idempotent terminal settlement. The
combined disposable fixture is `scripts/test-isolated-ha.sh`; see the constrained
execution note for environment and bounded-run evidence. A successful fixture
does not establish live-provider billing, indefinite soak, arbitrary SDK
compatibility or production certification.

On November 14, 2026, the actual combined candidate passed locally on macOS with
OrbStack, two Postgres/Redis planes, direct HTTP/gRPC mTLS, the installed runner
wheel and external OpenShell 0.1.8 with an authenticated TLS gateway. Three
bounded fault cycles completed; the managed test recorded 45.568 seconds.
Each retained acknowledged synthetic usage of seven prompt and three completion
tokens after reporting-plane death, opposite-plane cancellation and restart,
without duplicate settlement. Receipt replay under a different worker preserved
the original reporting generation and identity without another tool effect.
The five gateway TLS/outage probes passed and owned cleanup was confirmed.

The workspace evidence is
`outputs/runkite-next/isolated-ha-tls-final/runtime/managed-evidence.json`,
`gateway-tls-evidence.json` and `runtime-cleanup.json`. This is measured maintainer
qualification, not an independent assessment, hosted CI result, provider test
or long soak. See the [complete scope](constrained-execution.md#current-local-evidence-november-14-2026).
