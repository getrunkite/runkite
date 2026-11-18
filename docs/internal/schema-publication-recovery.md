# Recover static graph schemas after control-plane registration

Status: implemented in v0.10.0. This is a Python static
LangGraph publication fix. It adds no new framework introspection, schema
provenance field or storage migration.

## The existing report and the recovery gap

`cmd/serve.go:bootstrapAgents` registers agents and stores the four bootstrap
schema fields as exactly `{"type":"object"}`. Re-registering an existing agent
can replace its previously reported schema with those stubs. The Python
LangGraph worker already reported genuine input/output/state/config schemas
through `PUT /internal/agents/{agentID}/schema` after loading static graphs.
A running worker did not repeat that one-time report after a plane restart or
an initial publication failure.

`python/runkite_runner/schema_introspect.py:extract_agent_schema` keeps the
existing public LangGraph methods and Pydantic conversion. Fields fall back
independently when extraction fails. Factories are excluded because their shape
can depend on a run's context. TypeScript and generic/other Python adapters
retain their existing default bootstrap behavior. This change does not infer a
schema from an example run, execute a factory to discover one or add named-tenant
introspection.

## Cache once, recover independently of dispatch

`SchemaPublisher` captures immutable serialized reports from the loaded static
graphs once. Startup retains a bounded compatibility PUT round. A separate
cancellable task starts recovery immediately, then repeats bounded rounds with
a 60-second interval. It runs independently of `GetJob`, including when the
worker is idle or occupied. Shutdown cancels and awaits its owned task/client.
Recovery does not re-import graphs or repeat introspection.

Each cached graph gets at most one initial PUT attempt. If the startup round's
budget runs out before visiting every graph, the background task drains those
unattempted initial reports first. A failed attempt counts as attempted; later
visits use recovery POST, including after a failed initial PUT.

The publisher considers at most 64 static graphs, retains at most 512 KiB per
report and 4 MiB total, and skips reports outside those bounds. HTTP requests
have a five-second bound and each round a 20-second budget. The rotating cursor
prevents a slow graph from permanently starving later reports. Graph
introspection itself is trusted synchronous startup code and has no new
execution deadline or sandbox. These are bounded best-effort rounds, not a
60-second delivery SLA for every graph.

Publication targets the default tenant explicitly and does not inherit an active
assignment's tenant/run context. It does not expand the worker's existing tenant
or pool authority. Both schema publication routes reject a known configured
runner-kind mismatch when authenticated runner-kind context is available.
No-token local mode retains its compatibility behavior.

## Atomically fill only an existing bootstrap row

The new `POST /internal/agents/{agentID}/schema/recover` accepts the existing
`AgentSchema` body. It uses the optional
`state.AgentSchemaRecoveryStore:RecoverAgentSchema` extension, implemented by
Postgres, MySQL, SQLite and Mongo. The tenant-scoped operation atomically replaces
an existing schema row only if **all four stored fields are exact bootstrap
objects**. No row is inserted. Missing, annotated, omitted-field, partial or
otherwise useful reports are not bootstrap rows. An incoming all-stub report
also makes no change.

This preserves reports already published by another worker. Ordinary startup
PUT remains the original replacement operation; it can still replace a report.
After a bootstrap reset, an older compatible same-kind worker can win recovery
first. Neither the store guard nor the MCP content digest establishes which
worker, deployment or code version is current. Useful stale reports require an
explicit operator deployment decision; recovery intentionally does not replace
them periodically.

An older plane returns 404 for the new route. A custom store without the optional
extension returns 501. The worker never falls back from recovery POST to PUT.
Its original startup PUT remains compatible, but automatic recovery requires a
matching newer plane and worker. No DDL or Runner Protocol message is added.

## Operator recovery procedure

1. Confirm the agent uses a loaded static Python LangGraph graph, the intended
   default-tenant registration and a matching runner kind. A factory or another
   SDK profile is not expected to gain real introspection from this fix.
2. Upgrade the plane and Python worker to the matching checkout. Restart the
   worker when deploying changed graph code so its captured report matches that
   loaded process. An old worker does not gain recovery by upgrading the plane.
3. Inspect the actual `GET /agents/{agentID}/schemas` response or Admin Agent
   Detail. Following a registration reset, allow recovery rounds while checking
   fixed diagnostics and HTTP status. Do not infer publication success from the
   configured capability descriptor or from job execution succeeding.
4. For 404, check registration, tenant and plane version. For 403, check configured
   kind, credential and pool scope. For 501, upgrade the custom store to the
   optional atomic extension. Restore HTTP/TLS reachability for bounded request
   failures. Recovery retries cached reports without retrying a run or tool.
5. If only bootstrap stubs remain, check cache limits and field-extraction
   fallbacks. A useful or partial stored report is deliberately retained. Reconcile
   competing worker deployments instead of using recovery to overwrite it.
6. Reconnect stateful MCP clients after a report changes. Their tool catalogs
   remain session snapshots. `freshness: "unknown"` is still correct, and a
   schema content digest is not code attestation or a readiness result.

## Schema data and verification boundaries

The publisher sends definitions, not runtime tool arguments or conversations.
Defaults, examples, enum values and descriptions can nevertheless contain
sensitive text. Keep secrets and customer samples out of schema annotations,
control access to stored/publicly discovered schemas, and avoid publishing raw
schema dumps in support reports. The publisher uses fixed diagnostics or HTTP
status rather than schema bodies, upstream error bodies or exception strings.
That logging choice is not a general redactor for application/framework logs.

Regression coverage includes the atomic store guard, publisher bounds/lifecycle,
compatibility, authenticated kind and pool scope. Shared recovery conformance
passed locally against SQLite, PostgreSQL 16, MySQL 8.4 and a MongoDB 8 replica
set. The opt-in `python/tests/test_schema_recovery_native.py` also passed with a
real SQLite plane and the checkout Python/LangGraph worker. It checks an idle
plane-only restart with the worker PID unchanged, followed by schema-only HTTP
failure and recovery while a single-concurrency graph remains busy beyond its
ordinary reclaim lease. Real heartbeats continue and recovery precedes graph
release without another job poll or a second initial PUT.

The native fixture shortens only the publisher's request/round/interval timings;
unit tests retain coverage of production defaults. It uses synthetic data and no
model provider. `.github/workflows/schema-recovery.yml` runs the same real-worker
check on Ubuntu. The fixture does not qualify a published wheel, a model
provider, multi-replica recovery or long-running soak behavior.

MCP discovery remains advisory. It does not apply defaults, coerce or validate
invocation, grant permission, attest a deployment or refresh an existing
session. See [the discovery contract](mcp-input-schemas.md),
[operator runbook](../ops-runbook.md#static-graph-schema-publication-recovery-unreleased)
and [source integration plan](ecosystem-implementation.md).
