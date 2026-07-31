# Runkite

A Go control plane implementing the [Agent Protocol](https://github.com/langchain-ai/agent-protocol) spec. Framework-agnostic by design -- the server never imports your agent framework, and the Runner Protocol (gRPC) is the only integration point. Shipped runners: Python/LangGraph, TypeScript/LangGraph.js, and (proving the framework-agnostic claim for real) CrewAI, LlamaIndex, AutoGen, and plain LangChain -- see [Framework Adapters](#framework-adapters) below.

## What It Does

```
                         Clients
                 (SDK / Studio / Custom UI)
                           |
                  Agent Protocol HTTP/SSE
                           |
              +---------------------------+
              |    Runkite Control Plane   |
              |         (Go binary)       |
              |                           |
              |  +-------+  +---------+   |
              |  | State |  |Transport|   |
              |  |SQLite/|  |InMem/   |   |
              |  |Postgres| |Redis    |   |
              |  +-------+  +---------+   |
              |                           |
              |  +-------+  +---------+   |
              |  | Auth  |  |Connector|   |
              |  |JWT/Key|  |Registry |   |
              |  +-------+  +---------+   |
              +---------------------------+
                           |
                  gRPC Runner Protocol
                           |
              +---------------------------+
              |     Python Runner(s)      |
              |  LangGraph / CrewAI /     |
              |  LlamaIndex / LangChain   |
              +---------------------------+
```

The control plane owns: HTTP API, auth, streaming, job dispatch, state persistence, connector sessions.
Runners own: graph execution. Your LangGraph agents run unchanged.

## Quick Start

### Prerequisites

- Go 1.25+
- Python 3.12+ (for agents)
- Docker (optional, for Postgres/Redis)

### 1. Build

```bash
git clone https://github.com/sharanharsoor/runkite && cd runkite
make build
```

### 2. Start the control plane

```bash
# Zero-dependency mode: embedded SQLite + in-memory transport
./runkite dev --config examples/echo_agent/langgraph.json
```

Output:
```
  Runkite Control Plane (dev)
  HTTP API:    http://localhost:2026
  gRPC bridge: localhost:50051
  Admin UI:    http://localhost:2026/admin/
  Health:      http://localhost:2026/health
```

### 3. Start a runner

```bash
# Python
PYTHONPATH=python python -m runkite_runner --config examples/echo_agent/langgraph.json

# TypeScript (LangGraph.js) -- see "Runners" below
cd typescript/runkite-runner && npx tsx src/cli.ts --config ../../examples/echo_agent_ts/langgraph.json
```

### 4. Create a run via SDK

```python
from langgraph_sdk import get_client

client = get_client(url="http://localhost:2026")

# Create a thread and run
thread = await client.threads.create()
async for event in client.runs.stream(
    thread["thread_id"],
    "echo_agent",
    input={"messages": [{"role": "user", "content": "hello"}]},
):
    print(event.event, event.data)
```

For a more detailed, step-by-step walkthrough (writing a custom agent, HITL, auth, connectors, enabling Postgres/Redis, production compose), see [`docs/quickstart.md`](docs/quickstart.md).

## CLI Reference

```
runkite dev             Start control plane in dev mode (auto-discovers langgraph.json)
runkite run, serve      Start control plane in production mode
runkite db upgrade      Run database migrations
runkite db downgrade    Not supported yet -- schema is a single idempotent migration, not versioned
runkite db reset        Drop and recreate all tables
runkite agents list     List registered agents from config
runkite version         Print version info
```

Flags for `dev` / `serve`:
```
--port, -p        HTTP port (default: $HTTP_PORT or 2026)
--grpc-port       gRPC port (default: $GRPC_PORT or 50051)
--config, -c      Path to langgraph.json (default: auto-discover in cwd)
```

## Configuration

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
    "global": { "rps": 100, "burst": 200 },
    "per_user": { "rps": 10, "burst": 20 },
    "per_agent": { "rps": 5, "burst": 10 }
  },
  "webhooks": [
    { "url": "https://example.com/hook", "secret": "whsec_...", "events": ["run_complete", "error", "interrupt"] }
  ],
  "llm_cache": {
    "my_agent": { "ttl_seconds": 3600 }
  },
  "custom_routes": { "url": "http://127.0.0.1:8100" },
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

`webhooks` is opt-in and control-plane-wide, same first-file convention as `auth`/`rate_limit`. Each entry gets an HTTP POST for every event type it subscribes to (omit `events` to subscribe to all): `run_start`, `run_complete`, `tool_call`, `error`, `interrupt`. The body is the event JSON (`type`, `run_id`, `thread_id`, `agent_id`, `data`, `timestamp`); if `secret` is set, requests carry `X-Runkite-Signature: sha256=<hmac>` over the raw body for verification. Delivery retries up to 3 times with exponential backoff (500ms, 1s); a delivery that still fails is persisted as a dead letter, inspectable at `GET /internal/webhooks/dead-letters`. `tool_call` only fires if a runner emits a RunEvent with `method: "tool_call"` -- the control plane doesn't parse LangGraph/LangChain message shapes to detect tool calls itself (staying framework-agnostic); this is a hook a framework-aware runner can choose to populate. Firing exactly once per run for `run_complete`/`error`/`interrupt` is guaranteed even when both a cancel and the runner's own status report race for the same run.

Dispatch itself runs on a small bounded worker pool (20 workers, a queue depth of 500), not one goroutine per sink per event -- a burst of run completions, each fanning out to every configured sink, each of which can itself hold a delivery attempt open for up to ~30s while retrying an unreachable endpoint, would otherwise grow goroutines/sockets without limit. Still fully non-blocking for the caller either way: if the pool and its queue are both saturated, the event is dropped (logged, and counted in `runkite_webhook_queue_dropped_total`) rather than delaying run execution or growing the queue without bound. One real, documented trade-off from sharing a pool across every sink: a genuinely slow or hung endpoint can occupy enough workers to delay (not silently lose -- the retry/dead-letter path is unaffected once a job is actually picked up) a different, healthy sink's delivery. A well-sized pool makes this rare, not impossible.

Event hooks are also usable directly from Go code by embedding runkite as a library: any type implementing `hooks.Sink` can be registered on the `*hooks.Dispatcher` via `apiServer.SetHookDispatcher`, independent of the webhook config above.

`rate_limit` is opt-in and control-plane-wide (read from the first discovered `langgraph.json`, same convention as `auth`); any subset of `global`/`per_user`/`per_agent`/`per_tenant` may be set, unconfigured dimensions are unlimited. Each is a token bucket: `rps` is the sustained rate, `burst` is how many requests can arrive back-to-back before limiting kicks in. `global`/`per_user`/`per_tenant` are enforced at the HTTP layer (per-user keyed on the authenticated identity, per-tenant keyed on `tenant_id` -- see Multi-tenancy -- both unlimited when no auth provider is configured, since there's no identity/tenant to key on); `per_agent` is enforced in the shared run-creation path, so it applies uniformly across REST, WebSocket, and streaming-command run starts. Exceeding a limit returns `429` with a `Retry-After` header.

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
| `RUNNER_TOKEN_<kind>` | (unset) | Shared token for runner auth (e.g. `RUNNER_TOKEN_python_langgraph`) |
| `LOG_LEVEL` | `info` | `debug`\|`info`\|`warn`\|`error` (case-insensitive). Same variable, same values, on the control plane and both runners. |
| `LOG_FORMAT` | `text` | `text`\|`json`. `json` is the shape a log aggregator (Datadog, Grafana Loki, etc.) expects -- see [Logging](#logging) below. |

## Auth

Four providers, configured via `auth` in `langgraph.json`:

| Type | Description |
|------|-------------|
| `none` (default) | No auth -- all requests pass through |
| `api_key` | Static API keys with per-key permissions |
| `jwt` | Validate tokens against a JWKS endpoint, extract claims |
| `webhook` | Forward headers to a sidecar service for custom auth logic |

**Authorization**: `permissions` set on an API key, JWT claim, or webhook response are enforced -- `read` is required for GET/HEAD, `write` for everything else (`admin` bypasses both). An **empty** permissions list means unrestricted access (backward compatible -- authenticating without configuring fine-grained permissions keeps full access; you only get restricted by explicitly granting a limited list).

**`admin_keys`** (optional, under the same `auth` object) is an independent credential set accepted **only** for `/admin-api/*` -- every key implicitly grants `admin`. Useful when the primary provider is short-lived SSO and an operator wants a stable break-glass key for the dashboard without minting JWTs. It is additive: a primary credential that itself carries `admin` still works, and a missing/invalid admin key still falls through to a configured primary provider (so a normal SSO user with `admin` permission works too). With **no** primary `auth.type` configured at all, `admin_keys` fails closed on `/admin-api/*` -- a missing/invalid admin credential gets `401`, not the client-facing surface's local-dev trust-everyone. Configuring a credential never leaves the dashboard less protected than configuring none.

**Runner auth** is separate: in local mode runners are trusted implicitly. In production, set `RUNNER_TOKEN_<kind>` env vars -- one shared token per runner type, validated on every gRPC call and `/internal/*` HTTP request.

Example (webhook sidecar):
```json
{
  "auth": {
    "type": "webhook",
    "url": "http://localhost:8090/auth",
    "timeout_ms": 5000,
    "cache_ttl_seconds": 300,
    "cache_max_entries": 10000
  }
}
```

`cache_ttl_seconds > 0` caches a result per (credential, method, path) combination -- for a REST API whose paths embed resource IDs (`/threads/{id}/runs/{id}`), that's effectively one entry per distinct resource a caller has ever touched, not one per caller, so the cache is a size-bounded, TTL-evicting LRU rather than a plain map (`cache_max_entries`, default 10000) -- otherwise it would grow for the lifetime of a long-running control plane under completely normal traffic, not just under abuse.

## TLS / mTLS

Every network hop in this project is plaintext until you configure otherwise -- HTTP, the gRPC bridge, and both runners' calls back to the control plane's HTTP API. TLS is opt-in and env-var-driven, off by default, same convention as `POSTGRES_DSN`/`REDIS_URL`/`RUNNER_TOKEN_*`, not a `langgraph.json` field -- this is deployment infrastructure, not agent config.

**Control plane** (`cmd/serve.go`):

| Env var | Effect |
|---|---|
| `TLS_CERT_FILE` / `TLS_KEY_FILE` | Enables HTTPS on the client-facing HTTP API. Both must be set together. |
| `TLS_CLIENT_CA_FILE` | Additionally requires and verifies a client certificate on every HTTP request (mTLS) -- a second, independent trust boundary from whatever `auth` provider is configured; the two compose. |
| `GRPC_TLS_CERT_FILE` / `GRPC_TLS_KEY_FILE` | Enables TLS on the gRPC bridge. |
| `GRPC_TLS_CLIENT_CA_FILE` | Requires and verifies a runner's client certificate (mTLS) -- a stolen/guessed `RUNNER_TOKEN` no longer suffices on its own to open a connection at all; the TLS handshake itself rejects an uncertified client before the token interceptor ever runs. |

**Runners** (Python and TypeScript, identical env vars and semantics on both):

| Env var | Effect |
|---|---|
| `RUNKITE_TLS_CA_FILE` | Verifies the control plane's server certificate against this CA, **replacing** the system trust store for that verification (not in addition to it) -- required for a self-signed or internal-CA-signed cert. Enables TLS on both the gRPC channel and the proxy-mode HTTP calls (store/vector-store/A2A) at once, since a real deployment signs both with the same CA and a runner only ever talks to one control plane. |
| `RUNKITE_GRPC_TLS` | Enables gRPC TLS using the **system** trust store when `RUNKITE_TLS_CA_FILE` is *not* set -- the gRPC-side equivalent of what an `https://` URL already gives HTTP for free. gRPC has no URL scheme to carry an "I want TLS" signal the way `http://` vs `https://` does, so without this there's no way to ask for "TLS against a publicly-trusted cert, no custom CA needed" on the gRPC side -- only plaintext or TLS-with-a-specific-custom-CA. HTTP needs no equivalent flag: `https://` in `--http-address` is already that signal, and `httpx`/`fetch` both verify against the system trust store by default. |
| `RUNKITE_TLS_CLIENT_CERT_FILE` / `RUNKITE_TLS_CLIENT_KEY_FILE` | This runner's own client certificate for mTLS, when the control plane requires one -- independent of which trust store is in use above. |

Live-verified end to end with self-signed certificates: HTTPS-only (server cert, no mTLS) rejects a plain-HTTP request and accepts HTTPS; mTLS on the HTTP API rejects a request with no client cert or an untrusted one and accepts a CA-signed one; a real Python runner completed a full run over an mTLS gRPC channel plus HTTPS proxy-mode store calls; a real TypeScript runner did the same with mTLS on *both* the gRPC bridge and the HTTP API simultaneously; both runners, given `RUNKITE_GRPC_TLS=1` and no CA file, attempted a genuine system-trust TLS handshake against a self-signed server cert and correctly rejected it (`CERTIFICATE_VERIFY_FAILED: self signed certificate` / `self-signed certificate`) -- exactly the outcome a real publicly-trusted-cert deployment would need to *not* see.

```bash
# Control plane: HTTPS + mTLS on both HTTP and gRPC
TLS_CERT_FILE=server-cert.pem TLS_KEY_FILE=server-key.pem TLS_CLIENT_CA_FILE=ca-cert.pem \
GRPC_TLS_CERT_FILE=server-cert.pem GRPC_TLS_KEY_FILE=server-key.pem GRPC_TLS_CLIENT_CA_FILE=ca-cert.pem \
./runkite serve --config examples/echo_agent/langgraph.json

# Runner: trusts the CA, presents its own client cert for mTLS
RUNKITE_TLS_CA_FILE=server-cert.pem \
RUNKITE_TLS_CLIENT_CERT_FILE=client-cert.pem RUNKITE_TLS_CLIENT_KEY_FILE=client-key.pem \
python -m runkite_runner.worker --config examples/echo_agent/langgraph.json --grpc-address localhost:50051 --http-address https://localhost:2026

# Runner against a control plane with a PUBLICLY-TRUSTED cert (e.g. Let's Encrypt):
# no RUNKITE_TLS_CA_FILE needed for HTTP (https:// already verifies via system trust);
# RUNKITE_GRPC_TLS=1 gives gRPC the same system-trust behavior.
RUNKITE_GRPC_TLS=1 python -m runkite_runner.worker --config examples/echo_agent/langgraph.json --grpc-address controlplane.example.com:50051 --http-address https://controlplane.example.com
```

## Multi-tenancy

Flat tenant scoping -- a full workspace/org/team hierarchy with isolated data was considered, but a flat `tenant_id` is the actual scope built; a hierarchy can be layered on top later via a naming convention without a schema change, and wasn't needed to satisfy anything else already built, including `rate_limit.per_tenant`. Opt-in and fully additive: with no `auth` configured (or a provider that doesn't supply a tenant), every request resolves to an implicit `default` tenant -- exactly today's single-tenant behavior, unchanged.

**Enabling it** is one field on whichever auth provider is already configured:

```json
{
  "auth": {
    "type": "jwt",
    "jwks_url": "https://sso.example.com/.well-known/jwks.json",
    "tenant_claim": "org_id"
  }
}
```

`tenant_claim` (JWT only, default `"tenant_id"`) names the claim to read. For `api_key`, add `"tenant_id"` directly to a key's entry (`"keys": {"key-abc": {"name": "alice", "tenant_id": "acme-corp"}}`); for `webhook`, the auth sidecar's response includes it (`"user": {"identity": "...", "tenant_id": "acme-corp"}`).

**What's isolated**: agents, threads, runs, the store, LLM cache entries, and cron schedules all carry a `tenant_id`, enforced in every query -- a caller only ever sees, lists, updates, or deletes its own tenant's rows; reaching for another tenant's resource by ID returns a plain 404, not 403 (never confirms the ID even exists). Two tenants can independently use the same human-chosen name for an agent, a store namespace/key, or a cron schedule without colliding -- verified live with two real API keys mapped to two tenants: cross-tenant `GET`/`DELETE` by ID both correctly 404, `POST /threads/search` for each tenant returns only its own threads, and a shared runner dispatches both tenants' runs to completion correctly. `rate_limit.per_tenant` (previously a documented no-op) is now a real, independent token bucket per tenant.

**What's control-plane-wide, not per-tenant** (deployment config, not tenant data): `auth`, `rate_limit`, `webhooks`, `custom_routes`, and connector definitions. Agents/cron schedules bootstrapped from `langgraph.json` at startup always land in the `default` tenant -- a config file is one deployment-wide artifact, not tenant-scoped data; a specific tenant's own agents/schedules would need to be created dynamically via the API under that tenant's authenticated context, not static config.

**Known gaps, stated plainly, not hidden**:
- **Direct-mode runner store/checkpoint access always operates as the `default` tenant.** A direct-mode runner (Python or TypeScript) talks straight to Postgres with a raw DB connection, not an authenticated HTTP request -- there's no tenant identity to carry across that boundary without a Runner Protocol wire change. This is a known, documented trade-off of direct mode (see Runners below): proxy mode is the recommended path for real per-tenant store/checkpoint isolation; direct mode bypasses control-plane authz on that data in multi-tenant deployments, not something this feature silently fixed.
- **No central tenant registry.** A tenant "exists" the moment any resource is tagged with it -- there's no `POST /tenants` to pre-create one, list all known tenants, or manage tenant-level settings. Fine for a flat, claim-driven model; would matter if a full org/workspace/team hierarchy is built later. The Admin UI's Overview/Agents/Threads/Runs/Cron views do surface `tenant_id` per row across every tenant (see Admin UI below), which covers *visibility* -- there's just no dedicated tenant *management* surface yet.
- **A pre-existing (upgraded, not freshly created) database keeps its original single-column primary keys** on `agents`/`agent_schemas`/`store_items`/`cron_schedules`/`cron_claims` even after the migration adds `tenant_id` -- SQLite and Postgres both refuse to widen a primary key in place. Explicit unique indexes are created alongside so upserts still work correctly either way; every query still filters by `tenant_id` regardless, so isolation itself holds on both fresh and upgraded databases -- confirmed by running the full conformance suite against a real pre-existing (pre-multi-tenancy) Postgres database, not just fresh ones.

## Admin UI

A web dashboard (React + TypeScript, embedded into the `runkite` binary via Go's `embed.FS` -- no separate deploy step, no Node.js runtime dependency for end users) for operational visibility across every tenant: overview counts, agents, the registry, threads, runs (with a live/replayed SSE event log for debugging a specific run), connectors, cron schedules, and webhook dead-letters.

```
runkite serve --config langgraph.json
# -> Admin UI: http://localhost:2026/admin/
```

**Scope, stated plainly**: this is the *operational* half of "Admin API + UI" -- it does not include user/API-key management. There's no persisted user table today (`api_key` entries are static `langgraph.json` config, not a DB-backed model); building real user CRUD means building that persistence layer first, which is separate work, not a UI feature bolted onto the existing config.

**Auth**: the dashboard's login screen accepts whatever credential the configured `auth` provider expects (an API key or a JWT) and requires the `admin` permission specifically -- `read`/`write` are not enough, even for viewing the dashboard, since every `/admin-api/*` route sees across every tenant. An empty `permissions` list is still unrestricted (the same backward-compatible convention as the rest of the API). Optional `auth.admin_keys` are also accepted on `/admin-api/*` only (see Auth above) and fail closed even with no primary provider configured. With **neither** `admin_keys` **nor** a primary `auth` provider configured at all (pure local/dev mode), the dashboard skips the login screen entirely -- there's no credential to log in with, and the API is fully open, same as the client-facing surface in that mode.

The static UI shell (`GET /admin/*`) is always public at the HTTP layer, same as any web app's frontend bundle -- it contains no data, and gating it would make the login page itself unreachable before a credential exists. The actual gate is the JSON API it calls once you sign in:

```
GET /admin-api/overview                     Summary counts (agents/threads/runs, by status) across every tenant
GET /admin-api/agents                       List agents (tenant_id visible)
GET /admin-api/agents/{id}                  Agent detail
GET /admin-api/registry                     List registry entries (tenant_id visible)
GET /admin-api/registry/{name}              Registry entry detail (?tenant_id= disambiguates a cross-tenant name collision)
GET /admin-api/registry/{name}/versions     Registry entry version history (same ?tenant_id=; omitted merges every tenant's history)
GET /admin-api/threads                      List threads (tenant_id visible; ?status= filter)
GET /admin-api/threads/{id}                 Thread detail
GET /admin-api/threads/{id}/runs            Runs on a thread
GET /admin-api/runs                         List runs (tenant_id visible; ?status=/?agent_id=/?thread_id= filters)
GET /admin-api/runs/{id}                    Run detail
GET /admin-api/runs/{id}/stream             Live/replayed SSE event log for a run (same mechanics as the client-facing stream)
GET /admin-api/connectors                   Connector status, including circuit breaker state
GET /admin-api/cron                         Cron schedules across every tenant
GET /admin-api/webhooks/dead-letters        Failed webhook deliveries
POST /admin-api/runs/{id}/cancel            Cancel a run (any tenant; reuses client handler under system context)
DELETE /admin-api/threads/{id}              Delete a thread (any tenant)
POST /admin-api/webhooks/dead-letters/{id}/redeliver  Re-POST a dead letter's stored payload (unsigned -- secret isn't persisted)
```

Building the UI from source (only needed if you're changing `admin-ui/` itself -- the built output is committed to `internal/adminui/dist/`, so `go build`/`docker build` never need Node.js):

```bash
cd admin-ui && npm ci && npm run build   # builds straight into internal/adminui/dist/
```

## Factory Graphs (LangGraph SDK compatibility)

A `graph.py` export can be a per-request **factory** instead of a plain compiled graph -- needed for agents that require request-isolated state (fresh middleware/tool instances per user, avoiding cross-user state leakage). This implements [LangGraph's own documented factory-graph / `ServerRuntime` convention](https://docs.langchain.com/langsmith/graph-rebuild) -- LangChain's spec for their LangGraph Platform product, not anything specific to one third-party server. Any server that's LangGraph SDK-compatible implements the same spec, which is why a `graph.py` written against it runs here unchanged -- it's written against the LangGraph SDK's own public API, and this is that API, not a proprietary one. Which kind an export is gets decided by **inspecting its parameter names**, not a config flag -- zero changes needed beyond what the LangGraph SDK itself already required:

```python
from langgraph_sdk.runtime import ServerRuntime

# Any of these signatures work, in any parameter order:
def graph(): ...                                  # 0-arg: called once at startup
def graph(config: dict): ...                      # per-run, receives RunnableConfig
def graph(runtime: ServerRuntime): ...             # per-run, receives user/store/access_context
async def graph(config: dict, runtime: ServerRuntime): ...   # per-run, both -- may also be
                                                               # an @contextlib.asynccontextmanager
```

A factory (any variant with `config`/`runtime` params) is called **fresh for every run** -- the Python runner builds a new graph instance, attaches that run's checkpointer/store to it specifically, executes it, and tears it down afterward. A 0-arg callable is called once at worker startup and reused like a plain compiled graph from then on (matching LangGraph's own documented behavior for that variant).

`runtime.user` is populated from whichever auth provider authenticated the HTTP request that created the run (see Auth above) -- forwarded from the control plane through `RunAssignment.user`, so a factory can make per-user decisions (which MCP tools to attach, which store namespace to use) without the control plane needing to know anything about LangGraph's `ServerRuntime` type. `identity`/`display_name`/`is_authenticated`/`permissions` are always present; a `webhook` auth provider's response can include arbitrary additional fields (email, an internal user ID, upstream tokens to forward) that stay reachable via dict-like access (`runtime.user["email"]`) -- nothing beyond the fixed identity/permissions/tenant_id set is silently dropped. `runtime.user` is `None` (and `runtime.ensure_user()` raises `PermissionError`) when no auth provider is configured, matching LangGraph's own documented behavior for an unauthenticated factory call.

See `examples/factory_agent/` for a minimal working example, and `python/runkite_runner/factory_graph.py` for the full implementation notes.

### TypeScript SDK

The TypeScript runner implements the same classification and per-run lifecycle -- `graph.ts` exports work identically, just with JavaScript's own syntax for each variant:

```typescript
// Any of these signatures work, in any parameter order:
export function graph() { ... }                                   // 0-arg: called once at startup
export function graph(config: Record<string, unknown>) { ... }    // per-run, receives RunnableConfig
export function graph(runtime: { user, store, ensureUser() }) { ... }  // per-run, receives user/store
export async function graph(config, runtime) { ... }               // per-run, both -- may also
                                                                     // return an object with an
                                                                     // async dispose() method
```

Since JavaScript has no `inspect.signature`-equivalent for parameter names, classification parses the factory function's own source text (`fn.toString()`) instead -- by the time a `graph.ts` module is imported (`tsx` transpiles on the fly), TypeScript's type annotations are already stripped, so this only ever sees plain parameter names. A destructured parameter (`({ config }) => ...`) contributes no detectable name -- every documented LangGraph SDK factory example names `config`/`runtime` as plain identifiers, never destructured, so this covers the real convention without needing a full JS parser.

LangGraph.js has no published `ServerRuntime` type to construct (unlike Python, which best-effort constructs a real `langgraph_sdk._ExecutionRuntime`) -- the TypeScript runner always passes a duck-typed `ServerRuntimeStandIn` covering the same stable surface (`.user`, `.store`, `.ensureUser()`). There's also no `@contextlib.asynccontextmanager` equivalent to mirror for teardown: a factory that returns an object with an async `dispose()` method gets it called after the run finishes (this package's own convention, not a langgraph_sdk one).

See `examples/echo_agent_ts/factoryGraph.ts` for a minimal working example, and `typescript/runkite-runner/src/factoryGraph.ts` for the full implementation notes.

## Vector Store

Semantic search over embeddings, backed by **pgvector** (Tier 1, SQL-based), **Qdrant**, **Weaviate**, or **Pinecone** (the non-SQL exemplars, same role Mongo plays for the state store -- proof the `VectorStore` interface is implementable against a real standalone vector database, not just a Postgres extension). Disabled entirely by default, same opt-in convention as `llm_cache`/`rate_limit`/`webhooks`/`cron` -- never implicitly enabled just because `POSTGRES_DSN` is set, since an existing Postgres deployment may not have the pgvector extension installed or permitted.

```json
{
  "vector_store": {
    "type": "pgvector",
    "dimensions": 1536
  }
}
```

```json
{
  "vector_store": {
    "type": "qdrant",
    "url": "http://localhost:6333",
    "collection": "vector_items",
    "dimensions": 1536
  }
}
```

```json
{
  "vector_store": {
    "type": "weaviate",
    "url": "http://localhost:8080",
    "class": "VectorItems",
    "dimensions": 1536
  }
}
```

```json
{
  "vector_store": {
    "type": "pinecone",
    "index": "vector-items",
    "dimensions": 1536
  }
}
```

`dimensions` fixes the embedding vector's width at creation time (pgvector's `vector(N)` column / Qdrant's collection vector size / Pinecone's index dimension are all fixed-dimension; Weaviate has no such native constraint, so that backend's width is enforced by Runkite's own code on every Upsert/Search instead) -- defaults to 1536 (OpenAI `text-embedding-3-small`/`ada-002`'s size) when omitted. `pgvector` requires `POSTGRES_DSN` to be set on the control plane; the extension itself (`CREATE EXTENSION IF NOT EXISTS vector`) is created automatically on startup, but the Postgres server must have the pgvector extension binary available -- the `pgvector/pgvector:pg16` image (used by this repo's `docker-compose.yml`/`docker-compose.test.yml`) ships it; a bare `postgres:16` image does not. `qdrant` requires `vector_store.url` or `QDRANT_URL` -- one Qdrant collection holds every tenant/namespace (`tenant_id`/`namespace` are stored as payload fields and included in every filter, not one collection per tenant), and a caller's `(tenant_id, namespace, id)` is mapped to a deterministic UUID v5 for Qdrant's point ID (which must be an integer or UUID, never an arbitrary string). `weaviate` requires `vector_store.url` or `WEAVIATE_URL` -- one Weaviate class holds every tenant/namespace, the same shared-collection convention as Qdrant's, and object IDs are derived the same UUID v5 way; unlike Qdrant's freeform JSON payload, Weaviate requires every property's type to be declared up front, so arbitrary caller metadata is stored as a single JSON-encoded `metadata_json` property rather than one Weaviate property per key.

`pinecone` defaults `vector_store.url` to Pinecone's own fixed control-plane host (`https://api.pinecone.io`) if left unset -- only set `url`/`PINECONE_URL` explicitly to point at a self-hosted [Pinecone Local](https://docs.pinecone.io/guides/operations/local-development) instance for development/testing (this repo's own `docker-compose.test.yml` does exactly that). A real Pinecone account needs `vector_store.api_key`/`PINECONE_API_KEY` set; Pinecone Local ignores whatever key is sent entirely. Unlike Qdrant/Weaviate, no derived UUID or JSON-encoding workaround is needed here: Pinecone vector IDs accept arbitrary strings and its metadata is natively a freeform, directly-queryable object -- a caller's Namespace maps straight onto Pinecone's own first-class namespace concept (not onto a payload/property field the way Qdrant/Weaviate reuse it), while `tenant_id` still goes into metadata and every query's filter, since there's no second Pinecone-native partitioning dimension to spend on it. Search's `filter` is pushed directly into Pinecone's own native `$eq` filter alongside the tenant scope -- no over-fetch-then-filter-in-Go workaround needed the way Weaviate's JSON-blob metadata requires.

**API**: `PUT /vectors/items` (upsert -- overwrites on a repeat `id`, re-embedding a changed document is the common case, not a conflict), `DELETE /vectors/items`, `POST /vectors/search` (top-K cosine similarity, optional exact-match `filter` over metadata) -- identical regardless of which backend is configured. Same dual-mode convention as the key-value store: mirrored under `/internal/vectors/*` for a runner's proxy-mode client. 501s (not 404s) when `vector_store` isn't configured -- "this feature isn't turned on" is a more actionable signal than "this route doesn't exist" for something opt-in.

**Python SDK**: `RunkiteVectorStore` (`python/runkite_runner/vectorstore.py`) implements LangChain's `VectorStore` interface (`add_texts`, `similarity_search`, `similarity_search_with_score`, `from_texts`), so it drops into existing LangChain/LangGraph RAG code unchanged. Prefer proxy mode (`http_base_url` / `RUNKITE_HTTP_URL`) -- it talks to the control plane's `/vectors/*` API and works for every backend (pgvector, Qdrant, …). Direct mode (`postgres_dsn` only, no HTTP URL) queries `vector_items` over `psycopg` and is correct **only** when the control plane is on pgvector; when both DSN and HTTP URL are provided, proxy wins so a runner with `POSTGRES_DSN` set for checkpoints doesn't silently write vectors to Postgres while the CP is on Qdrant. See `examples/vector_agent/` for a working retrieval demo (fake, deterministic embeddings -- no API key needed; always uses proxy).

**TypeScript SDK**: `RunkiteVectorStore` (`typescript/runkite-runner/src/vectorstore.ts`) implements LangChain.js's `VectorStore` abstract class (`addDocuments`/`addVectors`/`similaritySearchVectorWithScore`/`delete`/`fromTexts`/`fromDocuments`) -- same role, **proxy mode only**, deliberately, not a port-in-progress omission: direct mode is only ever correct when the control plane's vector store happens to be pgvector specifically, and proxy mode is correct against every backend the control plane supports because the control plane -- not the runner -- owns that choice. Same reasoning Python's own module docstring gives for why proxy wins whenever both are configured there; this port just always takes the branch Python treats as the safe default.

```typescript
import { RunkiteVectorStore } from "runkite-runner";

const store = new RunkiteVectorStore(embeddings, {
  namespace: "docs",
  httpBaseUrl: process.env.RUNKITE_HTTP_URL ?? "http://localhost:2026",
  runnerToken: process.env.RUNNER_TOKEN,
});
await store.addDocuments([{ pageContent: "...", metadata: {} }]);
const results = await store.similaritySearchWithScore("query text", 4);
```

**Known limitations, stated plainly**:
- **Dimension is fixed at first creation, not migrated**, for all four backends. Changing `vector_store.dimensions` after the table/collection/class/index already exists does not migrate existing rows -- `Upsert` starts failing with a clear dimension-mismatch error (not silent corruption) until it's manually dropped or recreated at the new width.
- **Cosine similarity only.** All four backends support other distance metrics (pgvector: L2, inner-product; Qdrant: Euclidean, dot product; Weaviate: dot product, L2-squared, hamming, manhattan; Pinecone: Euclidean, dot product); only cosine is wired up today, the most common choice for text embeddings.
- **Direct mode is pgvector-only, and Python-only.** There is no runner-side Qdrant, Weaviate, or Pinecone client in either language, and the TypeScript client has no direct-pgvector mode at all (proxy-only by design -- see above), so a non-pgvector-backed deployment or any TypeScript runner always goes through the control plane's HTTP API. Functionally identical to Python's own proxy mode, just always one network hop instead of sometimes zero.
- **Qdrant's, Weaviate's, and Pinecone's `created_at` reset on re-index.** None has a built-in insert-vs-update distinction the way Postgres's `ON CONFLICT DO UPDATE` does, so pinning down "first write time" would need a read before every write (Qdrant, Pinecone), or Weaviate's own upsert-via-delete-then-create (see the package's own doc comment for why: Weaviate's PUT replaces an existing object only, and POST fails if one already exists -- neither alone is a true upsert; Pinecone's own upsert genuinely does overwrite the whole record atomically, but still with no partial-update path that would let created_at survive a resupplied value). `created_at` is set to "now" on every `Upsert` call, including re-indexing an already-existing `id` -- correct for a fresh item, but re-indexing an existing document resets rather than preserves its original `created_at`.
- **Weaviate's metadata `Filter` isn't pushed down into a native query.** Weaviate requires every property's type to be declared up front, so arbitrary Metadata is stored as a single JSON-encoded string property rather than one property per key (see above) -- Weaviate can't filter *inside* that JSON blob. Search instead pages through the tenant/namespace-scoped candidate set in the backend's own similarity order (via GraphQL `offset`), applying the exact-match filter in Go to each page, until `top_k` matches are found or the corpus is exhausted -- exact, not a fixed-size over-fetch window. An earlier implementation used a single fixed window (`top_k * 20`, capped at 500) instead of paging, which is confirmed live to silently return an empty result when a filter's only match sits outside that window (many closer, non-matching vectors + one distant matching one) -- fixed, with a permanent regression test reproducing exactly that shape. The one real remaining bound is Weaviate's own server-side `QUERY_MAXIMUM_RESULTS` default (10,000 combined offset+limit) -- paging that far without a match means Search is honestly no longer exact, a Weaviate-imposed ceiling on offset-based pagination itself, not something this package can page around. Less index-efficient than a native filter for a namespace where the filter matches rarely across a huge corpus (each rare match costs a full page scan). Pinecone doesn't share this limitation -- its metadata is natively freeform and queryable, so its `Filter` is pushed straight into Pinecone's own `$eq` filter.
- **Pinecone's tests never delete their throwaway indexes.** Confirmed live against Pinecone Local that its index deletion is asynchronous (`DELETE` returns `202 Accepted`, not `200`/`204`), and a newly created index can be assigned the exact same host:port a very recently deleted one had before that old index's data is actually cleared -- even after polling `GET` until it 404s, since the index name disappearing from the registry and the underlying port's data actually being torn down turned out to be two separate, unsynchronized events. Rather than fight that race, `internal/vectorstore/pinecone`'s own tests simply never delete what they create -- harmless for Pinecone Local specifically (in-memory, discarded on container teardown), same "a little sprawl instead of any risk of cross-test leakage" trade-off Qdrant's/Weaviate's own tests already accept for their collections/classes.

## Connectors

Pre-authenticated OAuth/MCP sessions for runners. Configured in YAML, referenced from `langgraph.json`:

```yaml
# connectors/salesforce.yaml
auth:
  type: oauth2_client_credentials
  token_url: https://login.salesforce.com/services/oauth2/token
  client_id: ${SF_CLIENT_ID}
  client_secret: ${SF_CLIENT_SECRET}
mcp:
  url: https://salesforce-mcp.internal/sse
errors:
  INSUFFICIENT_ACCESS: "You don't have permission for this Salesforce object."
tools:
  allow: [soqlQuery, getObjectSchema]
  deny: [deleteRecord, bulkUpdate]
```

Supported auth types: `oauth2_client_credentials`, `oauth2_token_exchange`, `api_key`, `bearer`.

Runners call `POST /internal/connectors/{name}/session` to get ready-to-use credentials without implementing auth flows themselves.

### Tool allow/deny enforcement (MCP connectors)

The `tools.allow`/`tools.deny` filter above is a real, enforced gate, not just an advisory hint. `GetSession`'s `mcp.url` points at this control plane's own MCP proxy (`POST /internal/connectors/{name}/mcp`), not the connector's raw downstream MCP server -- so a `tools/call` request passes through here first, where a denied tool is rejected with a JSON-RPC error and **never reaches the downstream server at all**. `tools/list` responses are filtered against the same allow/deny list using the downstream server's own real tool list -- correct even for a deny-only filter (no allow list), which can't be represented in a static preview without knowing the full tool universe the downstream server exposes. An empty/missing tool name is always rejected too, independent of the filter shape (never a legitimate request regardless of what's allowed or denied). Every other JSON-RPC method (`initialize`, `resources/*`, etc.) is forwarded transparently. The connector's existing circuit breaker (guarding token fetches) also guards the proxied MCP call.

**What this does and doesn't cover, precisely**: neither `GetSession` nor `GET /internal/connectors[/{name}]` ever hand out the connector's raw downstream credentials or raw MCP URL when MCP is configured (found and fixed after two of those leaks slipped through review) -- so there's no *Runkite-issued* way for an agent to reach the real server directly instead of through the proxy. What this can't and doesn't claim to cover: if an agent author independently has their own valid credentials or URL for that same downstream service (hardcoded, from an environment variable, from anywhere outside this system), nothing here can stop code using them directly -- that's a fundamentally different threat this feature was never positioned to solve, since Runkite can only mediate access to things it hands out itself.

### Circuit breakers

Every OAuth2 connector (`oauth2_client_credentials`, `oauth2_token_exchange`) gets a per-connector circuit breaker guarding its actual token-fetch network call -- always on, with tunable thresholds:

```yaml
circuit_breaker:
  failure_threshold: 5    # consecutive failures before opening (default: 5)
  cooldown_seconds: 30    # how long to fail fast before a trial call (default: 30)
```

Standard closed/open/half-open state machine: closed passes calls through and counts consecutive failures; opening it makes every call fail fast (`503` + `Retry-After`, no network attempt) until the cooldown elapses; half-open lets exactly one trial call through and closes on success or reopens on failure. `api_key`/`bearer` auth never touches a breaker -- they make no network calls to break on. A cached, still-valid `client_credentials` token keeps being served even while the breaker is open on a since-broken refresh endpoint. Current state is visible at `GET /internal/connectors/{name}` (`circuit_breaker_state`).

## Custom Routes

User-defined HTTP endpoints mounted at `/custom/*` alongside the Agent Protocol API. From the control plane's side, both modes below are the exact same mechanism -- a reverse proxy to `custom_routes.url` in `langgraph.json`, with the `/custom` prefix stripped before forwarding (`/custom/webhook` reaches the target as `/webhook`). Unreachable target returns `502`; unconfigured returns `404`.

**Sidecar mode** (language-agnostic): run any HTTP service yourself, point `custom_routes.url` at it. Works for non-Python routes, or routes that need independent scaling/deployment from the runner.

**In-runner mode** (Python, simplest DX): the runner SDK hosts your ASGI app itself, via `uvicorn`, as a background task alongside its own gRPC poll loop -- "similar to dropping a file in your project":

```json
{
  "custom_routes": { "url": "http://127.0.0.1:8100" },
  "custom_app": { "module": "./app.py:app", "host": "127.0.0.1", "port": 8100 }
}
```

`custom_app.module` uses the same `path:symbol` convention as `graphs`. Works with FastAPI, Starlette, or any ASGI-callable -- `uvicorn` is the only SDK dependency, not a specific framework. WSGI frameworks (e.g. Flask) need an ASGI adapter (e.g. `a2wsgi.WSGIMiddleware`) first, since `uvicorn` only serves ASGI. Because it shares the runner's own process and event loop, a slow custom-route handler can, in principle, delay the runner's own async work -- use sidecar mode instead if a route needs isolation or independent scaling. See `examples/custom_routes_agent/` for a working FastAPI example.

## Agent-to-Agent (A2A)

An agent calls another agent as a sub-task, mid-execution -- native sub-agent delegation via the same Agent Protocol API. The mechanism is deliberately **not** a new protocol surface -- it's the exact same `POST /threads/{id}/runs` + wait-for-result path any client already uses, just reachable from inside a runner's own process via one new internal route (`POST /internal/a2a/runs`) instead of a public one.

**Python SDK**: `call_agent` (`python/runkite_runner/a2a.py`) is what a node calls, using the exact `config` LangGraph already passes it -- everything needed (the calling run's own `run_id`, the authenticated user to forward) is already there:

```python
from runkite_runner.a2a import call_agent

async def coordinator_node(state, config: RunnableConfig) -> dict:
    result = await call_agent(config, "worker_agent", {"messages": [...]}, wait=True)
    ...
```

**TypeScript SDK**: `callAgent` (`typescript/runkite-runner/src/a2a.ts`) is a direct port -- same request shape, same `config.configurable.run_id`/`langgraph_auth_user` forwarding. The runner's `executeRun.ts` now builds this via an exported `buildRunConfig`, which sets the same `configurable` keys Python's `build_run_config` does (`assistant_id`/`graph_id`/`langgraph_auth_user`/`user_id`/`user_display_name`, not just `thread_id`/`run_id` as before) -- one deliberate difference: the TypeScript version builds a fresh config object rather than mutating `assignment.config` in place, which Python's does (harmless either way; nothing compares object identity):

```typescript
import { callAgent } from "runkite-runner";

async function coordinatorNode(state: State, config: RunnableConfig) {
  const result = await callAgent(config, "worker_agent", { messages: [...] }, { wait: true });
  ...
```

See `examples/a2a_agent/` for a complete working example (`coordinator_agent` delegates to `worker_agent`).

Three things this adds on top of the shared run-creation/wait path:

- **Auth context propagation**: the runner forwards the caller's identity/permissions via `on_behalf_of` (both the Python and TypeScript helpers copy them from the parent run's `langgraph_auth_user`, via each language's own duck-typed `to_dict()`/`toDict()` check). Tenant is derived from the PARENT run's own `tenant_id` (looked up server-side), never trusted from the request body. This is **propagation, not enforcement** -- the control plane does not re-check `on_behalf_of.permissions` against a stored parent-auth record (runs don't persist the original caller's auth), so a buggy or compromised agent/runner could claim higher permissions than the parent run actually had. The trust boundary is "the runner is trusted," same as the rest of `/internal/*`.
- **Recursion limits**: every sub-run's `depth` is enforced against `a2a.max_depth` (default 10) at creation time -- an accidental cycle or runaway delegation chain fails fast with `400`, not a resource leak. Configurable:
  ```json
  { "a2a": { "max_depth": 10 } }
  ```
- **Cost attribution**: every *delegated* run carries `parent_run_id` and `root_run_id` (the top of the chain), persisted and indexed. `RunSearchRequest` exposes `root_run_id` as a client-facing filter (`POST /runs/search`) -- pass the tree's root `run_id` (or any descendant's own `root_run_id` value, which is the same thing) to list every OTHER run in the tree with one query. The root itself is never returned this way (its own `root_run_id` is nil by design; fetch it separately by ID), and this filtered search is subject to the same `maxSearchLimit` (100) clamp as any other client-facing search. `GET /runs/{runID}/cost` (below) is more permissive: pass ANY run's ID in the tree -- root or descendant -- and it resolves to the same root internally before aggregating, no client-side root-finding required.
- **Cancel cascade**: cancelling a run cancels everything it delegated to, directly or transitively (not ancestors or siblings) -- a cancelled parent can't leave orphaned children still executing.

**Cost aggregation** (`GET /runs/{runID}/cost`) is deliberately convention-based, not a Runner Protocol change: nothing requires a runner to report token usage today, so this reads whatever a run's own `output` JSON already contains. If it happens to include a top-level `usage` object shaped like most LLM APIs already return it (`prompt_tokens`/`completion_tokens`/`total_tokens`, plus an optional `cost_usd`), it's summed across every run in the tree; a run with no such object just contributes zero. `total_tokens` is filled in as `prompt_tokens + completion_tokens` if a runner reports the two halves but not their sum.

```json
// GET /runs/{any-run-in-the-tree}/cost
{
  "root_run_id": "root-run-id",
  "run_count": 3,
  "usage": {"prompt_tokens": 420, "completion_tokens": 180, "total_tokens": 600, "cost_usd": 0.03},
  "runs": [
    {"run_id": "root-run-id", "agent_id": "coordinator", "depth": 0, "usage": {...}},
    {"run_id": "child-run-id", "agent_id": "worker", "depth": 1, "usage": {...}}
  ]
}
```

**Known limitations, stated plainly**:
- **Concurrency deadlock risk, real and easy to hit**: if the SAME runner process executes both the calling agent and the agent it delegates to, a `wait=True` call blocks that runner's in-flight job slot waiting for a sub-run the same process must dequeue. Default `--concurrency 1` deadlocks on a single hop. **`--concurrency 2` covers one wait-hop**; a deeper wait-chain (A waits on B waits on C) on one process needs roughly `concurrency >= chain_depth + 1`, or another replica of the same `runner_kind`. Horizontally-scaled runner replicas avoid this entirely (any replica can pick up the sub-run).
- **Parent lookup is cross-tenant under runner trust.** `/internal/a2a/runs` resolves `parent_run_id` with system context (no client tenant), then scopes the child to that parent's tenant. A compromised runner that learns another tenant's run UUID could attach a child there -- same "runner is trusted" boundary as other `/internal/*` routes, stated explicitly rather than implied closed.
- **Cost aggregation is best-effort, not authoritative.** It's reading a convention out of existing `output` JSON, not a value the control plane verified or a runner is required to produce -- a misbehaving or silent runner reports zero, not an error.

## MCP Server

The reverse direction of Connectors: Connectors let a Runkite *agent* consume external MCP tool servers (MCP-client); this exposes Runkite's *own* configured agents *as* MCP tools, so any MCP-speaking client (Claude Desktop, Cursor, or your own tooling) can call them directly.

```
POST/GET/DELETE /mcp   Streamable HTTP MCP transport (session-based, per the 2025-03-26/2025-06-18/2025-11-25 spec revisions)
```

Every configured agent becomes exactly one MCP tool, named after its `agent_id`, using its `description` if set. Calling one dispatches a real run through the exact same `createRunCtx` + wait-for-result path every client-facing create-and-wait endpoint already uses (the same pattern A2A reuses for agent-to-agent calls) and blocks until it finishes -- MCP's `tools/call` is inherently request/response, there's no streaming variant to map Runkite's own SSE/WebSocket paths onto.

```json
// tools/call arguments
{
  "message": "What's the weather in Boston?",   // wrapped as a single user turn -- the common case
  "input": { "messages": [...] },                 // OR: raw input, for a caller that knows this agent's own shape. Wins if both given.
  "thread_id": "existing-thread-id"                // optional: continue a conversation instead of starting fresh
}
```

The result's text content is the agent's last message; a run that errors or gets interrupted comes back as a **tool-level** error (`isError: true` in the content, not a raw MCP protocol failure) so the calling LLM can actually see what went wrong and self-correct, per the MCP spec's own guidance on tool errors.

Mounted as a normal client-facing route (`/mcp`, not under `/internal/*`), so it goes through the exact same auth middleware (API key/JWT) as every other endpoint -- an MCP client is configured with a Runkite API key exactly the way any other Agent Protocol client would be. A session's tenant is fixed for its lifetime, resolved once at session establishment (the natural behavior for how MCP clients are actually configured in practice -- one static key per configured server entry) -- see `internal/api/mcpserver.go`'s doc comment for why the request context itself, not just the tenant/identity it carries, can't be reused across a session's later calls.

Uses the stateful (session-ID-based) Streamable HTTP transport rather than the newest stateless-only mode, since real-world MCP clients today predominantly still speak the older, session-based protocol revisions -- the official Go SDK (`github.com/modelcontextprotocol/go-sdk`) negotiates the right one automatically per client.

**Session security: bound to whoever created it.** The MCP Go SDK has its own session-hijack protection, but it keys off an OAuth-specific field none of Runkite's auth providers (API key, JWT) ever populate -- silently making that check a no-op here. Closed with Runkite's own middleware instead: the tenant + identity that established a session is recorded when it's created, and every later request against that same `Mcp-Session-Id` must match, or it's rejected with `403` -- confirmed live: a second, otherwise perfectly valid credential presenting a leaked or guessed session ID from a DIFFERENT tenant is rejected outright, not silently continued under the original tenant. This ownership record is also given an explicit TTL (30 minutes idle, swept every 5) so it doesn't grow unbounded over a long-running process -- and the SDK's own `StreamableHTTPOptions.SessionTimeout` is set to expire *before* that TTL can ever elapse (25 minutes), not left at the SDK's default of "never." A review caught this ordering as a real gap during verification: leaving the SDK's own session immortal while Runkite's ownership record still expired on schedule meant the hijack window reopened after 30 minutes of activity instead of never -- the ownership record would be swept, and the (still-alive) SDK session would then answer a mismatched caller's request instead of ever reaching the check. Fixing it just meant configuring the SDK's own idle timeout deliberately rather than leaving it unset.

**Multi-instance: `/mcp` needs sticky routing, unlike the rest of the API.** Session state (the SDK's own, and Runkite's session-ownership tracking above) lives in one replica's process memory -- a client's `initialize` landing on replica A, then a later call for that same session landing on replica B via plain round-robin, gets a genuine `404 session not found` from B. `docker-compose.multi.yml`'s `nginx-multi.conf` handles this with `hash $remote_addr consistent` on a dedicated `/mcp` upstream (open-source nginx, no nginx-plus or external session store needed) -- confirmed live, including a real pitfall found while testing it: hashing on the session ID itself (the more obvious-seeming choice) does NOT work, since that ID is an opaque, effectively random string a replica only assigns AFTER already being picked by some OTHER key, so its own hash value has no relationship to which replica actually holds it -- confirmed by reproducing the exact failure this way before switching to the client-address-based hash, which is consistent across a session's entire lifetime by construction (the key never changes), not by chance. If you're running a self-managed multi-instance deployment without this nginx config, you need the equivalent (or a single `/mcp`-serving replica) for MCP sessions to survive longer than one request.

## Agent Marketplace / Registry

A searchable catalog of agent definitions -- publish, discover, and deploy agent definitions. **Minimal viable registry** scope, chosen deliberately: publish/search/get/version-history via API and Admin UI, no security review workflow, and no automatic clone-and-execute deploy pipeline. A registry entry is metadata plus a `source_ref` -- a git URL, a plain URL, or an inline `langgraph.json` snippet -- pointing at where a human (or their own tooling) goes to actually wire it into a deployment. This is deliberately a catalog, not a package manager's install step: running arbitrary fetched code is a fundamentally different trust/sandboxing problem than a searchable listing, and out of scope here.

```
PUT    /registry/entries/{name}                   Publish (create or new version)
GET    /registry/entries/{name}                   Get current entry
DELETE /registry/entries/{name}                   Unpublish
POST   /registry/search                           Search (name substring, tags -- must have ALL, author -- exact)
GET    /registry/entries/{name}/versions          Version history, newest first
GET    /registry/entries/{name}/versions/{v}      One specific historical snapshot
```

```json
{
  "display_name": "Sales Qualifier",
  "description": "Qualifies inbound leads using firmographic data",
  "author": "alice",
  "tags": ["sales", "lead-gen"],
  "source_type": "git",
  "source_ref": "https://github.com/example/sales-qualifier@main"
}
```

Versioning follows the exact same convention as agent versioning above: publishing unchanged content doesn't bump the version, an actual content change does, and every bump writes an immutable snapshot to its own history (append-only, `git revert` not `git reset` -- see the versioning section above for the full rationale, identical here). A registry is private to the tenant that published it ("an internal/private registry for one's own agents," not a shared public catalog across tenants), same isolation convention as agents/threads/runs. Browsable at `/admin/registry` in the Admin UI.

**Known limitations, stated plainly**:
- **No security review workflow.** Anything published is immediately visible to every caller in that tenant -- there's no approve/reject step, unlike a real package registry's review queue.
- **No deploy automation.** Publishing an entry doesn't register a runnable agent -- `source_ref` still requires a human (or separate tooling) to actually wire the code into a `langgraph.json` and restart/reload the relevant runner. The registry is a catalog, not a deployment pipeline.
- **Admin lookups by name alone are ambiguous under a cross-tenant name collision.** `registry_entries`' real key is `(tenant_id, name)`, not `name` -- if two tenants both publish "sales-bot", `GET /admin-api/registry/{name}` (system context, no tenant filter) returns an arbitrary match, and `GET /admin-api/registry/{name}/versions` genuinely merges both tenants' histories into one list. An explicit `?tenant_id=` query param on both routes disambiguates by scoping to one tenant; the admin version response also exposes `tenant_id` per entry so a merged (no-param) response is at least distinguishable after the fact. Client-facing routes are unaffected (already tenant-scoped from the caller's own auth context, same as every other resource).

`PublishRegistryEntry` and `DeleteRegistryEntry` run inside a real Mongo transaction on the MongoDB backend (same as agent versioning's `UpsertAgent` -- see State Backends below), not the non-atomic entry-table-update-then-version-insert this section used to describe as a limitation. Requires Mongo to run as a replica set (`docker-compose.test.yml`/CI now do); a standalone `mongod` rejects the transaction outright rather than silently degrading.

## Architecture

**Control plane** (Go, single static binary):
- Full Agent Protocol HTTP/SSE surface
- State persistence (SQLite default; Postgres, MySQL, or MongoDB opt-in)
- Transport layer (in-memory default, Redis opt-in)
- Auth engine (JWT, API key, webhook, plus a separate runner-token tier for the gRPC bridge)
- Connector/MCP registry
- Prometheus metrics (`/metrics`)
- Job dispatch (gRPC long-poll)

**Runner Protocol** (gRPC, versioned):
- Runners pull jobs from the control plane
- Execute agent graphs (LangGraph `astream()` / LangGraph.js `stream()` -- see Runners below)
- Stream events back (values, updates, messages, lifecycle)
- Support interrupt/resume for HITL

**State backends**:
| Concern | Default | Production |
|---------|---------|------------|
| Metadata (agents/threads/runs) | Embedded SQLite | Postgres, MySQL, or MongoDB |
| Job queue + event broker | In-memory | Redis or NATS (JetStream) for both; Kafka for the queue only, paired with Redis or in-process for the broker/cancelbus |

Switch backends by setting `POSTGRES_DSN`, `MYSQL_DSN`, `MONGO_URI`, `REDIS_URL`, and/or `NATS_URL`. No code changes, no config files. MongoDB (`internal/state/mongo`) is the project's non-SQL exemplar backend -- proof `state.Store` is genuinely implementable against a document store, and a template for community-contributed backends. It passes the identical conformance suite Postgres and SQLite do; `UpsertAgent`/`PublishRegistryEntry`/`DeleteRegistryEntry` run inside real Mongo transactions, so the connected Mongo **must be a replica set** (even a single-node one) -- a standalone `mongod` rejects the transaction outright. One caveat: the Python/TypeScript runners' own direct-mode checkpointer (`AsyncPostgresSaver`/its JS equivalent) only exists for Postgres -- MongoDB-, MySQL-, and SQLite-backed control planes' runners all use **proxy mode** for checkpoints/store (see below); Postgres is the only backend with a direct-mode option at all. MySQL (`internal/state/mysql`) is the second SQL exemplar alongside Postgres/SQLite -- same conformance suite, fully wired into `runkite serve`/`db upgrade`/`db reset` via `MYSQL_DSN`; DynamoDB remains a documented possible future driver, not built at all.

### NATS transport (JetStream)

`internal/transport/nats` is the second full transport implementation, checked with the exact same `internal/transport/conformance` suite Redis's own transport is (JobQueue, EventBroker, and CancelBroker, all three -- Kafka, added later, deliberately only implements JobQueue; see its own package doc for why NATS specifically earns the full triad and Kafka doesn't). Wired the same way as Redis: set `NATS_URL` (e.g. `nats://localhost:4222`), no code changes.

The queue side genuinely differs from Redis's, not just in wire format: NATS JetStream gives two of this project's own three crash-recovery problems natively -- `AckWait` (a per-message redelivery timer, closing the same gap Redis's hand-built lease-and-reaper closes) and `InProgress` (a heartbeat call that resets that timer without acking, backing `Renew` directly). Fencing is the one piece NOT native, confirmed against a real, still-open NATS issue ([nats-server#4786](https://github.com/nats-io/nats-server/issues/4786)): JetStream currently accepts an ack arriving after a message has already been redelivered to a different consumer -- the exact "stale runner's late report clobbers the current attempt" gap Redis needed generation fencing for. Closed the same way conceptually, but for free where Redis needed a hand-rolled counter: a message's own `NumDelivered` (1 on first delivery, incremented natively by the server on every redelivery) *is* this backend's fencing generation. Shared, cross-replica in-flight tracking (the same requirement Redis's own item-16 fix imposed) is a JetStream KV bucket holding each in-flight run's reply subject -- a NATS reply subject is just a string, so any control-plane replica holding it can publish an ack/nak/in-progress signal directly, without needing the original connection that received the message.

Passes all 26 conformance tests, but not on the first attempt -- two real, live-found bugs along the way: comparing JetStream sentinel errors with `==` instead of `errors.Is` silently never matched (they come back wrapped), breaking not-found handling in several places at once; and `Replay`'s first implementation reused the same `OrderedConsumer` type `Subscribe` uses for its live tail, which turned out to have self-healing reset/retry logic that doesn't behave well driven by repeated one-shot `FetchNoWait` calls -- it hung retrying an internal consumer reset indefinitely. Fixed by giving `Replay` its own plain ephemeral consumer instead, explicitly deleted when done, with none of `OrderedConsumer`'s continuous-tail machinery to misbehave. Also needed a config-file-only server setting (`max_payload: 8MB` in `nats-server.conf`, mounted into `docker-compose.test.yml`'s `nats` service): the server's own default (1MB) is smaller than this project's own large-payload conformance test, confirmed live via a genuine "maximum payload exceeded" rejection before the fix.

Known, honest gap versus Redis: `EventBroker.Close` is a documented no-op here, not an "immediately closed channel for a late subscriber" marker the way Redis's `closedKey` gives -- a `Subscribe` call after a run's already-terminal event returns an open channel that silently never receives anything, rather than a channel a caller can distinguish as already-done via a receive-with-ok check. Every current caller already has its own independent terminal-status check against the run's stored state, so this doesn't strand anything in practice, but a future caller relying solely on this channel's own closure to detect "already done" would be wrong to assume parity with Redis here.

### Kafka transport (JobQueue only)

`internal/transport/kafka` is a third `JobQueue` implementation -- deliberately JobQueue-only, not the full triad Redis/NATS give: Kafka has no pub/sub primitive suited to `EventBroker`'s fan-out-plus-replay shape or `CancelBroker`'s fire-and-forget signal, and bolting one on top of Kafka's own log model would mean reinventing a second, unrelated technology rather than using Kafka for what it's good at. Set `KAFKA_URL` (e.g. `localhost:9092`, comma-separated for multiple brokers) to pick Kafka for the queue; pair it with `REDIS_URL` for `EventBroker`/`CancelBroker` (both set = Kafka queue + Redis broker/cancelbus), or leave `REDIS_URL` unset to fall back to the in-process broker/cancelbus (single-instance only, same caveat the in-process transport always carries).

Kafka's own redelivery model is fundamentally different from Redis's and NATS's: it only tracks one committed offset per partition, and committing offset N implicitly commits everything below it too (a documented `segmentio/kafka-go` behavior) -- there's no way to redeliver one specific message out of a partition's sequence via Kafka's own offset mechanism alone. Rather than force a per-message lease onto that model, this backend treats Kafka's own offset commit as a cheap "how far can a fresh consumer skip on restart" hint, not the authoritative record of whether a job is done -- `Dequeue` writes a durable state-topic entry for the job **before** committing its offset (see below for why the order matters), and a separate, single-partition, log-compacted topic (`<namespace>.state`) is the actual source of truth for what's in-flight, its fencing generation, and its full payload for re-delivery. Every control-plane replica tails that compacted topic from its own beginning at startup and keeps an in-memory materialized view (the log is the source of truth, the map is a derived, disposable index any replica can rebuild) -- what makes Ack/Renew/Nack/ReclaimStale usable from any replica regardless of which one's Fetch call actually received a given message, the same "shared, not process-local" requirement Redis's own item-16 fix imposed. Fencing generation is hand-rolled here too, same reasoning as NATS's own writeup: Kafka's consumer-group generation ID is a group-membership/rebalance concept, not a per-message counter, and using it directly would require staying on the exact connection that fetched a message -- generation instead lives in the state-topic entry, bumped by `ReclaimStale`/`Nack` via a real `json.Unmarshal`/`Marshal` round trip (not string surgery the way Redis's own Lua-side bump needed).

**Multi-replica dequeue needs `KAFKA_JOB_PARTITIONS` (default 1, honestly).** Kafka's consumer-group protocol only ever assigns a given partition to one group member at a time -- with the default of 1 partition per job topic, only ONE control-plane replica actually dequeues a given `runner_kind` at once; other replicas' own `GetJob` calls simply see an empty queue (not an error, and not a correctness gap -- Ack/Renew/Nack still work from any replica via the shared state topic) until Kafka rebalances the partition to them, e.g. if the currently-assigned replica dies. Set `KAFKA_JOB_PARTITIONS` (wired to `kafkatransport.WithJobPartitions`) to more than 1, matching or exceeding your replica count, for genuine concurrent multi-replica dequeue throughput -- confirmed live: a fresh job topic created with `WithJobPartitions(3)` reports `PartitionCount: 3` via `kafka-topics.sh --describe`. Only takes effect the first time a job topic is created; an already-existing topic's partition count doesn't change retroactively, so set this consistently across every replica sharing a cluster.

Known, honest trade-off versus Redis/NATS: neither the state-topic's read-modify-write (an in-memory fencing check, then a produce) nor two replicas' reaper ticks racing on the exact same stale run_id are atomic the way Redis's Lua scripts or NATS KV's revision-checked `Update` are -- Kafka alone has no compare-and-swap primitive to close that race with. In the rare case of two replicas reclaiming the identical run_id within the same tick, both could re-produce it, briefly double-dispatching one job. Confirmed live (killing a runner mid-`slow_agent` execution against a real Kafka broker, Postgres, and Redis, re-verified after the fixes below): the reaper correctly reclaimed the stale job (`reclaimed stale jobs count=1 max_age=6s`) and a second runner picked it up and completed it successfully.

Passes all 15+ conformance tests, live-verified end-to-end, but real bugs surfaced along the way at every review pass, all confirmed against a real single-node KRaft broker, not assumed: (1) `ReclaimStale`/`Nack` were re-producing a job's original, unmodified bytes -- since Kafka (unlike Redis/NATS) has no native redelivery counter, a job's own embedded `generation` field is what `Dequeue` trusts, and re-producing without updating it meant every redelivery silently looked like generation 1 again, defeating fencing entirely; fixed by unmarshaling, bumping, and re-marshaling the payload before every re-produce. (2) The consumer-group id was scoped only by `runner_kind`, not also by this backend's own topic namespace -- two independent `Queue`s pointed at the same broker with different namespaces but the same literal `runner_kind` string collided on group membership, corrupting each other's offsets; fixed by folding the namespace into the group id too. (3) `kafka-go`'s default `Writer`/`Reader` both cap message size at 1MB independent of the broker's own `message.max.bytes`, silently rejecting or truncating this project's own large-payload conformance test until raised explicitly (`BatchBytes`/`MaxBytes`, 8MB, matching NATS's own `max_payload` fix). (4) A closer external review caught `Dequeue` committing a message's offset *before* writing its state-topic entry -- if the process died or that write failed in between, the job was neither Kafka-redeliverable (offset already committed) nor reclaimable (no state entry), silently lost for good; fixed by reordering so the state write happens first, so a failure in that window instead leaves the message uncommitted and redeliverable to a fresh consumer session, trading a small duplicate-delivery risk for eliminating the lost-job risk. (5) The same review caught `Len` deduplicating `ReadPartitions`' results by topic name and always reading partition 0, silently undercounting once `KAFKA_JOB_PARTITIONS` (see above) was raised above 1 -- fixed to sum lag across every partition.

Two further live-confirmed characteristics, not bugs: **cold start.** A brand-new Kafka cluster's very first consumer group anywhere forces the broker to lazily create its own internal `__consumer_offsets` topic (50 partitions by default), which took long enough in testing to miss a 5s `Dequeue` timeout on the very first call against a truly virgin broker -- a real, one-time, whole-cluster cost (not per-`runner_kind`), and in practice already paid by any long-running production Kafka cluster before Runkite ever connects; deliberately not papered over with a blocking warm-up inside `NewQueue` itself, since the only way to force it early is to wait on an empty topic until timeout, taxing every construction rather than just the first. **Per-consumer-group-lifecycle overhead.** This package's own conformance suite is meaningfully slower than Redis's or NATS's (`make test-kafka` takes ~3 minutes vs seconds) -- each of its ~15 sub-tests joins and leaves its own fresh, uniquely-namespaced consumer group (needed for test isolation, since Kafka has no cheap "wipe everything" primitive the way Redis's `FLUSHALL` or NATS KV bucket deletion do), and that join/leave protocol exchange itself, not this project's own code, is what's slow.

### Checkpoint dual mode

The Python runner checkpoints agent state -- separately from the control plane's own thread/run metadata above:

- **Direct mode** (when `POSTGRES_DSN` is set for the runner, same DB as the control plane): uses LangGraph's own `AsyncPostgresSaver`, writing to its own tables (`checkpoints`, `checkpoint_blobs`, `checkpoint_writes`) alongside but separate from the control plane's tables. Thread state survives a runner restart -- verified by killing a runner mid-HITL-interrupt and resuming against a completely fresh process.
- **In-memory fallback** (no `POSTGRES_DSN`): LangGraph's `MemorySaver`. Explicitly ephemeral -- fine for local dev, does not survive a runner restart.

**Crash recovery covers a job's WHOLE execution, not just its start.** The control plane tracks every dequeued job as in-flight, and an unacked job past a short max-age (6s) is automatically reclaimed and redelivered to a live runner. This closes two windows, not one: the "zombie GetJob" case (a runner dying between `GetJob` and actually starting work -- via gRPC keepalive detecting the dead connection in ~4s plus a same-instant `ctx.Err()` check in `GetJob`), and a runner dying *during* execution, at any point, via a periodic **Heartbeat** RPC the runner calls every ~2s for the whole duration of a run, extending the same in-flight lease. Both are the same underlying mechanism (a Redis-backed lease with a reclaim reaper), not two separate systems -- the runner just keeps resetting the clock throughout execution instead of only once at the start.

This closed a real, previously-shipped gap, found and fixed the honest way: live multi-instance testing first found that a runner killed immediately after emitting its first event left the run permanently stuck (nothing was watching it past that point -- only the delivery, not the whole execution, was covered). Rather than paper over it, that finding was published as a Known Limitation and fixed properly. Live-verified after the fix, via a Redis `MONITOR` trace during a real run: `HSET`/`ZADD` at dequeue, a first renewal ~6ms later (the runner's first `StreamEvents` message), then renewals every ~2.0s for the run's whole duration, and a final `HDEL`/`ZREM` only once the run actually completes. Then verified the actual recovery, not just the renewal mechanism: dispatched a 3-step, 6-second run, killed the runner mid-second-step (a scenario that previously left the run permanently `"status":"pending"` / thread `"status":"busy"` forever) -- a surviving control-plane replica's reaper reclaimed the job ~8s later (`"reclaimed stale jobs" count=1`), a live runner picked it up in the same instant, and the run reached `"status":"success"` with the thread back to `"idle"`, no manual intervention.

With the Redis transport, in-flight tracking lives in Redis itself, not in any one control-plane process's memory -- so it's correctly shared across a multi-node control plane, also verified live via the same `MONITOR` trace: the `HSET`/`ZADD` recording a job in-flight came from one control-plane replica's IP, and later renewals came from other replicas' IPs -- cross-instance sharing under nginx's round-robin, confirmed on the wire. The in-process (zero-dependency, single-instance) transport is, as its name says, still local to that one process by design.

**Fencing closes the last edge case: a reclaimed job's original runner finishing late anyway.** A rarer race than a real crash -- a runner has a transient network blip, misses enough heartbeats for the reaper to reclaim and redispatch its job to a second runner, but the *original* runner's blip was temporary and it finishes execution anyway, trying to report a result after a second runner already took over. A generation token (`RunAssignment.Generation`, bumped every time `ReclaimStale` reclaims a job) closes this: `Heartbeat` and `ReportStatus` both carry the generation the runner was actually dispatched with, and the control plane rejects either call if it's stale. Crucially, `Heartbeat` is where a stale runner actually *learns* it's superseded and can act on it -- every ~2s throughout its (now pointless) execution, not only once it eventually finishes -- so it stops cooperatively (the exact same cancellation path a real cancel signal uses) instead of wasting the rest of a run nobody wants.

Live-verified end to end on the real multi-instance topology: dispatched a `slow_agent` run, `docker pause`d the runner that dequeued it (freezing it mid-execution, not killing it); a surviving replica's reaper reclaimed the job (`"reclaimed stale jobs" count=1`) and a second runner completed it `status=success`. `docker unpause`d the first runner -- it resumed unaware it had been reclaimed, and its very next heartbeat came back superseded (`"heartbeat superseded, signaling runner to stop"`); the runner stopped and reported `status=interrupted`, but that report was itself rejected (`"ignored superseded status report"`) rather than overwriting the real outcome. The run's final status stayed `"success"` from the runner that actually earned it, and the thread ended `"idle"`, not stuck.

A stale runner's terminal ("end") event is also filtered before it ever reaches a subscriber, not just its final status report -- otherwise a client long-polling for this run's result (or watching it live over SSE) could observe the stale runner's own spurious outcome before the real one arrives. Only terminal events are checked this way; ordinary progress events are not, since nothing treats them as authoritative the way a run's final status is.

### Store dual mode

The unified KV store (`/store/*`) is also usable directly from agent code, not just via raw HTTP: `RunkiteStore` (`python/runkite_runner/store.py`) implements LangGraph's `BaseStore` interface and is attached to every loaded graph automatically, so any node using the standard `get_store()` injection (`store.put(...)`, `store.get(...)`) just works -- no agent-code changes needed.

- **Direct mode** (when `POSTGRES_DSN` is set): queries the control plane's own `store_items` table straight over `psycopg`, using the identical `\x1F`-delimited namespace encoding the Go control plane uses. Zero HTTP hop.
- **Proxy mode** (no `POSTGRES_DSN`): calls the control plane's `/store/*` HTTP API over `httpx`.

Both modes read and write the exact same rows -- a value written by one Python runner in direct mode is immediately visible to another runner in proxy mode, and to any HTTP client. Verified end-to-end: a direct-mode runner increments a cross-thread counter via `store.put`, the runner is killed, a fresh proxy-mode runner picks up the next run and reads/increments the same counter through HTTP.

**TTL**: `RunkiteStore` sets `supports_ttl = True` and implements LangGraph's `BaseStore` TTL contract in full (real gap found live: a framework calling `store.aput(..., ttl=...)` used to hard-fail with `NotImplementedError: TTL is not supported by RunkiteStore`) -- `store.put(ns, key, value, ttl=<minutes>)` expires the item that many minutes after it was last accessed, `ttl=None` means no expiration, and reads (`get`/`search`) refresh the expiry by default (`refresh_ttl=False` opts out per-call). Backed by `ttl_minutes`/`expires_at` columns on `store_items`, supported identically across SQLite, Postgres, and MongoDB, and in both direct and proxy mode. Expired items read as absent (no special error) and are swept by a background job that runs regardless of the `retention` config below, since it's hygiene for an already-excluded-from-reads state, not a configurable retention policy. The TypeScript runner's `RunkiteStore` does not implement TTL -- not a gap, `@langchain/langgraph-checkpoint`'s `BaseStore` has no TTL concept in the JS ecosystem at all as of the installed version.

## Runners

Two runner SDKs today, both implementing the exact same Runner Protocol against the exact same Go control plane -- proof that the protocol is actually language-agnostic, not just designed to look that way on paper:

| | Python (`python/runkite_runner/`) | TypeScript (`typescript/runkite-runner/`) |
|---|---|---|
| Framework | LangGraph | LangGraph.js |
| `runner_kind` | `python-langgraph` (default) | `typescript-langgraphjs` |
| gRPC client | `grpcio` | `@grpc/grpc-js` + `@grpc/proto-loader` (dynamic proto loading, no codegen step) |
| Checkpoint direct mode | `AsyncPostgresSaver` | `PostgresSaver` (`@langchain/langgraph-checkpoint-postgres`) |
| Store dual mode | `RunkiteStore` (`BaseStore`) | `RunkiteStore` (`BaseStore`, same `batch()`-only abstract surface) |
| Dynamic graph loading | `importlib` | `tsx` (esbuild-based, no build step for agent code) |
| Custom routes (in-runner) | any ASGI app via `uvicorn` | any `(req, res) => void` handler -- covers plain `node:http` and Express directly; Koa needs `app.callback()` exported instead of `app` itself |

A run is routed to whichever runner declared the target agent: `langgraph.json`'s top-level `runner_kind` is stashed into that agent's metadata at bootstrap, and `createRun` reads it back to set `RunAssignment.runner_kind` -- not hardcoded, so a single control plane can serve a Python runner and a TypeScript runner side by side, each only ever receiving jobs for the agents its own config declared. Falls back to `python-langgraph` when an agent predates this field or its lookup fails, so every existing deployment keeps working unchanged.

### Concurrency (both runners)

By default a runner process (Python's `worker.py` and every `generic_worker`-based adapter, or the TypeScript runner) handles exactly one job at a time -- `--concurrency N` / `RUNKITE_CONCURRENCY=N` (default `1`, fully backward compatible) lets it dispatch up to `N` jobs concurrently instead, via a semaphore-bounded dispatcher (one `asyncio.Task` per job in Python; one un-awaited promise per job, tracked by a small hand-rolled `Semaphore` class, in TypeScript -- Node has no `asyncio.Semaphore` equivalent built in). The control plane needs zero changes for this: `GetJob`/`StreamEvents`/`WatchCancels` were already safe for multiple concurrent calls from one runner connection, and the job queue's dequeue is already atomic across concurrent callers. Setting `N` also sizes each runner's direct-mode Postgres connection pool (Python: `psycopg_pool.AsyncConnectionPool`, replacing a single shared connection behind a lock; TypeScript: `pg.Pool`'s own `max`, floored at node-postgres's default of 10 so a low `N` can't shrink it below that) for both the store and the checkpointer, so concurrent jobs' store/checkpoint I/O don't serialize on one connection either.

```bash
python -m runkite_runner --config langgraph.json --grpc-address localhost:50051 --concurrency 10

# TypeScript
npx tsx src/cli.ts --config langgraph.json --grpc-address localhost:50051 --concurrency 10
```

**What this actually helps, and what it doesn't**: genuinely effective for agents whose wall-clock time is dominated by *waiting* (slow LLM API calls, tool calls, external HTTP requests) -- many concurrent jobs can overlap productively since each spends most of its time not touching the CPU at all (proven for Python: 20 concurrent runs against the same static graph, zero cross-contamination, combined wall time ~14x faster than the sequential sum -- see `bench/REPORT.md`'s finding 1d; proven for TypeScript: 5 concurrent runs against `slow_agent_ts`'s 3-step, 2s-per-step graph all created and completed within the same 6-second window, not staggered 6s apart as concurrency=1 would produce). It does **not** let one process exceed one CPU core's worth of throughput for a CPU-bound or near-zero-compute agent (`asyncio` only overlaps I/O waiting, not CPU work, and Python's GIL means one process uses one core at a time; Node's single-threaded event loop has the same ceiling for CPU-bound work) -- confirmed via direct measurement in `bench/REPORT.md` for Python. For that case, run multiple runner processes (replicas) of the same `runner_kind` against the same control plane instead -- already fully supported, zero config changes, since the queue's dispatch is fair across any runner of that kind.

**CrewAI/AutoGen-specific caveat**: a shared `Crew` (CrewAI) or `AssistantAgent` (AutoGen) instance is not safe for concurrent invocation -- confirmed by reading each framework's own source: CrewAI's `kickoff`/`akickoff` write results onto shared instance attributes like `self.usage_metrics`; AutoGen's `AssistantAgent.run()` appends to a shared, mutable `model_context` (conversation history). Both adapters serialize concurrent calls on the same `graph_id` via a per-graph lock. AutoGen's adapter additionally `clear()`s that context before each run so sequential jobs on the same long-lived agent do not leak history across unrelated threads (LlamaIndex avoids the equivalent by reconstructing `chat_history` per call). CrewAI/AutoGen runs sharing a `graph_id` don't get real parallelism from `--concurrency`, only correctness -- LangGraph, LlamaIndex, and plain LangChain runs do get real parallelism.

```bash
cd typescript/runkite-runner
npm install
npx tsx src/cli.ts --config ../../examples/echo_agent_ts/langgraph.json --grpc-address localhost:50051
```

Same environment variables as the Python runner (`POSTGRES_DSN` for direct-mode checkpoint/store, `RUNKITE_GRPC_URL`/`RUNKITE_HTTP_URL`, `RUNNER_TOKEN`). `npm run build` compiles to `dist/` for production; `npx tsx` runs directly from TypeScript source for local development, matching the Python runner's zero-build-step DX.

Dockerized via `Dockerfile.runner-ts` (same zero-build-step pattern as `Dockerfile.runner`'s Python image -- `tsx` loads a user's `graph.ts` directly, no separate `tsc` compile). `docker-compose.yml`/`docker-compose.dev.yml` (the Python-focused stack) are unchanged; a standalone `docker-compose.ts.yml` demonstrates the TS runner instead, fully self-contained (SQLite + in-memory transport, no postgres/redis needed):

```bash
docker compose -f docker-compose.ts.yml up --build
```

Kept separate rather than merged into the main compose file because a single control-plane instance's `LANGGRAPH_CONFIG` binds to one `runner_kind` at a time (see `internal/config/loader.go`'s `RunnerKind` field) -- mixing Python and TS agents in one deployment means auto-discovering multiple `langgraph.json` files (leaving `LANGGRAPH_CONFIG` unset), not something this demo needed to prove the image itself works.

Live-verified end to end against a real control plane -- manually, the same way cron's multi-instance claim was verified live rather than in CI (spinning up a full multi-process stack per test run isn't worth paying on every commit): the TypeScript runner dynamically loads a `.ts` graph, executes it with direct-mode Postgres checkpointing and store attached, streams events back, a cancel mid-execution correctly propagates through a real `WatchCancels` gRPC stream, and a full interrupt -> human approval -> `Command(resume)` -> completion round trip persists correctly across two separate runs on the same thread -- the exact same three validation gates already verified for the Python runner (VG-001/002/003), now proven independently in a second language. `make test-e2e`'s automated suite is still Python-only; the TypeScript equivalents above are manual-verification-only, not regression-guarded in CI.

### Framework Adapters

Four more Python runners (`python/adapters/{crewai_adapter,llamaindex_adapter,autogen_adapter,langchain_adapter}/`), each proving the control plane never assumed LangGraph -- built on a new shared, framework-agnostic loop (`runkite_runner.generic_worker`, extracted from but not replacing `worker.py`'s LangGraph-specific one) that handles only the gRPC polling/streaming/status-reporting mechanics. Each adapter is a thin translation layer implementing just two methods (`load_config`, `execute`) -- a small framework-adapter shim:

| | CrewAI | LlamaIndex | AutoGen | Plain LangChain |
|---|---|---|---|---|
| `runner_kind` | `python-crewai` | `python-llamaindex` | `python-autogen` | `python-langchain` |
| Loads | a `Crew` (`./crew.py:crew`) | a chat engine/agent (`./chat_engine.py:chat_engine`) | an `AssistantAgent` (`./agent.py:agent`) | any `Runnable` (`./chain.py:chain`) |
| Executes via | `crew.akickoff(inputs={"input": ...})` | `engine.achat(text, chat_history=...)` | `agent.run(task=...)` | `runnable.ainvoke({"input": ...})` |
| Venv | isolated (`python/adapters/crewai_adapter/.venv`) | isolated (`python/adapters/llamaindex_adapter/.venv`) | isolated (`python/adapters/autogen_adapter/.venv`) | shared `python/.venv` (`langchain-core` already a dependency) |
| Example | `examples/crewai_agent/` | `examples/llamaindex_agent/` | `examples/autogen_agent/` | `examples/langchain_agent/` |

All four examples are offline and deterministic (a hand-written fake LLM/model-client subclass returning a fixed response) -- no API key needed, same convention as `examples/vector_agent`'s fake embeddings. Input/output convention matches the LangGraph runner: extract the last human message's text from `RunAssignment.input.messages`, invoke the framework, append the reply as `{"role": "ai", "content": ...}` -- so client code built against one `runner_kind` doesn't need to change to talk to another.

Cancellation is wired into all four via `generic_worker.run_cancellable` -- each adapter races its single framework call (`akickoff`/`achat`/`run`/`ainvoke`) against `cancel_event`, calling `.cancel()` on the underlying task and reporting `interrupted` (not `error`) if the cancel wins, the same outcome a cancelled LangGraph run reports. Live-verified against a real gRPC `WatchCancels` signal, not just a unit-test mock.

**Why CrewAI, LlamaIndex, and AutoGen get their own isolated venv** (plain LangChain doesn't need one): confirmed live during development -- installing `crewai` into the shared `python/.venv` silently downgraded `protobuf`, a dependency the production LangGraph runner's generated gRPC stubs are version-sensitive about (see the Runner Protocol section's protobuf note). AutoGen's own dependencies didn't conflict during development, but an isolated venv is kept anyway for consistency and future-proofing. Real deployments would run each framework's runner as a genuinely separate process anyway (arguably a separate container), so an isolated venv here matches that reality rather than fighting it. Setup:

```bash
cd python/adapters/crewai_adapter        # or llamaindex_adapter / autogen_adapter
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python crewai grpcio protobuf   # or: llama-index-core ... / autogen-agentchat ...

cd ../../../examples/crewai_agent
PYTHONPATH=<repo>/python:<repo>/python/adapters \
  <repo>/python/adapters/crewai_adapter/.venv/bin/python -m crewai_adapter \
  --config langgraph.json --grpc-address localhost:50051
```

Live-verified end to end for all four: a real control plane, a real runner process for each framework, a real `POST /threads/{id}/runs` request through to a real thread-values response containing that framework's actual output. `make test-adapters` runs the CrewAI/LlamaIndex/AutoGen unit tests once their venvs exist (`test_generic_worker.py`/`test_langchain_adapter.py` cover the shared loop and plain LangChain respectively, and run in CI/`make test-python` alongside the rest of the Python suite -- CrewAI/LlamaIndex/AutoGen's isolated-venv tests run in a dedicated CI step instead, for the same shared-venv-isolation reason above). Plain LangChain additionally has automated, CI-gated end-to-end coverage (`test/e2e/adapters/`, part of `make test-e2e`) -- a real control plane dispatching to a real `langchain_adapter` runner subprocess over real gRPC, including cancellation via a real `WatchCancels` signal, not just the unit-level fakes. CrewAI/LlamaIndex/AutoGen don't have the equivalent e2e tier yet (their isolated venvs make it more setup than LangChain's shared one) -- live-verified manually during development, same as before, just not CI-gated.

## Docker

### Full-stack deployment

`docker-compose.yml` runs the whole system -- Postgres, Redis, the control plane, and a Python runner -- built from `Dockerfile` and `Dockerfile.runner`:

```bash
cp .env.example .env   # fill in POSTGRES_PASSWORD and RUNNER_TOKEN with real values
docker compose up -d --build
curl http://localhost:2026/health
docker compose down -v
```

`POSTGRES_PASSWORD` and `RUNNER_TOKEN` are required, on purpose -- there's no built-in fallback, so `docker compose up` fails fast with a clear error if either is unset rather than silently starting with a well-known password or a shared "change-me-in-production" token nobody actually changes. Postgres and Redis also don't publish any port to the host in this file -- only the `runkite`/`runner` services on the same compose network need to reach them by service name.

`docker-compose.dev.yml` is the zero-dependency local stack — control plane + Python runner only (SQLite + in-memory transport, no Postgres/Redis), with source-mounted `examples/` and `python/`:

```bash
docker compose -f docker-compose.dev.yml up -d --build
```

Full-stack compose uses Postgres/Redis on 5432/6379; stop local services on those ports first, or override via a compose override file.

### Test infrastructure

`docker-compose.test.yml` starts ephemeral, tmpfs-backed Postgres + MySQL + Redis + MongoDB + Qdrant + Weaviate + [Pinecone Local](https://docs.pinecone.io/guides/operations/local-development) + NATS + Kafka for running the conformance test suite (see Development below), on non-standard ports (5433/3307/6380/27018/6333/8080/5080-5200) specifically to avoid colliding with the full-stack compose above or any local services -- NATS and Kafka are the exceptions, left on their standard ports (4222 / 9092) since nothing else in this project's compose files uses them:

```bash
docker compose -f docker-compose.test.yml up -d
make test-all
docker compose -f docker-compose.test.yml down -v
```

## Development

```bash
make test           # SQLite + in-memory only (no external deps)
make test-pg        # Postgres conformance (requires infra-up)
make test-mysql     # MySQL conformance + cmd's backend-selection wiring (requires infra-up)
make test-redis     # Redis conformance (requires infra-up)
make test-mongo     # MongoDB conformance (requires infra-up)
make test-kafka     # Kafka JobQueue conformance (requires infra-up; ~3min, see Kafka transport section for why)
make test-all       # All backends (requires infra-up)
make test-e2e       # Black-box E2E: real binary + real runner + PG/Redis (requires infra-up)
make test-python    # Python runner unit tests
make test-ts        # TypeScript runner unit tests
make test-adapters  # CrewAI/LlamaIndex/AutoGen adapter unit tests (requires their isolated venvs, see python/adapters/*/README.md)
make infra-up       # Start ephemeral Postgres + MySQL + Redis + MongoDB + Qdrant via Docker
make infra-down     # Stop test infrastructure
make vet            # go vet
make build          # Build the binary

make lint           # gofmt/vet/golangci-lint + ruff + oxlint/prettier, all three SDKs
make fmt            # Auto-fix formatting for all three SDKs
make lint-go        # Just Go: gofmt -l + go vet + golangci-lint (go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest)
make lint-python    # Just Python: ruff check + ruff format --check (python/.venv/bin/pip install -r python/requirements-dev.txt)
make lint-ts        # Just TypeScript: oxlint + prettier --check
```

Same three-linter shape enforced in CI (`.github/workflows/ci.yml`) on every push/PR. Config lives in `.golangci.yml` (Go), `ruff.toml` (Python), and `typescript/runkite-runner/.oxlintrc.json` + `.prettierrc.json` (TypeScript) -- each a deliberately moderate starting rule set (golangci-lint's own curated "standard" linters, not "all"; ruff's `E`/`F`/`I`/`UP`/`B` rule groups) rather than maximally strict, so the gate catches real bugs (unused imports, unchecked errors on non-cleanup calls, suspicious constructs) without drowning contributors in day-one style nitpicks.

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
See [Agent Marketplace / Registry](#agent-marketplace--registry) above for the full design and known limitations.

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

### Custom Routes
```
*      /custom/*                  Reverse-proxied to custom_routes.url, /custom prefix stripped
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
    "interval_minutes": 60
  }
}
```

A background loop (same pattern as the cron scheduler) runs immediately on startup and then every `interval_minutes` (default 60) and, for whichever fields are set:
- `runs_max_age` -- deletes TERMINAL runs (`success`/`error`/`interrupted`/`timeout`; `pending`/`running` are never touched regardless of age) whose `updated_at` is older than this duration.
- `checkpoints_keep_last` -- keeps only this many most recent checkpoints per thread in runkite's own `thread_checkpoints` table, deleting older ones. Never touches a thread's current-state snapshot (`threads.values_json`), only its history.
- `cron_claims_max_age` -- deletes old rows from the cron scheduler's fire-dedup table.

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
GET    /metrics                    Prometheus metrics (outside auth)
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

Every standard `OTEL_EXPORTER_OTLP_*` env var is honored (endpoint, headers for hosted backends' auth tokens, protocol `grpc`/`http/protobuf`, TLS, compression, timeout) -- this is what makes it work with any OTLP-speaking backend (Jaeger, an OTel Collector, Langfuse, Arize Phoenix, Datadog Agent) with zero runkite-specific configuration. Every HTTP request gets a span automatically; each run gets its own child span (`run.id`, `thread.id`, `graph.id`, `run.status` attributes) spanning from creation to terminal status, and the run's real W3C `traceparent` is propagated to the runner in `RunAssignment.trace_context` -- a runner with its own OTel instrumentation (e.g. LangChain's tracing callback) gets its spans nested under the same trace instead of an orphaned one. On `SIGTERM`/`SIGINT`, buffered spans are force-flushed before exit so a redeploy doesn't silently drop the last few seconds of traces.

### LangGraph SDK Compatibility
```
POST   /assistants/search          Alias for /agents/search
GET    /assistants/{id}            Alias for /agents/{id}
GET    /assistants/{id}/schemas    Alias for /agents/{id}/schemas
```

## Examples

| Example | Description |
|---------|-------------|
| `examples/echo_agent/` | Minimal echo agent -- proves the bridge works |
| `examples/react_agent/` | ReAct agent with tool calls (fake LLM, no API key) |
| `examples/approval_agent/` | HITL interrupt/resume with `langgraph.types.interrupt()` |
| `examples/slow_agent/` | Long-running agent for streaming/cancel testing |
| `examples/all_agents/` | Multi-agent config referencing all example graphs |
| `examples/cron_agent/` | Cron-scheduled daily run |
| `examples/store_agent/` | Uses `get_store()` to prove Store Dual Mode's direct/proxy interop |
| `examples/custom_routes_agent/` | FastAPI app hosted in-runner, reachable via `/custom/*` |
| `examples/echo_agent_ts/` | Echo, slow (cancel), approval (HITL), and factory-graph agents, in TypeScript/LangGraph.js -- proves the Runner Protocol is language-agnostic |
| `examples/vector_agent/` | Retrieval-augmented demo using `RunkiteVectorStore` (Vector Store Dual Mode) -- fake, deterministic embeddings, no API key needed |
| `examples/factory_agent/` | Per-request Factory Graph -- proves fresh-instance-per-run isolation and `runtime.user` identity passthrough |
| `examples/llm_sim_agent/` | Configurable simulated-LLM-latency agent (`LLM_SIM_DELAY_MS`) for `--concurrency` benchmarking under a realistic, I/O-wait-dominated workload shape -- see `bench/REPORT.md` section 7. `echo_agent_ts/llmSimGraph.ts` is the TypeScript equivalent. |
| `examples/a2a_agent/` | `coordinator_agent` delegates to `worker_agent` mid-execution via `call_agent` -- proves Agent-to-Agent delegation end to end (see [Agent-to-Agent (A2A)](#agent-to-agent-a2a)) |

## Known Limitations

Honest gaps, not hidden ones:

- **`StreamEvents`' non-terminal events are not fenced (a deliberate, narrow, documented trade-off).** `Heartbeat`, `ReportStatus`, and `StreamEvents`' own terminal ("end"/"error") events all reject/drop a stale generation (see "Crash recovery" above) -- but an ordinary progress event within a still-active stream is not checked, since nothing downstream treats a non-terminal event as authoritative the way a run's final status is, and checking every single event (rather than just the one terminal event per run) would add a Redis round-trip per streamed chunk for no real correctness benefit. In practice this only matters for a narrow window -- a reclaimed runner streaming a few more progress events before its own `Heartbeat` catches up and tells it to stop (heartbeats fire every ~2s) -- and the residual risk is cosmetic: a live SSE subscriber could see a stray extra progress event mixed in, not a wrong final outcome. Run deadline/timeout is opt-in via `langgraph.json`'s `run_timeout` section (see [Run Timeout](#run-timeout)) -- disabled by default so a deployment that never configured it keeps the historical "runs until cancel/completion" behavior. The in-process (zero-dependency, single-instance) transport's in-flight tracking is local to that one process by design, not a gap -- there's only one process to track it in.
- **Authorization is coarse-grained.** Permissions are enforced at `read`/`write`/`admin` method-level granularity (see Auth section), not per-resource ACLs.
- **`db downgrade` isn't implemented.** The schema is a single idempotent migration, not versioned up/down migrations.
- **OTel tracing covers the control plane, not runner-internal spans.** Neither runner's own LLM calls, tool calls, etc. are wrapped in OTel spans yet -- but the run's real W3C `traceparent` is already propagated to both (`RunAssignment.trace_context`), so a runner-side OTel integration (e.g. wiring LangChain's tracing callback to the same propagator) has what it needs and nests correctly under the same trace the moment it's added.
- **Direct-mode store/checkpoint access is single-tenant regardless of multi-tenancy config.** See the Multi-tenancy section's "Known gaps" -- a known, documented trade-off of direct mode (see Runners above), not new.
- **No central tenant registry, and no user/API-key management UI.** See Multi-tenancy and Admin UI -- a tenant exists implicitly the moment a resource is tagged with it, and there's no persisted user/API-key table to manage in the first place (`api_key` entries are static config).
- **`on_tool_call` requires runner cooperation.** The control plane fires it whenever it sees a RunEvent with `method: "tool_call"`, but doesn't parse LangGraph/LangChain message shapes itself (staying framework-agnostic) -- it's up to a framework-aware runner to emit that method. Both LangGraph runners do: Python's `worker.py` (`find_new_tool_calls`) and the TypeScript runner's `executeRun.ts` (`findNewToolCalls`, a direct port -- same recursive scan for `AIMessage.tool_calls`, same dedup-by-`id` semantics, same "checked before interrupt/cancel handling in the stream loop" ordering) both scan every stream chunk and dedupe by tool-call `id` so a message seen in both `values` and `updates` mode (if both are requested) only fires once. Live-verified end to end for Python: `examples/react_agent` (a real `StateGraph` + `ToolNode`, not a mock) run through the real control plane + real Python runner, with a webhook configured for `tool_call`, actually delivers `{"name":"search","args":{...},"id":"call_001"}` to an external receiver; the TypeScript port is unit-tested against the same message shapes (`executeRun.test.ts`) but not yet re-verified against a live tool-using TS example graph the same way. `generic_worker.py` still doesn't emit it -- `run_start`/`run_complete`/`error`/`interrupt` are fully wired for every runner and fire from real control-plane-observed lifecycle transitions regardless.
- **In-runner custom routes are ASGI-only.** `uvicorn` (the SDK's only custom-routes dependency) doesn't serve WSGI apps -- Flask needs an adapter (e.g. `a2wsgi.WSGIMiddleware`) wrapped around it first. Sidecar mode has no such restriction (any language, any framework).
- **Real agent schema introspection is Python-only.** See the Agents API reference above -- the Python LangGraph runner reports each static graph's real input/output/state/config JSON Schema at startup via genuine public LangGraph API; the TypeScript runner still reports the `{"type": "object"}` bootstrap stub, since LangGraph.js's compiled graph has no public equivalent of Python's `get_input_jsonschema()`/etc (only private, underscore-prefixed internal fields, not safe to depend on across versions). CrewAI/LlamaIndex/AutoGen/plain LangChain (via `generic_worker.py`) also keep the stub -- none of them have a typed state-graph concept the same way, so there's no equivalently real schema to report in the first place.
- **`checkpoint_ref` is rejected, not implemented.** A client asking to resume from a specific past checkpoint (rather than a thread's latest one) gets an immediate `400`, not silent wrong behavior -- found by an external audit that `checkpoint_ref` used to flow all the way from the API down to the runner and get silently dropped there, so a client requesting it got a normal `200` and a run that quietly resumed from the wrong place. Implementing real checkpoint-ref resume (not just rejecting it) remains open.
- **`serve` (not `dev`) fails closed on an insecure default posture.** Starting `serve` without a durable state backend (`POSTGRES_DSN`/`MYSQL_DSN`/`MONGO_URI`), a shared transport (`REDIS_URL`/`NATS_URL`/`KAFKA_URL`), `RUNNER_TOKEN_*`, or client-facing `auth` configured now exits `1` with a clear error instead of silently starting a single-node, fully-open instance that still passes its own `/readyz` -- found live in an external audit. Set `RUNKITE_ALLOW_INSECURE_SERVE=1` to start anyway (a private network, CI, or a deliberate quick demo are legitimate reasons); `runkite dev` is unaffected and always starts with the zero-dependency local posture.
- **CORS `"*"` no longer implies credentialed access.** `allow_origins: ["*"]` still means "any origin may read this API," but `Access-Control-Allow-Credentials` is now only sent for an explicitly-listed origin, never for a wildcard match -- an external audit found the old behavior reflected the request's `Origin` header verbatim *and* set `Allow-Credentials: true` for literally any origin once `"*"` was configured, which is worse than a literal `Access-Control-Allow-Origin: *` (browsers refuse that exact combination with credentials; reflecting the origin instead bypassed that protection). List explicit origins alongside `"*"` if some callers need credentialed access.
- **`action=rollback` on run cancel doesn't delete checkpoints.** See the Runs API reference above -- it deletes the run record, but not any checkpoints, despite the Agent Protocol spec's literal wording. Checkpoints are keyed by `thread_id`, not `run_id` (accumulated across every run on a thread, with no per-run attribution to select just one run's slice), and in direct mode live entirely in LangGraph's own Postgres tables, outside this project's own state store.
- **TypeScript runner's Docker image (`Dockerfile.runner-ts`) is built and live-verified** (a real run against `echo_agent_ts` through `docker-compose.ts.yml` completes and echoes back correctly), but its own VG-001/002/003-equivalent verification is otherwise manual-only, not regression-guarded in CI (same as cron's multi-instance claim test).
- **Vector store supports pgvector, Qdrant, Weaviate, and Pinecone.** Every backend named in the original plan is implemented -- see the Vector Store section.
- **Transport supports in-memory, Redis, NATS (JetStream), and Kafka.** See the NATS transport section for its own real difference from Redis: fencing generation reuses JetStream's native per-message delivery count instead of a hand-rolled counter, and `EventBroker.Close` is a documented no-op there (a late `Subscribe` on an already-finished run gets a silently-empty channel, not an immediately-closed one the way Redis's explicit closed-marker gives). See the Kafka transport section for why it's JobQueue-only (paired with Redis or in-process for the broker/cancelbus) and its own honest double-dispatch race under concurrent reclaim, which Redis/NATS close atomically and Kafka alone cannot.
- **Admin UI overview counts are capped, not a real `COUNT` query.** `GET /admin-api/overview` derives its totals/by-status breakdowns by fetching up to 1000 rows per resource type and counting in Go, same as every other list endpoint's pagination -- fine at the scale a single admin dashboard is typically used at; a deployment with more rows than that per resource would want a dedicated `COUNT` query added to `state.Store` instead.
- **Factory Graph's `_ExecutionRuntime` construction depends on `langgraph_sdk`'s internal (underscore-prefixed) class.** `langgraph_sdk.runtime.ServerRuntime` has no public constructor -- factory_graph.py constructs `_ExecutionRuntime` directly, which could break on a `langgraph-sdk` upgrade that changes its internals; a `_MinimalRuntime` duck-typed stand-in (covering the same documented `.user`/`.store`/`.execution_runtime`/`.ensure_user()` surface) is used as a fallback if that construction fails, so factory graphs keep working either way for the common case.
- **Factory Graph's `runtime.execution_runtime.context` is always the run's `configurable` dict, not a typed `context_schema` instance.** LangGraph Platform's own beta API supports strongly-typed context via a graph's declared `context_schema`; this passthrough is untyped (a plain dict), sufficient for factories that only read specific keys but not for ones relying on `context_schema`'s validation/defaults.
- **Admin UI write actions**: cancel-run and delete-thread reuse the exact client-facing handlers under system context (`POST /admin-api/runs/{runID}/cancel`, `DELETE /admin-api/threads/{threadID}`); webhook redelivery (`POST /admin-api/webhooks/dead-letters/{id}/redeliver`) is genuinely new, since the client-facing API has no equivalent. Known limitation: `WebhookDeadLetter` doesn't persist the original signing secret, so a redelivery is sent unsigned -- a receiver enforcing `X-Runkite-Signature` will reject it. Redelivery also doesn't remove the entry from the dead-letter list on success (it stays as an audit record, not a mutation of stored state) -- the response's `delivered` field is the UI's only success signal.
- **Retention's `checkpoints_keep_last` only covers proxy-mode checkpoints.** See the Retention section above -- direct-mode Python+Postgres deployments use LangGraph's own `AsyncPostgresSaver` tables, which this feature doesn't touch.

## License

Runkite is licensed under the [Business Source License 1.1](LICENSE). You
can use, modify, and self-host it (including in production, including
commercially within your own org) free of charge. The one restriction:
you may not offer Runkite itself as a hosted/managed service to third
parties without a commercial license. Each release converts to
Apache 2.0 four years after publication -- see the `LICENSE` file for
the exact terms.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for development setup, running tests, and how to add a new state/vector-store backend, runner language, or framework adapter. Found a security issue? See [`SECURITY.md`](SECURITY.md) instead of opening a public issue.
