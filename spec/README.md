# OpenAPI Specifications

Runkite ships three OpenAPI 3.1.0 specs covering the full HTTP surface:

| File | Scope |
|---|---|
| `openapi.json` | **Public client API** — Agent Protocol v0.1.6 plus Runkite platform extensions (registry, vectors, cost, MCP, streaming protocol, pprof). SDK authors should use this file. |
| `openapi-admin.json` | **Admin API** — `/admin-api/*` cross-tenant management surface. Requires `admin` permission. |
| `openapi-internal.json` | **Internal (Runner) API** — `/internal/*` runner-facing endpoints (A2A delegation, connector proxy, store/vectors proxy mode, schema reporting). Authenticated via `X-Runner-Kind` + `X-Runner-Token` headers. |

## Regenerating

```bash
python3 scripts/openapi/build.py
```

`build.py` loads `spec/openapi.json` as its base, patches it with all missing paths/schemas, and writes all three files. The output is idempotent — running it twice produces byte-identical files.

## Checking completeness

```bash
python3 scripts/openapi/check.py
```

`check.py` extracts every `HandleFunc`/`Handle` registration from Go source and verifies each one appears in the appropriate spec. Exit 0 means complete coverage; exit 1 prints a list of missing routes.

Both targets are also available via `make openapi` and `make openapi-check`.

## Path parameter naming

Go's `net/http` mux uses camelCase path parameters (`{agentID}`, `{threadID}`, `{runID}`). OpenAPI specs use snake_case (`{agent_id}`, `{thread_id}`, `{run_id}`). The wire paths are identical — this is purely a documentation convention difference. `extract.py` normalizes Go names to snake_case for comparison.

## Authentication schemes

### Public API (`openapi.json`)

- **BearerAuth** — `Authorization: Bearer <token>` where the token is a JWT (`auth.type=jwt`) or an opaque API key (`auth.type=api_key`).
- **ApiKeyAuth** — `X-API-Key: <key>` as an alternative for `auth.type=api_key` (same keys as Bearer).
- Health/livez/readyz, metrics, and the admin UI static files are unauthenticated (`security: []`).

### Admin API (`openapi-admin.json`)

- **BearerAuth** / **ApiKeyAuth** — Admin keys (`auth.admin_keys` in `langgraph.json`) or the primary auth provider with `admin` permission.

### Internal API (`openapi-internal.json`)

- **RunnerAuth** — `X-Runner-Kind` + `X-Runner-Token` headers. Configured via `RUNNER_TOKEN_*` environment variables. Disabled in local/dev mode.

## Clients & releases

- How to call Runkite today (LangGraph SDK, curl) and when we might ship a first-party client package: [`docs/client-sdk.md`](../docs/client-sdk.md).
- On each `v*` git tag, `.github/workflows/release.yml` attaches these three JSON files to the GitHub Release (via GoReleaser).
