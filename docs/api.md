# API

> Deep dive moved from the root README. For a 60-second overview see the [root README](../README.md).

## OpenAPI Specifications

Machine-readable API specs live in [`spec/`](../spec/README.md):

| File | Surface |
|---|---|
| [`spec/openapi.json`](../spec/openapi.json) | Public client API (Agent Protocol v0.1.6 + Runkite extensions) |
| [`spec/openapi-admin.json`](../spec/openapi-admin.json) | Admin API (`/admin-api/*`) |
| [`spec/openapi-internal.json`](../spec/openapi-internal.json) | Internal runner API (`/internal/*`) |

**Call the API today** with the [LangGraph SDK](https://github.com/langchain-ai/langgraph) (`pip install langgraph-sdk`) or any Agent Protocol / OpenAPI client against `spec/openapi.json`. There is no separate `pip install runkite` client yet — see [`docs/client-sdk.md`](client-sdk.md) for the roadmap (thin first-party SDK only if traction warrants it). Tagged GitHub Releases attach the three JSON specs as downloadable artifacts.

Quick create → run → stream (control plane + runner already up; `dev` often has no client auth):

```bash
BASE=http://localhost:2026
THREAD=$(curl -sf -X POST "$BASE/threads" -H 'Content-Type: application/json' -d '{}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["thread_id"])')
curl -sf -N -X POST "$BASE/threads/$THREAD/runs/stream" \
  -H 'Content-Type: application/json' \
  -d '{"agent_id":"echo_agent","input":{"messages":[{"role":"user","content":"hello"}]}}'
```

Regenerate with `make openapi`; verify completeness with `make openapi-check` (also runs in CI).

## API Reference

### Agents
```
POST   /agents/search                          Search/list agents
GET    /agents/{id}                            Get agent details
GET    /agents/{id}/schemas                    Get agent input/output/state/config schemas
GET    /agents/{id}/versions                   List version history (newest first)
POST   /agents/{id}/versions/{v}/rollback      Roll back to an old version's content
PUT    /internal/agents/{id}/schema            Runner reports its real, introspected schema (see below)
```

`GET .../schemas` starts as a `{"type": "object"}` stub for every field the moment an agent is registered (the control plane never loads a runner's graph itself, so it has no way to know the real shape up front) -- the **Python** LangGraph runner overwrites it with the real thing at startup, calling `PUT /internal/agents/{id}/schema` once per static graph (not factory graphs -- their shape depends on per-run context that doesn't exist yet at load time) with `get_input_jsonschema()`/`get_output_jsonschema()`/`get_config_jsonschema()` plus the compiled graph's own `builder.state_schema` converted via pydantic's `TypeAdapter` -- all genuine LangGraph public API, no private/internal fields. **The TypeScript runner still reports the stub** -- LangGraph.js's compiled graph has no public equivalent of Python's `get_*_jsonschema()` methods (only internal, underscore-prefixed fields not safe to depend on across versions), so real introspection isn't implemented there; `getGraph()`/`getGraphAsync()` exist but return the visual node/edge structure for diagramming, not a data schema.

Agents carry a `version` field: starts at 1, increments only when an agent's actual definition (name/description/metadata/capabilities) changes on re-bootstrap -- restarting the control plane with an unchanged `langgraph.json` does not bump it. Every bump writes an immutable snapshot to a separate version-history record (never updated or deleted afterward), so `GET /agents/{id}/versions` can show every definition an agent has ever had. `POST .../rollback` re-applies an old snapshot's content via the same `UpsertAgent` path -- this creates a NEW version whose content matches the old one, rather than deleting or rewriting history (`git revert`, not `git reset`): rolling back from v5 to v2's content produces v6, and v2/v3/v4 remain in history unchanged, showing that a rollback happened rather than erasing what came after it.

**A/B deployment routing**: a client-facing name resolves to one of several REAL, independently-registered agents, weighted-random per run, via `langgraph.json`'s `agent_aliases` section:
```json
{
  "agent_aliases": {
    "my_agent": { "targets": { "my_agent_v1": 90, "my_agent_v2": 10 } }
  }
}
```
Weights are relative, not required to sum to 100 (`{"a": 1, "b": 1}` means an even 50/50 split, same as `{"a": 50, "b": 50}`). A run created against `my_agent` resolves to a real target (`my_agent_v1` or `my_agent_v2`) *before per-agent rate limiting*, agent lookup, or dispatch -- everything downstream sees the real target consistently, never the alias name. (Global / per-user / per-tenant rate limits are HTTP middleware and still run before the body is parsed -- they are not keyed on agent id, so alias resolution does not affect them.) The resulting run's `agent_id` is the resolved target (what actually executed); `metadata.requested_alias` records which alias was asked for, for after-the-fact attribution (e.g. "what fraction of `my_agent`'s traffic actually went to v2").

**Known limitations, stated plainly**:
- **No built-in analytics dashboard.** `metadata.requested_alias` makes "which target did this run use" a queryable field, but comparing conversion/error rates between targets is left to the caller (e.g. via `POST /runs/search` filtered by `metadata.requested_alias`), not a built-in rollup.
- **Rollback only affects future run creations.** It changes what `UpsertAgent` currently serves as the agent's definition; any run already in flight (or already completed) keeps whatever it was actually dispatched with -- there's no retroactive re-execution.
- **Aliases are static config, not runtime-adjustable via API.** Changing a split percentage means editing `langgraph.json` and restarting (or hot-reloading, if the deployment supports that), not a `PATCH` call.
- **Aliases are control-plane-global, not tenant-scoped.** One `langgraph.json` map applies to every tenant -- fine for single-tenant A/B; multi-tenant deployments that need different splits per tenant need separate control planes or a future per-tenant override.

### Registry
```
PUT    /registry/entries/{name}                   Publish (create or new version)
GET    /registry/entries/{name}                   Get current entry
DELETE /registry/entries/{name}                   Unpublish
POST   /registry/search                           Search (name/tags/author)
GET    /registry/entries/{name}/versions          Version history
GET    /registry/entries/{name}/versions/{v}      One specific snapshot
```
See [Agent Marketplace / Registry](registry.md) above for the full design and known limitations.

### Threads
```
POST   /threads                    Create thread
GET    /threads/{id}               Get thread
PATCH  /threads/{id}               Update thread metadata
DELETE /threads/{id}               Delete thread
POST   /threads/search             Search threads
POST   /threads/{id}/copy          Copy thread (fork)
GET    /threads/{id}/state         Get current thread state
POST   /threads/{id}/state         Update thread state
GET    /threads/{id}/history       Get checkpoint history
POST   /threads/{id}/history       Get checkpoint history (with filters)
```

### Runs
```
POST   /threads/{id}/runs          Create run on thread
POST   /threads/{id}/runs/stream   Create and stream run (SSE)
POST   /threads/{id}/runs/wait     Create and wait for completion
GET    /threads/{id}/runs          List runs on thread
GET    /threads/{id}/runs/{runID}  Get run details
GET    /threads/{id}/runs/{runID}/stream   Stream existing run
GET    /threads/{id}/runs/{runID}/wait     Wait for existing run
POST   /threads/{id}/runs/{runID}/cancel   Cancel run
DELETE /threads/{id}/runs/{runID}  Delete run
POST   /runs                       Create background run (no thread)
POST   /runs/stream                Create and stream background run
POST   /runs/wait                  Create and wait background run
POST   /runs/search                Search runs
GET    /runs/{runID}               Get run by ID
DELETE /runs/{runID}               Delete run by ID
GET    /runs/{runID}/stream        Stream run by ID
GET    /runs/{runID}/wait          Wait for run by ID
POST   /runs/{runID}/cancel        Cancel run by ID (query params: wait, action -- see below)
```

`POST .../cancel`'s two Agent Protocol query params (also honored on the thread-scoped `.../runs/{runID}/cancel`): `wait` (bool, default `false`) makes the response wait for the post-cancel grace window that gives the runner a few seconds to emit any final events, instead of backgrounding it -- the run's status is set to `interrupted` synchronously either way, `wait` only changes when the response is sent. `action` (`interrupt` default, or `rollback`) additionally deletes the run record after cancelling it when set to `rollback` -- honest limitation: this does **not** delete checkpoints the way the spec's literal wording describes, since checkpoints in this schema are keyed by `thread_id` (accumulated across every run ever executed on that thread), with no per-run attribution to select just one run's slice, and in direct mode they live in LangGraph's own Postgres tables entirely outside this project's own state store anyway.

### Store
```
PUT    /store/items                Create/update store item (body: ..., ttl_minutes?)
GET    /store/items                Get store item (query params: ns, key, refresh_ttl?)
DELETE /store/items                Delete store item
POST   /store/items/search         Search store items (body: ..., refresh_ttl?)
POST   /store/namespaces           List namespaces
```

### Vector Store
```
PUT    /vectors/items              Upsert a vector item (embed/re-embed)
DELETE /vectors/items              Delete a vector item
POST   /vectors/search             Top-K cosine similarity search
```
501s if `vector_store` isn't configured (see Vector Store section above). Mirrored under `/internal/vectors/*` for a runner's proxy-mode client, same dual-mode convention as `/internal/store/*`.

### Streaming
```
POST   /threads/{id}/stream        Open persistent SSE stream
POST   /threads/{id}/commands      Send command (resume interrupt, etc.)
GET    /threads/{id}/websocket     Bidirectional WebSocket (commands + events, one connection)
```

The WebSocket endpoint is the chatbot use case: one connection instead of a separate command POST and SSE GET. Send the same JSON shape `/threads/{id}/commands` accepts:

```json
{"id": 1, "method": "run.start", "params": {"agent_id": "my_agent", "input": {"messages": [...]}}}
{"id": 2, "method": "input.respond", "params": {"response": true}}
{"id": 3, "method": "run.cancel", "params": {"run_id": "..."}}
```

Each command gets an ack (`{"type": "success", "id": 1, "result": {"run_id": "..."}}` or `{"type": "error", ...}`), and `run.start`/`input.respond` immediately begin pushing that run's events on the same connection -- the same `StreamingEvent` JSON shape SSE uses (`{"type": "event", "method": "values", "seq": 2, "params": {...}}`), replayed from history first so nothing is missed if events were published before the client attached.

### Event Hooks / Webhooks
```
GET    /internal/webhooks/dead-letters   List webhook deliveries that exhausted retries (query param: limit)
```

Observational lifecycle webhooks (`webhooks` in config) cannot reject runs. Sync deny-before-create guardrails use `preflight_hooks` — see the `preflight_hooks` section under Configuration above.

### Custom Routes
```
*      {mount}/*                  Reverse-proxied to custom_routes.url (default mount `/custom`); mount prefix stripped; X-Runkite-* identity headers injected
```

### Cron Scheduler
```
GET    /internal/cron              List configured cron schedules (name, agent_id, expression, timezone, enabled)
```

### Retention

Disabled by default -- runs and checkpoints persist forever until an explicit `DELETE /threads/{id}`, until you configure one or more of these in `langgraph.json`:

```json
{
  "retention": {
    "runs_max_age": "720h",
    "checkpoints_keep_last": 50,
    "cron_claims_max_age": "168h",
    "terminal_hook_claims_max_age": "168h",
    "webhook_dead_letters_max_age": "168h",
    "interval_minutes": 60
  }
}
```

A background loop (same pattern as the cron scheduler) runs immediately on startup and then every `interval_minutes` (default 60) and, for whichever fields are set:
- `runs_max_age` -- deletes TERMINAL runs (`success`/`error`/`interrupted`/`timeout`; `pending`/`running` are never touched regardless of age) whose `updated_at` is older than this duration.
- `checkpoints_keep_last` -- keeps only this many most recent checkpoints per thread in runkite's own `thread_checkpoints` table, deleting older ones. Never touches a thread's current-state snapshot (`threads.values_json`), only its history.
- `cron_claims_max_age` -- deletes old rows from the cron scheduler's fire-dedup table.
- `terminal_hook_claims_max_age` -- deletes old rows from the multi-replica terminal-webhook exactly-once claim table. Only written when webhooks (or other hook sinks) are configured.
- `webhook_dead_letters_max_age` -- deletes webhook dead-letter rows whose `failed_at` is older than this duration.

Each field is independently optional; setting none of them is the same as omitting `retention` entirely. Applies across every tenant (a deployment-wide policy, not something an individual caller sets).

**Known limitation**: `checkpoints_keep_last` only prunes runkite's own `thread_checkpoints` table, used in **proxy mode** (the TypeScript runner, or a Python runner without direct DB access). In **direct mode** (the common Python + Postgres production setup), the runner's checkpointer is LangGraph's own `AsyncPostgresSaver`, which manages its own tables (`checkpoints`, `checkpoint_blobs`, `checkpoint_writes`) independent of runkite's `state.Store` -- this retention feature does not clean those up. LangGraph itself doesn't ship a built-in TTL for its own checkpointer tables either; a direct-mode deployment needing this today would run its own periodic `DELETE` against LangGraph's schema directly.

### Run Timeout

Disabled by default -- a hung agent (alive but stuck, e.g. an infinite tool-call loop) stays `pending`/`running` forever unless a client cancels it, until you configure:

```json
{
  "run_timeout": {
    "max_duration": "30m",
    "interval_seconds": 15
  }
}
```

A background loop runs immediately on startup and then every `interval_seconds` (default 15) and forces any `pending`/`running` run whose `created_at` is older than `max_duration` to status `timeout`. That winner cancels the queue lease, publishes a cancel signal to the runner, releases the thread to `idle` when no other active run owns it, closes the event broker, and fires the same `on_error` hook path as a failed run. Multi-instance safe: N replicas racing the same overdue `run_id` produce exactly one winner (`TryMarkRunTimeout`).

Distinct from crash reclaim (heartbeat lease): reclaim covers a **dead** runner; this covers a **live** one that never finishes. `max_duration` is required for the section to take effect -- absent/empty/invalid disables the sweep entirely. Applies across every tenant (deployment-wide policy).

### Health & Observability
```
GET    /health                     Returns {"status": "ok"} (unconditional, kept for backward compat)
GET    /livez                      Same as /health, under the Kubernetes-conventional name
GET    /readyz                     Actually checks store + transport connectivity -- see below
GET    /metrics                    Prometheus metrics (outside client auth; optional RUNKITE_METRICS_TOKEN)
```

Metrics exposed: `runkite_http_requests_total`, `runkite_http_request_duration_seconds`, `runkite_runs_total`, `runkite_run_duration_seconds`, `runkite_active_runs`, `runkite_queue_depth`, `runkite_active_sse_connections`. HTTP path labels are normalized (UUIDs and resource IDs become `{id}`) to keep cardinality bounded.

`/livez` and `/health` deliberately never fail just because a downstream dependency is unreachable -- restarting this process doesn't bring Postgres back, so a liveness check that reflects dependency health only turns a transient DB outage into a pointless restart-crash-loop. `/readyz` is the one that actually round-trips the state store and job queue (`state.Store.Ping` / `transport.JobQueue.Ping`), and also the event/cancel broker when that's a genuinely separate connection from the queue's own (only the Kafka-queue + Redis-broker/cancelbus combination, since every other pairing shares one connection already covered by the queue check). Returns `503` with a per-dependency `checks` object identifying exactly which one failed, `200` otherwise:

```bash
curl http://localhost:2026/readyz
# {"status":"ready","checks":{"store":"ok","queue":"ok","event_broker":"ok","cancel_broker":"ok"}}
```

Point a load balancer's health check (and Kubernetes' own `readinessProbe`) at `/readyz`, not `/health` -- that's the actual point of the distinction: stop routing traffic to a replica whose database connection died, without also killing/restarting a replica that's otherwise perfectly healthy. `docker-compose.yml` and `docker-compose.multi.yml` both do this already.

### Graceful Shutdown

On `SIGTERM`/`SIGINT`, the control plane stops accepting new work and drains what's already in flight instead of exiting immediately: background loops (queue-depth poller, stale-job reclaimer, cron scheduler, retention, run timeout, store TTL sweep) stop right away since none of them serve a live client request, then the HTTP server stops accepting new connections and lets in-flight requests -- including long-lived SSE run/thread streams and `/runs/wait` -- finish on their own, then the gRPC bridge does the same for in-flight runner RPCs, and finally telemetry is flushed and the store is closed. Live-verified: a `/runs/wait` request that started 2s before `SIGTERM`, for an agent that takes ~6s total, still completes with `"status": "success"` after the signal, rather than being cut off (`test/e2e/graceful_shutdown_test.go`).

The whole sequence has a 15s budget. A runner's `WatchCancels` stream is intentionally long-lived (open for the runner's whole lifetime, not per-run) and will not close on its own just because the control plane is shutting down, so the gRPC side of the drain races `GracefulStop()` against that same 15s budget and force-stops any RPCs still open past it -- in practice this means a shutdown with a connected runner reliably takes close to the full 15s, not near-zero, and that's expected rather than a bug. This is why `docker-compose.yml` and `docker-compose.multi.yml` both set `stop_grace_period: 30s` on the control plane service(s): Compose's own default (10s) would send `SIGKILL` before this budget is ever used. A Kubernetes deployment should set `terminationGracePeriodSeconds` to at least 20-30s for the same reason (its own default of 30s already covers this).

`ReadHeaderTimeout` (10s) and `IdleTimeout` (120s) are set on the HTTP server to close slow-header (Slowloris-style) connections and stale keep-alives; `ReadTimeout`/`WriteTimeout` are deliberately left unset, since either would silently kill a legitimately long-lived SSE/WebSocket/long-poll connection well before this project's own use cases (a multi-minute agent run streamed live) are done with it.

### Logging

`LOG_LEVEL` (`debug`|`info`|`warn`|`error`, default `info`) and `LOG_FORMAT` (`text`|`json`, default `text`) are the same two env vars, with the same values, on the control plane binary and both runners -- set them once in your process manager / container env and every component picks them up consistently.

```bash
LOG_LEVEL=debug LOG_FORMAT=json ./runkite serve --config examples/echo_agent/langgraph.json
LOG_LEVEL=debug LOG_FORMAT=json python -m runkite_runner.worker --config examples/echo_agent/langgraph.json
LOG_LEVEL=debug LOG_FORMAT=json npx runkite-runner --config examples/echo_agent/langgraph.json
```

`json` is the shape a log aggregator (Datadog, Grafana Loki, an OTel Collector's log pipeline, etc.) expects -- every existing log call already passes structured fields (`run_id`, `thread_id`, `error`, ...), so switching the output format is the whole change; no call sites needed touching to get this. `text` (the default) is unchanged from before these env vars existed, so nothing regresses for anyone not setting them.

- **Go control plane**: built on `log/slog`; `LOG_FORMAT=json` swaps in `slog.NewJSONHandler`.
- **Python runner** (LangGraph + all 4 framework adapters -- CrewAI, LlamaIndex, AutoGen, plain LangChain): a minimal stdlib-only JSON formatter, no new dependency.
- **TypeScript runner**: a small logger module (`src/logger.ts`) replacing raw `console.log`/`console.warn`/`console.error` calls; `Error` args are serialized into `{name, message, stack}` in JSON mode instead of being lost to `String(err)`.

### Distributed tracing (OpenTelemetry)

Disabled by default -- zero overhead, no background connection attempts -- until you set `OTEL_EXPORTER_OTLP_ENDPOINT` (or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`):

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317 \
OTEL_SERVICE_NAME=runkite \
./runkite serve --config examples/echo_agent/langgraph.json
```

Every standard `OTEL_EXPORTER_OTLP_*` env var is honored (endpoint, headers for hosted backends' auth tokens, protocol `grpc`/`http/protobuf`, TLS, compression, timeout) -- this is what makes it work with any OTLP-speaking backend (Jaeger, an OTel Collector, Langfuse, Arize Phoenix, Datadog Agent) with zero runkite-specific configuration. Every HTTP request gets a span automatically; each run gets its own child span (`run.id`, `thread.id`, `graph.id`, `run.status` attributes) spanning from creation to terminal status, and the run's real W3C `traceparent` is propagated to the runner in `RunAssignment.trace_context`. Both runners honor the same `OTEL_EXPORTER_OTLP_*` / `OTEL_SERVICE_NAME` env vars and open a `runkite.run` child span under that parent for each job (default service name `runkite-runner`). On `SIGTERM`/`SIGINT`, buffered spans are force-flushed before exit so a redeploy doesn't silently drop the last few seconds of traces.

### LangGraph SDK Compatibility
```
POST   /assistants/search          Alias for /agents/search
GET    /assistants/{id}            Alias for /agents/{id}
GET    /assistants/{id}/schemas    Alias for /agents/{id}/schemas
```
