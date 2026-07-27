# Runkite

A Go control plane implementing the [Agent Protocol](https://github.com/langchain-ai/agent-protocol) spec. Framework-agnostic by design -- the server never imports your agent framework, and the Runner Protocol (gRPC) is the only integration point. Shipped runners: Python/LangGraph, TypeScript/LangGraph.js, and (proving the framework-agnostic claim for real) CrewAI, LlamaIndex, and plain LangChain -- see [Framework Adapters](#framework-adapters) below. AutoGen remains on the roadmap, not yet implemented.

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
| `MONGO_URI` | (unset) | MongoDB connection URI; enables MongoDB state backend (checked after `POSTGRES_DSN`, so setting both uses Postgres) |
| `MONGO_DB` | `runkite` | MongoDB database name (used when `MONGO_URI` is set) |
| `REDIS_URL` | (unset) | Redis URL; enables Redis transport (queue + broker) |
| `DATABASE_PATH` | `./runkite.db` | SQLite file path (used when neither `POSTGRES_DSN` nor `MONGO_URI` is set) |
| `QDRANT_URL` | (unset) | Qdrant REST base URL; fallback for `vector_store.url` when `vector_store.type` is `"qdrant"` (only read if `vector_store` is configured at all -- doesn't enable Qdrant by itself, unlike `POSTGRES_DSN`/`MONGO_URI` for the state backend) |
| `LANGGRAPH_CONFIG` | (unset) | Path to langgraph.json (alternative to --config flag) |
| `RUNNER_TOKEN_<kind>` | (unset) | Shared token for runner auth (e.g. `RUNNER_TOKEN_python_langgraph`) |

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
    "cache_ttl_seconds": 300
  }
}
```

## Multi-tenancy

Flat tenant scoping (master plan: "workspace/org/team hierarchy with isolated data" -- a flat `tenant_id` is the actual scope built; a hierarchy can be layered on top later via a naming convention without a schema change, and wasn't needed to satisfy anything else already built, including `rate_limit.per_tenant`). Opt-in and fully additive: with no `auth` configured (or a provider that doesn't supply a tenant), every request resolves to an implicit `default` tenant -- exactly today's single-tenant behavior, unchanged.

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
- **Direct-mode runner store/checkpoint access always operates as the `default` tenant.** A direct-mode runner (Python or TypeScript) talks straight to Postgres with a raw DB connection, not an authenticated HTTP request -- there's no tenant identity to carry across that boundary without a Runner Protocol wire change. This is exactly the master plan's own Direct Mode Trust Model guidance: proxy mode is the recommended path for real per-tenant store/checkpoint isolation; direct mode is documented as bypassing control-plane authz on that data in multi-tenant deployments, not something this feature silently fixed.
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

## Vector Store

Semantic search over embeddings (master plan: "Vector/semantic store"), backed by **pgvector** (Tier 1, SQL-based) or **Qdrant** (the non-SQL exemplar, same role Mongo plays for the state store -- proof the `VectorStore` interface is implementable against a real standalone vector database, not just a Postgres extension). Disabled entirely by default, same opt-in convention as `llm_cache`/`rate_limit`/`webhooks`/`cron` -- never implicitly enabled just because `POSTGRES_DSN` is set, since an existing Postgres deployment may not have the pgvector extension installed or permitted.

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

`dimensions` fixes the embedding vector's width at creation time (pgvector's `vector(N)` column / Qdrant's collection vector size are both fixed-dimension) -- defaults to 1536 (OpenAI `text-embedding-3-small`/`ada-002`'s size) when omitted. `pgvector` requires `POSTGRES_DSN` to be set on the control plane; the extension itself (`CREATE EXTENSION IF NOT EXISTS vector`) is created automatically on startup, but the Postgres server must have the pgvector extension binary available -- the `pgvector/pgvector:pg16` image (used by this repo's `docker-compose.yml`/`docker-compose.test.yml`) ships it; a bare `postgres:16` image does not. `qdrant` requires `vector_store.url` or `QDRANT_URL` -- one Qdrant collection holds every tenant/namespace (`tenant_id`/`namespace` are stored as payload fields and included in every filter, not one collection per tenant), and a caller's `(tenant_id, namespace, id)` is mapped to a deterministic UUID v5 for Qdrant's point ID (which must be an integer or UUID, never an arbitrary string).

**API**: `PUT /vectors/items` (upsert -- overwrites on a repeat `id`, re-embedding a changed document is the common case, not a conflict), `DELETE /vectors/items`, `POST /vectors/search` (top-K cosine similarity, optional exact-match `filter` over metadata) -- identical regardless of which backend is configured. Same dual-mode convention as the key-value store: mirrored under `/internal/vectors/*` for a runner's proxy-mode client. 501s (not 404s) when `vector_store` isn't configured -- "this feature isn't turned on" is a more actionable signal than "this route doesn't exist" for something opt-in.

**Python SDK**: `RunkiteVectorStore` (`python/runkite_runner/vectorstore.py`) implements LangChain's `VectorStore` interface (`add_texts`, `similarity_search`, `similarity_search_with_score`, `from_texts`), so it drops into existing LangChain/LangGraph RAG code unchanged. Prefer proxy mode (`http_base_url` / `RUNKITE_HTTP_URL`) -- it talks to the control plane's `/vectors/*` API and works for every backend (pgvector, Qdrant, …). Direct mode (`postgres_dsn` only, no HTTP URL) queries `vector_items` over `psycopg` and is correct **only** when the control plane is on pgvector; when both DSN and HTTP URL are provided, proxy wins so a runner with `POSTGRES_DSN` set for checkpoints doesn't silently write vectors to Postgres while the CP is on Qdrant. See `examples/vector_agent/` for a working retrieval demo (fake, deterministic embeddings -- no API key needed; always uses proxy).

**Known limitations, stated plainly**:
- **Dimension is fixed at first creation, not migrated**, for both backends. Changing `vector_store.dimensions` after the table/collection already exists does not migrate existing rows -- `Upsert` starts failing with a clear dimension-mismatch error (not silent corruption) until it's manually dropped or recreated at the new width.
- **Cosine similarity only.** Both backends support other distance metrics (pgvector: L2, inner-product; Qdrant: Euclidean, dot product); only cosine is wired up today, the most common choice for text embeddings.
- **Direct mode is pgvector-only.** There is no runner-side Qdrant client; Qdrant-backed deployments always go through the control plane's HTTP API. Functionally identical, just always one network hop instead of sometimes zero.
- **Qdrant's `created_at` resets on re-index.** Qdrant has no built-in insert-vs-update distinction the way Postgres's `ON CONFLICT DO UPDATE` does, so pinning down "first write time" would need a read before every write. `created_at` is set to "now" on every `Upsert` call, including re-indexing an already-existing `id` -- correct for a fresh item, but re-indexing an existing document resets rather than preserves its original `created_at`.
- **Weaviate, Pinecone are not implemented** (Tier 2 -- experimental, not yet built). The `VectorStore` interface (`internal/vectorstore`) is backend-agnostic and has a shared conformance suite (`internal/vectorstore/conformance`) both pgvector and Qdrant pass identically, but those two remaining backends have no implementation yet.

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

### Circuit breakers

Every OAuth2 connector (`oauth2_client_credentials`, `oauth2_token_exchange`) gets a per-connector circuit breaker guarding its actual token-fetch network call -- always on, with tunable thresholds:

```yaml
circuit_breaker:
  failure_threshold: 5    # consecutive failures before opening (default: 5)
  cooldown_seconds: 30    # how long to fail fast before a trial call (default: 30)
```

Standard closed/open/half-open state machine: closed passes calls through and counts consecutive failures; opening it makes every call fail fast (`503` + `Retry-After`, no network attempt) until the cooldown elapses; half-open lets exactly one trial call through and closes on success or reopens on failure. `api_key`/`bearer` auth never touches a breaker -- they make no network calls to break on. A cached, still-valid `client_credentials` token keeps being served even while the breaker is open on a since-broken refresh endpoint. Current state is visible at `GET /internal/connectors/{name}` (`circuit_breaker_state`).

## Custom Routes

User-defined HTTP endpoints mounted at `/custom/*` alongside the Agent Protocol API (master plan: "Custom routes"). From the control plane's side, both modes below are the exact same mechanism -- a reverse proxy to `custom_routes.url` in `langgraph.json`, with the `/custom` prefix stripped before forwarding (`/custom/webhook` reaches the target as `/webhook`). Unreachable target returns `502`; unconfigured returns `404`.

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

An agent calls another agent as a sub-task, mid-execution (master plan: "Agent-to-agent (A2A): agent calls agent via the same Agent Protocol API -- native sub-agent delegation"). The mechanism is deliberately **not** a new protocol surface -- it's the exact same `POST /threads/{id}/runs` + wait-for-result path any client already uses, just reachable from inside a runner's own process via one new internal route (`POST /internal/a2a/runs`) instead of a public one.

**Python SDK**: `call_agent` (`python/runkite_runner/a2a.py`) is what a node calls, using the exact `config` LangGraph already passes it -- everything needed (the calling run's own `run_id`, the authenticated user to forward) is already there:

```python
from runkite_runner.a2a import call_agent

async def coordinator_node(state, config: RunnableConfig) -> dict:
    result = await call_agent(config, "worker_agent", {"messages": [...]}, wait=True)
    ...
```

See `examples/a2a_agent/` for a complete working example (`coordinator_agent` delegates to `worker_agent`).

Three things this adds on top of the shared run-creation/wait path:

- **Auth context propagation**: the runner forwards the caller's identity/permissions via `on_behalf_of` (the Python helper copies them from the parent run's `langgraph_auth_user`). Tenant is derived from the PARENT run's own `tenant_id` (looked up server-side), never trusted from the request body. This is **propagation, not enforcement** -- the control plane does not re-check `on_behalf_of.permissions` against a stored parent-auth record (runs don't persist the original caller's auth), so a buggy or compromised agent/runner could claim higher permissions than the parent run actually had. The trust boundary is "the runner is trusted," same as the rest of `/internal/*`.
- **Recursion limits**: every sub-run's `depth` is enforced against `a2a.max_depth` (default 10) at creation time -- an accidental cycle or runaway delegation chain fails fast with `400`, not a resource leak. Configurable:
  ```json
  { "a2a": { "max_depth": 10 } }
  ```
- **Cost attribution**: every *delegated* run carries `parent_run_id` and `root_run_id` (the top of the chain). The fields are persisted and indexed; a server-side `GET /runs/search?root_run_id=` filter is not exposed yet (see limitations), so tree queries today are client-side filters over listed runs.

**Known limitations, stated plainly**:
- **Concurrency deadlock risk, real and easy to hit**: if the SAME runner process executes both the calling agent and the agent it delegates to, a `wait=True` call blocks that runner's in-flight job slot waiting for a sub-run the same process must dequeue. Default `--concurrency 1` deadlocks on a single hop. **`--concurrency 2` covers one wait-hop**; a deeper wait-chain (A waits on B waits on C) on one process needs roughly `concurrency >= chain_depth + 1`, or another replica of the same `runner_kind`. Horizontally-scaled runner replicas avoid this entirely (any replica can pick up the sub-run).
- **Parent lookup is cross-tenant under runner trust.** `/internal/a2a/runs` resolves `parent_run_id` with system context (no client tenant), then scopes the child to that parent's tenant. A compromised runner that learns another tenant's run UUID could attach a child there -- same "runner is trusted" boundary as other `/internal/*` routes, stated explicitly rather than implied closed.
- **`root_run_id` isn't a `RunSearchRequest` filter yet.** Persisted (and indexed on every backend) but finding "every run in this delegation tree" today means fetching runs and filtering client-side. Top-level (non-delegated) runs leave `root_run_id` nil -- only descendants carry it.
- **No cost/token aggregation / cancel cascade built on the tree yet.** Summing tokens across a tree, and cancelling children when a parent is cancelled, are left to the caller for now.

## Agent Marketplace / Registry

A searchable catalog of agent definitions (master plan: "Agent marketplace / registry: publish, discover, and deploy agent definitions"). **Minimal viable registry** scope, chosen deliberately: publish/search/get/version-history via API and Admin UI, no security review workflow, and no automatic clone-and-execute deploy pipeline. A registry entry is metadata plus a `source_ref` -- a git URL, a plain URL, or an inline `langgraph.json` snippet -- pointing at where a human (or their own tooling) goes to actually wire it into a deployment. This is deliberately a catalog, not a package manager's install step: running arbitrary fetched code is a fundamentally different trust/sandboxing problem than a searchable listing, and out of scope here.

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
- **Mongo's publish is non-transactional** across the entry-table update and the version-snapshot insert, same stated limitation as agent versioning (see above) and for the same reason (no replica set in the test/deploy Mongo). Delete is transactional on Postgres/SQLite (both statements commit or roll back together) but not on Mongo, for the same reason.
- **Admin lookups by name alone are ambiguous under a cross-tenant name collision.** `registry_entries`' real key is `(tenant_id, name)`, not `name` -- if two tenants both publish "sales-bot", `GET /admin-api/registry/{name}` (system context, no tenant filter) returns an arbitrary match, and `GET /admin-api/registry/{name}/versions` genuinely merges both tenants' histories into one list. An explicit `?tenant_id=` query param on both routes disambiguates by scoping to one tenant; the admin version response also exposes `tenant_id` per entry so a merged (no-param) response is at least distinguishable after the fact. Client-facing routes are unaffected (already tenant-scoped from the caller's own auth context, same as every other resource).

## Architecture

**Control plane** (Go, single static binary):
- Full Agent Protocol HTTP/SSE surface
- State persistence (SQLite default, Postgres opt-in)
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
| Metadata (agents/threads/runs) | Embedded SQLite | Postgres, or MongoDB |
| Job queue + event broker | In-memory | Redis |

Switch backends by setting `POSTGRES_DSN`, `MONGO_URI`, and/or `REDIS_URL`. No code changes, no config files. MongoDB (`internal/state/mongo`) is the project's non-SQL exemplar backend -- proof `state.Store` is genuinely implementable against a document store, and a template for community-contributed backends (MySQL/DynamoDB are documented as possible future drivers, not built). It passes the identical conformance suite Postgres and SQLite do. One caveat: the Python/TypeScript runners' own direct-mode checkpointer (`AsyncPostgresSaver`/its JS equivalent) only exists for Postgres -- a MongoDB-backed control plane's runners use **proxy mode** for checkpoints/store (see below), the same as SQLite deployments do.

### Checkpoint dual mode

The Python runner checkpoints agent state -- separately from the control plane's own thread/run metadata above:

- **Direct mode** (when `POSTGRES_DSN` is set for the runner, same DB as the control plane): uses LangGraph's own `AsyncPostgresSaver`, writing to its own tables (`checkpoints`, `checkpoint_blobs`, `checkpoint_writes`) alongside but separate from the control plane's tables. Thread state survives a runner restart -- verified by killing a runner mid-HITL-interrupt and resuming against a completely fresh process.
- **In-memory fallback** (no `POSTGRES_DSN`): LangGraph's `MemorySaver`. Explicitly ephemeral -- fine for local dev, does not survive a runner restart.

**Crash recovery**: if a runner crashes mid-job, the queue doesn't silently lose it. The control plane tracks every dequeued job as in-flight; a runner's first event (or `ReportStatus`) Acks it, and an unacked job past a short max-age (6s) is automatically reclaimed and redelivered to a live runner. Combined with gRPC keepalive (detects a dead runner connection in ~4s) and a same-instant `ctx.Err()` check in `GetJob`, a crash-and-immediate-resume no longer loses the job -- verified by killing a runner mid-interrupt and resuming against a fresh process with zero added delay. The in-flight tracking is process-local to the control plane node that dequeued the job (fine for a single control-plane instance; a multi-node control plane needs shared lease state, e.g. Redis Streams consumer groups, to extend this guarantee across nodes).

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

### Concurrency (Python runners)

By default a Python runner process (`worker.py` and every `generic_worker`-based adapter) handles exactly one job at a time -- `--concurrency N` / `RUNKITE_CONCURRENCY=N` (default `1`, fully backward compatible) lets it dispatch up to `N` jobs concurrently instead, via a semaphore-bounded dispatcher that spawns one `asyncio.Task` per job. The control plane needs zero changes for this: `GetJob`/`StreamEvents`/`WatchCancels` were already safe for multiple concurrent calls from one runner connection, and the job queue's dequeue is already atomic across concurrent callers. Setting `N` also sizes `RunkiteStore`/`vectorstore.py`'s direct-mode Postgres connection pool (`psycopg_pool.AsyncConnectionPool`, replacing a single shared connection behind a lock) and the checkpointer's pool, so concurrent jobs' store/checkpoint I/O don't serialize on one connection either.

```bash
python -m runkite_runner --config langgraph.json --grpc-address localhost:50051 --concurrency 10
```

**What this actually helps, and what it doesn't**: genuinely effective for agents whose wall-clock time is dominated by *waiting* (slow LLM API calls, tool calls, external HTTP requests) -- many concurrent jobs can overlap productively since each spends most of its time not touching the CPU at all (proven: 20 concurrent runs against the same static graph, zero cross-contamination, combined wall time ~14x faster than the sequential sum -- see `bench/REPORT.md`'s finding 1d). It does **not** let one process exceed one CPU core's worth of throughput for a CPU-bound or near-zero-compute agent (`asyncio` only overlaps I/O waiting, not CPU work, and Python's GIL means one process uses one core at a time) -- confirmed via direct measurement in `bench/REPORT.md`. For that case, run multiple runner processes (replicas) of the same `runner_kind` against the same control plane instead -- already fully supported, zero config changes, since the queue's dispatch is fair across any runner of that kind.

**CrewAI-specific caveat**: a shared `Crew` instance is not safe for concurrent `akickoff()` calls (confirmed by reading crewai's own source -- `kickoff`/`akickoff` write results onto shared instance attributes like `self.usage_metrics`), so `python/adapters/crewai_adapter/adapter.py` serializes concurrent calls on the same `graph_id` via a per-graph lock. This means CrewAI runs sharing a `graph_id` don't get real parallelism from `--concurrency`, only correctness -- LangGraph, LlamaIndex, and plain LangChain runs do get real parallelism (LlamaIndex's adapter was already designed to avoid the equivalent risk, reconstructing `chat_history` per call instead of relying on a shared engine's mutable state).

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

Three more Python runners (`python/adapters/{crewai_adapter,llamaindex_adapter,langchain_adapter}/`), each proving the control plane never assumed LangGraph -- built on a new shared, framework-agnostic loop (`runkite_runner.generic_worker`, extracted from but not replacing `worker.py`'s LangGraph-specific one) that handles only the gRPC polling/streaming/status-reporting mechanics. Each adapter is a thin translation layer implementing just two methods (`load_config`, `execute`), matching the master plan's own "small framework-adapter shim" description:

| | CrewAI | LlamaIndex | Plain LangChain |
|---|---|---|---|
| `runner_kind` | `python-crewai` | `python-llamaindex` | `python-langchain` |
| Loads | a `Crew` (`./crew.py:crew`) | a chat engine/agent (`./chat_engine.py:chat_engine`) | any `Runnable` (`./chain.py:chain`) |
| Executes via | `crew.akickoff(inputs={"input": ...})` | `engine.achat(text, chat_history=...)` | `runnable.ainvoke({"input": ...})` |
| Venv | isolated (`python/adapters/crewai_adapter/.venv`) | isolated (`python/adapters/llamaindex_adapter/.venv`) | shared `python/.venv` (`langchain-core` already a dependency) |
| Example | `examples/crewai_agent/` | `examples/llamaindex_agent/` | `examples/langchain_agent/` |

All three examples are offline and deterministic (a hand-written fake LLM subclass returning a fixed response) -- no API key needed, same convention as `examples/vector_agent`'s fake embeddings. Input/output convention matches the LangGraph runner: extract the last human message's text from `RunAssignment.input.messages`, invoke the framework, append the reply as `{"role": "ai", "content": ...}` -- so client code built against one `runner_kind` doesn't need to change to talk to another.

Cancellation is wired into all three via `generic_worker.run_cancellable` -- each adapter races its single framework call (`akickoff`/`achat`/`ainvoke`) against `cancel_event`, calling `.cancel()` on the underlying task and reporting `interrupted` (not `error`) if the cancel wins, the same outcome a cancelled LangGraph run reports. Live-verified against a real gRPC `WatchCancels` signal, not just a unit-test mock.

**Why CrewAI and LlamaIndex get their own isolated venv** (plain LangChain doesn't need one): confirmed live during development -- installing `crewai` into the shared `python/.venv` silently downgraded `protobuf`, a dependency the production LangGraph runner's generated gRPC stubs are version-sensitive about (see the Runner Protocol section's protobuf note). Real deployments would run each framework's runner as a genuinely separate process anyway (arguably a separate container), so an isolated venv here matches that reality rather than fighting it. Setup:

```bash
cd python/adapters/crewai_adapter        # or llamaindex_adapter
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python crewai grpcio protobuf   # or: llama-index-core grpcio protobuf

cd ../../../examples/crewai_agent
PYTHONPATH=<repo>/python:<repo>/python/adapters \
  <repo>/python/adapters/crewai_adapter/.venv/bin/python -m crewai_adapter \
  --config langgraph.json --grpc-address localhost:50051
```

Live-verified end to end for all three: a real control plane, a real runner process for each framework, a real `POST /threads/{id}/runs` request through to a real thread-values response containing that framework's actual output. `make test-adapters` runs the CrewAI/LlamaIndex unit tests once their venvs exist (`test_generic_worker.py`/`test_langchain_adapter.py` cover the shared loop and plain LangChain respectively, and run in CI/`make test-python` alongside the rest of the Python suite -- CrewAI/LlamaIndex's isolated-venv tests run in a dedicated CI step instead, for the same shared-venv-isolation reason above). Plain LangChain additionally has automated, CI-gated end-to-end coverage (`test/e2e/adapters/`, part of `make test-e2e`) -- a real control plane dispatching to a real `langchain_adapter` runner subprocess over real gRPC, including cancellation via a real `WatchCancels` signal, not just the unit-level fakes. CrewAI/LlamaIndex don't have the equivalent e2e tier yet (their isolated venvs make it more setup than LangChain's shared one) -- live-verified manually during development, same as before, just not CI-gated.

## Docker

### Full-stack deployment

`docker-compose.yml` runs the whole system -- Postgres, Redis, the control plane, and a Python runner -- built from `Dockerfile` and `Dockerfile.runner`:

```bash
docker compose up -d --build
curl http://localhost:2026/health
docker compose down -v
```

Runner auth is enabled by default in this compose file (`RUNNER_TOKEN_PYTHON_LANGGRAPH`). Set your own token via the `RUNNER_TOKEN` env var before starting, or it falls back to a placeholder -- change it for any real deployment.

`docker-compose.dev.yml` is the zero-dependency local stack — control plane + Python runner only (SQLite + in-memory transport, no Postgres/Redis), with source-mounted `examples/` and `python/`:

```bash
docker compose -f docker-compose.dev.yml up -d --build
```

Full-stack compose uses Postgres/Redis on 5432/6379; stop local services on those ports first, or override via a compose override file.

### Test infrastructure

`docker-compose.test.yml` starts ephemeral, tmpfs-backed Postgres + Redis + MongoDB for running the conformance test suite (see Development below), on non-standard ports (5433/6380/27018) specifically to avoid colliding with the full-stack compose above or any local services:

```bash
docker compose -f docker-compose.test.yml up -d
make test-all
docker compose -f docker-compose.test.yml down -v
```

## Development

```bash
make test           # SQLite + in-memory only (no external deps)
make test-pg        # Postgres conformance (requires infra-up)
make test-redis     # Redis conformance (requires infra-up)
make test-mongo     # MongoDB conformance (requires infra-up)
make test-all       # All backends (requires infra-up)
make test-e2e       # Black-box E2E: real binary + real runner + PG/Redis (requires infra-up)
make test-python    # Python runner unit tests
make test-ts        # TypeScript runner unit tests
make test-adapters  # CrewAI/LlamaIndex adapter unit tests (requires their isolated venvs, see python/adapters/*/README.md)
make infra-up       # Start ephemeral Postgres + Redis + MongoDB via Docker
make infra-down     # Stop test infrastructure
make vet            # go vet
make build          # Build the binary
```

## API Reference

### Agents
```
POST   /agents/search                          Search/list agents
GET    /agents/{id}                            Get agent details
GET    /agents/{id}/schemas                    Get agent input/output schemas
GET    /agents/{id}/versions                   List version history (newest first)
POST   /agents/{id}/versions/{v}/rollback      Roll back to an old version's content
```

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
POST   /runs/{runID}/cancel        Cancel run by ID
```

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

### Health & Observability
```
GET    /health                     Returns {"status": "ok"}
GET    /metrics                    Prometheus metrics (outside auth)
```

Metrics exposed: `runkite_http_requests_total`, `runkite_http_request_duration_seconds`, `runkite_runs_total`, `runkite_run_duration_seconds`, `runkite_active_runs`, `runkite_queue_depth`, `runkite_active_sse_connections`. HTTP path labels are normalized (UUIDs and resource IDs become `{id}`) to keep cardinality bounded.

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
| `examples/cron_agent/` | Cron-scheduled daily run (master plan: "Cron scheduler") |
| `examples/store_agent/` | Uses `get_store()` to prove Store Dual Mode's direct/proxy interop |
| `examples/custom_routes_agent/` | FastAPI app hosted in-runner, reachable via `/custom/*` |
| `examples/echo_agent_ts/` | Echo, slow (cancel), and approval (HITL) agents, in TypeScript/LangGraph.js -- proves the Runner Protocol is language-agnostic |
| `examples/vector_agent/` | Retrieval-augmented demo using `RunkiteVectorStore` (Vector Store Dual Mode) -- fake, deterministic embeddings, no API key needed |
| `examples/factory_agent/` | Per-request Factory Graph -- proves fresh-instance-per-run isolation and `runtime.user` identity passthrough |
| `examples/a2a_agent/` | `coordinator_agent` delegates to `worker_agent` mid-execution via `call_agent` -- proves Agent-to-Agent delegation end to end (see [Agent-to-Agent (A2A)](#agent-to-agent-a2a)) |

## Known Limitations

Honest gaps, not hidden ones:

- **Crash recovery is single-control-plane-node only.** In-flight job tracking (Ack/Nack/reclaim) lives in the memory of whichever control plane process dequeued the job. This is correct and sufficient for a single control-plane instance (the default and most common deployment), but a multi-node control plane needs shared lease state (e.g. Redis Streams consumer groups) to extend the same crash-recovery guarantee across nodes -- not yet built.
- **Authorization is coarse-grained.** Permissions are enforced at `read`/`write`/`admin` method-level granularity (see Auth section), not per-resource ACLs.
- **`db downgrade` isn't implemented.** The schema is a single idempotent migration, not versioned up/down migrations.
- **OTel tracing covers the control plane, not runner-internal spans.** Neither runner's own LLM calls, tool calls, etc. are wrapped in OTel spans yet -- but the run's real W3C `traceparent` is already propagated to both (`RunAssignment.trace_context`), so a runner-side OTel integration (e.g. wiring LangChain's tracing callback to the same propagator) has what it needs and nests correctly under the same trace the moment it's added.
- **Direct-mode store/checkpoint access is single-tenant regardless of multi-tenancy config.** See the Multi-tenancy section's "Known gaps" -- this is the master plan's own documented Direct Mode Trust Model trade-off, not new.
- **No central tenant registry, and no user/API-key management UI.** See Multi-tenancy and Admin UI -- a tenant exists implicitly the moment a resource is tagged with it, and there's no persisted user/API-key table to manage in the first place (`api_key` entries are static config).
- **`on_tool_call` requires runner cooperation.** The control plane fires it whenever it sees a RunEvent with `method: "tool_call"`, but doesn't parse LangGraph/LangChain message shapes itself (staying framework-agnostic) -- it's up to a framework-aware runner to emit that method. `worker.py` (the flagship LangGraph runner) does: `find_new_tool_calls` scans every stream chunk for `AIMessage.tool_calls`, deduping by tool-call `id` so a message seen in both `values` and `updates` mode (if both are requested) only fires once. Live-verified end to end: `examples/react_agent` (a real `StateGraph` + `ToolNode`, not a mock) run through the real control plane + real Python runner, with a webhook configured for `tool_call`, actually delivers `{"name":"search","args":{...},"id":"call_001"}` to an external receiver. `generic_worker.py` and the TypeScript runner don't emit it yet -- `run_start`/`run_complete`/`error`/`interrupt` are fully wired for every runner and fire from real control-plane-observed lifecycle transitions regardless.
- **In-runner custom routes are ASGI-only.** `uvicorn` (the SDK's only custom-routes dependency) doesn't serve WSGI apps -- Flask needs an adapter (e.g. `a2wsgi.WSGIMiddleware`) wrapped around it first. Sidecar mode has no such restriction (any language, any framework).
- **TypeScript runner's Docker image (`Dockerfile.runner-ts`) is built and live-verified** (a real run against `echo_agent_ts` through `docker-compose.ts.yml` completes and echoes back correctly), but its own VG-001/002/003-equivalent verification is otherwise manual-only, not regression-guarded in CI (same as cron's multi-instance claim test).
- **Vector store supports pgvector and Qdrant.** Weaviate and Pinecone remain Tier 2 (experimental) and not yet built -- see the Vector Store section.
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
