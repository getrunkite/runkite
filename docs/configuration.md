# Configuration

> Deep dive moved from the root README. For a 60-second overview see the [root README](../README.md).

### langgraph.json

```json
{
  "graphs": {
    "my_agent": "./agent/graph.py:graph"
  },
  "dependencies": ["./agent"],
  "runner_kind": "python-langgraph",
  "auth": {
    "type": "jwt",
    "jwks_url": "https://sso.example.com/.well-known/jwks.json",
    "audience": "my-agent-api"
  },
  "connectors": {
    "salesforce": { "config_ref": "./connectors/salesforce.yaml" }
  },
  "rate_limit": {
    "backend": "redis",
    "global": { "rps": 100, "burst": 200 },
    "per_user": { "rps": 10, "burst": 20 },
    "per_agent": { "rps": 5, "burst": 10 }
  },
  "webhooks": [
    { "url": "https://example.com/hook", "secret": "whsec_...", "events": ["run_complete", "error", "interrupt"] }
  ],
  "preflight_hooks": [
    { "url": "https://guardrails.example/check", "secret": "whsec_...", "timeout_ms": 2000 }
  ],
  "policy": {
    "default_effect": "deny",
    "grants": [
      {
        "id": "acme-sales-sf-read",
        "tenant_id": "acme",
        "agent_id": "sales-assistant",
        "connector": "salesforce",
        "tools": { "allow": ["query", "getRecord"], "deny": ["updateRecord"] }
      }
    ],
    "siem": { "url": "https://siem.example/hooks/runkite", "secret": "siem-hmac" }
  },
  "llm_cache": {
    "my_agent": { "ttl_seconds": 3600 }
  },
  "custom_routes": { "url": "http://127.0.0.1:8100", "mount": "/custom" },
  "cron": {
    "daily-report": {
      "agent_id": "my_agent",
      "expression": "0 9 * * *",
      "timezone": "America/New_York",
      "input": { "messages": [{ "role": "human", "content": "generate the daily report" }] }
    }
  }
}
```

`runner_kind` declares which runner implementation loads and executes every graph in *this* file -- `python-langgraph` (default, omit the field entirely for existing configs) or `typescript-langgraphjs`. One config file maps to one runner process reading it, so this is file-level, not per-graph: a mixed deployment (some agents Python, some TypeScript) uses two separate `langgraph.json` files, each with its own `runner_kind`, and the control plane routes each run to whichever runner declared the target agent -- see Runners below.

`llm_cache` is per-agent and opt-in -- never on by default, since an agent with side effects (e.g. "send an email") must never have its result cached and replayed for a repeated input. The control plane caches whole-run results (keyed by a hash of `agent_id` + `input` + `config`), not individual LLM calls -- it never sees inside a runner's execution to do the latter without becoming framework-aware. A cache hit short-circuits entirely: no queue entry, no runner dispatch, the run comes back immediately with `metadata.cache_hit: true` and the previously-produced output. Resumes (`command.resume`) are never served from or written to the cache -- they continue a specific prior execution, never a fresh cacheable computation. Cache entries expire via `ttl_seconds`; there's no active cleanup sweep for expired rows (they're just never returned), so a very long-running deployment with many distinct inputs would want a periodic `DELETE WHERE expires_at < NOW()` -- not built.

`webhooks` is opt-in and control-plane-wide, same first-file convention as `auth`/`rate_limit`. Each entry gets an HTTP POST for every event type it subscribes to (omit `events` to subscribe to all): `run_start`, `run_complete`, `tool_call`, `error`, `interrupt`, `policy_decision`. The body is the event JSON (`type`, `run_id`, `thread_id`, `agent_id`, `tenant_id`, `data`, `timestamp`); if `secret` is set, requests carry `X-Runkite-Signature: sha256=<hmac>` over the raw body for verification. Delivery retries up to 3 times with exponential backoff (500ms, 1s); a delivery that still fails is persisted as a dead letter, inspectable at `GET /internal/webhooks/dead-letters`. `tool_call` only fires if a runner emits a RunEvent with `method: "tool_call"` -- the control plane doesn't parse LangGraph/LangChain message shapes to detect tool calls itself (staying framework-agnostic); this is a hook a framework-aware runner can choose to populate. `policy_decision` fires from the policy engine on every Decide (including cache hits) when policy is enabled — prefer dedicated `policy.siem` (below) unless you intentionally want one URL for run lifecycle and policy. Terminal events (`run_complete`/`error`/`interrupt`) fire exactly once per run across control-plane replicas: a shared DB claim (`TryClaimTerminalHook`) lets only one replica dispatch when cancel and ReportStatus race across an LB (within one process, an in-memory guard skips a redundant claim). They are not gated on the create replica's in-memory OTel span -- ReportStatus often lands on a different process under multi-replica LB. A claim-store error fail-opens (still dispatches) so a DB blip cannot silence every terminal webhook; receivers may still treat `run_id` as an idempotency key for that rare case.

Dispatch itself runs on a small bounded worker pool (20 workers, a queue depth of 500), not one goroutine per sink per event -- a burst of run completions, each fanning out to every configured sink, each of which can itself hold a delivery attempt open for up to ~30s while retrying an unreachable endpoint, would otherwise grow goroutines/sockets without limit. Still fully non-blocking for the caller either way: if the pool and its queue are both saturated, the event is dropped (logged, and counted in `runkite_webhook_queue_dropped_total`) rather than delaying run execution or growing the queue without bound. One real, documented trade-off from sharing a pool across every sink: a genuinely slow or hung endpoint can occupy enough workers to delay (not silently lose -- the retry/dead-letter path is unaffected once a job is actually picked up) a different, healthy sink's delivery. A well-sized pool makes this rare, not impossible.

Event hooks are also usable directly from Go code by embedding runkite as a library: any type implementing `hooks.Sink` can be registered on the `*hooks.Dispatcher` via `apiServer.SetHookDispatcher`, independent of the webhook config above.

`preflight_hooks` are **synchronous** gates that can **allow or deny** a run **before** any thread auto-create, thread claim, or run row (Promptfoo-style guardrails). They are separate from `webhooks` on purpose: observational webhooks must never delay or block run creation; preflight hooks exist specifically to block. Same first-file / control-plane-wide convention as `webhooks`.

Each entry POSTs a `before_run` event JSON (`type`, `run_id`, `thread_id`, `agent_id`, `tenant_id`, `data.input`, `timestamp`) to `url`, with the same optional `X-Runkite-Signature` HMAC as webhooks. The gate must respond `2xx` with `{"allow": true}` or `{"allow": false, "reason": "..."}`. Deny, non-2xx, timeout, or malformed JSON → **fail closed** (`403` to the client; no thread auto-created for a new `thread_id`, no run persisted, no `run_start` webhook). `timeout_ms` defaults to `2000`. Multiple entries all must allow (first deny wins). Library embedders can `RegisterGate` any `hooks.Gate` implementation on the same Dispatcher.

`policy` is opt-in and control-plane-wide (first-file). When absent or empty, connector access stays V1-open after runner auth + run-binding. When any `grants` or `webhook` is present, `connector.session` and MCP `tools/call` are fail-closed: a grant must match `(tenant_id, agent_id, connector)` from the in-flight run (see [Trust & governance](trust-governance.md)). Optional `webhook` is a sync PolicyProvider (same HMAC / fail-closed culture as `preflight_hooks`, not the async `webhooks` path). Optional `siem` is an **async** export sink for `policy_decision` events only (same delivery / HMAC / dead-letter path as top-level `webhooks`; never blocks Decide). Decisions are written to Postgres `audit_events` when `audit` is true (default); Compatible backends skip durable audit in this release. Every Decide also emits an OTel span event `policy.decide` on the current request span when `OTEL_EXPORTER_OTLP_*` is set.

`rate_limit` is opt-in and control-plane-wide (read from the first discovered `langgraph.json`, same convention as `auth`); any subset of `global`/`per_user`/`per_agent`/`per_tenant` may be set, unconfigured dimensions are unlimited. Each is a token bucket: `rps` is the sustained rate, `burst` is how many requests can arrive back-to-back before limiting kicks in. `global`/`per_user`/`per_tenant` are enforced at the HTTP layer (per-user keyed on the authenticated identity, per-tenant keyed on `tenant_id` -- see Multi-tenancy -- both unlimited when no auth provider is configured, since there's no identity/tenant to key on); `per_agent` is enforced in the shared run-creation path, so it applies uniformly across REST, WebSocket, and streaming-command run starts. Exceeding a limit returns `429` with a `Retry-After` header.

`backend` selects where the buckets live:
- `"redis"` -- shared Lua token buckets on `REDIS_URL`, so N control-plane replicas share one ceiling (required for real multi-instance rate limiting). Fails startup if `REDIS_URL` is unset.
- `"memory"` -- process-local buckets (the historical default). Explicit opt-out of sharing even when Redis is present.
- omit -- auto: uses Redis when `REDIS_URL` is set, otherwise memory.

Redis keys are `rk:rl:{scope}:{id}` (same `rk:*` prefix as the Redis transport). A Redis eval error fails open (allows the request and logs) rather than turning an infra blip into a 429 storm; `/readyz` already surfaces Redis unavailability separately.

`cron` schedules runs on a standard 5-field cron expression (`minute hour day-of-month month day-of-week`), keyed by a schedule name that must be unique across every discovered `langgraph.json` (schedules are bootstrapped from every file, not just the first -- the same convention as `graphs`, not the control-plane-wide `auth`/`rate_limit`/`webhooks`/`custom_routes` singletons). `timezone` is an IANA name (e.g. `America/New_York`); omit it for UTC. `enabled` defaults to `true`; set it to `false` to keep a schedule registered but dormant. Every fire creates a run on a fixed thread `cron:<schedule-name>` -- the same thread every time, so a schedule's run history is browsable as one continuous thread via `GET /threads/cron:<schedule-name>/history` like any other -- and is enqueued exactly like a client-triggered run, so `llm_cache`, rate limiting, event hooks, and everything else in this list apply to it unchanged. If a previous fire's run is still in flight when the next one comes due, the new one is rejected the same way any overlapping run on a busy thread would be (no concurrent runs stack up on one schedule).

The scheduler polls every 15 seconds. A **restarting** schedule (one that has fired at least once before, per `cron_claims`) catches up to the single latest fire missed while the process was down, not a backlog of every missed one -- a catch-up storm isn't what "cron" means to most users. A **brand new** schedule (never claimed before) starts counting from the moment it's registered instead, so adding a schedule doesn't surprise-fire it immediately just because its expression's most recent occurrence already passed before it existed. With multiple control-plane replicas sharing one Postgres database, the `cron_claims` table (`INSERT ... ON CONFLICT DO NOTHING` keyed on `(schedule_name, fire_time)`) guarantees exactly one replica dispatches each fire -- verified live against two real control-plane instances sharing one Postgres + Redis: two consecutive minute-boundary fires, each dispatched by exactly one instance, the other's scheduler loop correctly losing the claim both times. A dispatch that fails transiently (rate limit, momentary store error) releases its claim so the next tick retries the same fire; a dispatch rejected because the schedule's own previous run is still in flight keeps the claim (that occurrence is skipped, not retried) rather than resigning it to overlap with a run that's still busy. This claim table grows by one row per schedule per fire (typically hourly/daily -- slow, but unbounded without cleanup); see the Retention section above's `cron_claims_max_age` for the periodic sweep that covers it. Inspect what's actually registered at `GET /internal/cron`. See `examples/cron_agent/`.

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `HTTP_PORT` | `2026` | HTTP API listen port |
| `GRPC_PORT` | `50051` | gRPC bridge listen port |
| `POSTGRES_DSN` | (unset) | Postgres connection string; enables Postgres state backend |
| `MYSQL_DSN` | (unset) | MySQL connection string (`user:pass@tcp(host:3306)/db?parseTime=true` -- the `parseTime=true` param is required); enables MySQL state backend (checked after `POSTGRES_DSN`, before `MONGO_URI`) |
| `MONGO_URI` | (unset) | MongoDB connection URI; enables MongoDB state backend (checked after `POSTGRES_DSN`/`MYSQL_DSN`, so setting multiple backend env vars at once is deterministic, not a race). **Must point at a replica set** (even a single-node one, e.g. `?replicaSet=rs0&directConnection=true`) -- `UpsertAgent`/`PublishRegistryEntry`/`DeleteRegistryEntry` run inside real Mongo transactions, which a standalone `mongod` rejects outright rather than silently degrading to non-atomic writes |
| `MONGO_DB` | `runkite` | MongoDB database name (used when `MONGO_URI` is set) |
| `REDIS_URL` | (unset) | Redis URL; enables Redis transport (queue + broker + cancel) |
| `NATS_URL` | (unset) | NATS URL; enables NATS/JetStream transport (queue + broker + cancel) -- checked after `REDIS_URL`, ignored if that's also set |
| `KAFKA_URL` | (unset) | Kafka broker address(es), comma-separated for multiple; enables Kafka for the job queue only (see `internal/transport/kafka`'s own package doc for why) -- checked before `REDIS_URL`/`NATS_URL`, so setting both `KAFKA_URL` and `REDIS_URL` means "Kafka queue, Redis broker/cancelbus," not "Redis for everything" |
| `KAFKA_JOB_PARTITIONS` | `1` | Partitions per Kafka job topic, only applied the first time a topic is created -- raise to match/exceed your replica count for genuine concurrent multi-replica dequeue (see the Kafka transport section's own honest default-1 ceiling) |
| `DATABASE_PATH` | `./runkite.db` | SQLite file path (used when none of `POSTGRES_DSN`/`MYSQL_DSN`/`MONGO_URI` is set) |
| `QDRANT_URL` | (unset) | Qdrant REST base URL; fallback for `vector_store.url` when `vector_store.type` is `"qdrant"` (only read if `vector_store` is configured at all -- doesn't enable Qdrant by itself, unlike `POSTGRES_DSN`/`MYSQL_DSN`/`MONGO_URI` for the state backend) |
| `WEAVIATE_URL` | (unset) | Weaviate REST base URL; fallback for `vector_store.url` when `vector_store.type` is `"weaviate"` (same opt-in-only convention as `QDRANT_URL`) |
| `PINECONE_URL` | (unset, defaults to `https://api.pinecone.io`) | Fallback for `vector_store.url` when `vector_store.type` is `"pinecone"`; override to point at a self-hosted Pinecone Local instance |
| `PINECONE_API_KEY` | (unset) | Fallback for `vector_store.api_key`; required for a real Pinecone account, ignored entirely by Pinecone Local |
| `LANGGRAPH_CONFIG` | (unset) | Path to langgraph.json (alternative to --config flag) |
| `RUNNER_TOKEN_<kind>` | (unset) | Shared token for runner auth (e.g. `RUNNER_TOKEN_PYTHON_LANGGRAPH` for kind `python-langgraph`) |
| `RUNNER_TENANTS_<kind>` | (unset) | Optional comma-separated tenant allow-list for that kind's `X-Runkite-Tenant-Id` on `/internal/*` (e.g. `RUNNER_TENANTS_PYTHON_LANGGRAPH=acme,beta`). Unset = any tenant after kind-token auth. Missing header counts as `default`. |
| `LOG_LEVEL` | `info` | `debug`\|`info`\|`warn`\|`error` (case-insensitive). Same variable, same values, on the control plane and both runners. |
| `LOG_FORMAT` | `text` | `text`\|`json`. `json` is the shape a log aggregator (Datadog, Grafana Loki, etc.) expects -- see [Logging](api.md#logging) below. |

### Database CLI

Backend selection matches `serve` (`POSTGRES_DSN` → `MYSQL_DSN` → `MONGO_URI` → SQLite `DATABASE_PATH`):

| Command | Behavior |
|---------|----------|
| `runkite db upgrade` | Apply pending numbered schema migrations (`schema_migrations`). `serve`/`dev` also do this on startup via `Init`. Pre-versioned DBs run baseline Up (self-heal ADD COLUMN) then stamp v1. |
| `runkite db downgrade` | Roll back the most recently applied migration (prints a warning first). Baseline (v1) Down drops application tables/collections (destructive). Exits `1` when already at version 0. |
| `runkite db reset` | Truncate (or delete the SQLite file) and re-upgrade to latest. |

### Vector-store CLI

Requires a `vector_store` section in `langgraph.json` (`--config` / `LANGGRAPH_CONFIG`). Does not share the state `schema_migrations` table.

| Command | Behavior |
|---------|----------|
| `runkite vector upgrade` | Apply pending vector schema (pgvector: numbered Ups via `vector_schema_migrations`; Qdrant/Weaviate/Pinecone: create-if-missing `Init`). `serve`/`dev` also call `Init` when vector_store is configured. |
| `runkite vector downgrade` | Roll back one pgvector migration (baseline Down drops `vector_items`). Exits `1` for backends without a Down step, or when already at version 0. |

Future pgvector schema changes are new numbered Up/Down steps -- not ad-hoc `ALTER` trails inside a single growing `Init`. Dimension changes are still not migrated (drop/recreate).
