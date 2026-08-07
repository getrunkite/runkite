# Auth

> Deep dive moved from the root README. For a 60-second overview see the [root README](../README.md).

Four providers, configured via `auth` in `langgraph.json`:

| Type | Description |
|------|-------------|
| `none` (default) | No auth -- all requests pass through |
| `api_key` | Static API keys with per-key permissions |
| `jwt` | Validate tokens against a JWKS endpoint, extract claims |
| `webhook` | Forward headers to a sidecar service for custom auth logic |

**Authorization**: `permissions` set on an API key, JWT claim, or webhook response are enforced -- `read` is required for GET/HEAD, `write` for everything else (`admin` bypasses both). When `auth.type` is `api_key`, `jwt`, or `webhook`, **empty permissions fail closed by default** (`403`) -- every caller must carry an explicit `read`/`write`/`admin` allow-list. Set `"strict_permissions": false` only for local/dev if you intentionally want the old empty=unrestricted behavior. `"strict_permissions": true` is always fail-closed (same as the default for those provider types).

**`admin_keys`** (optional, under the same `auth` object) is an independent credential set accepted **only** for `/admin-api/*` -- every key implicitly grants `admin`. Useful when the primary provider is short-lived SSO and an operator wants a stable break-glass key for the dashboard without minting JWTs. It is additive: a primary credential that itself carries `admin` still works, and a missing/invalid admin key still falls through to a configured primary provider (so a normal SSO user with `admin` permission works too). With **no** primary `auth.type` configured at all, `admin_keys` fails closed on `/admin-api/*` -- a missing/invalid admin credential gets `401`, not the client-facing surface's local-dev trust-everyone. Configuring a credential never leaves the dashboard less protected than configuring none.

**Admin UI login** (`POST /admin-api/session`) exchanges a pasted API key/JWT for an httpOnly `runkite_admin_session` cookie; the browser never keeps the credential in JavaScript. Mutating dashboard calls send `X-CSRF-Token` from the login response. `GET /admin-api/session` rehydrates CSRF after refresh; `DELETE /admin-api/session` logs out. Curl/OpenAPI keep using `Authorization: Bearer` on `/admin-api/*` with no CSRF.

**Runner auth** is separate: in local mode runners are trusted implicitly. In production, set `RUNNER_TOKEN_<kind>` env vars -- one shared token per runner type, validated on every gRPC call and `/internal/*` HTTP request. Optionally set `RUNNER_TENANTS_<kind>` (comma-separated tenant ids, same kind encoding as tokens) to restrict which tenants that kind may use; unset means any tenant is still accepted after kind-token auth. A missing tenant is treated as `default` for the allow check.

**Run-binding** applies to proxy-mode store/vector calls and connector `session`/`mcp`: runners must send `X-Runkite-Run-Id` + `X-Runkite-Generation` for an in-flight assignment. The control plane derives `tenant_id` (and agent id) from that assignment — it does **not** trust `X-Runkite-Tenant-Id` on those paths. See [Trust & governance](trust-governance.md).

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
- **Direct-mode LangGraph checkpoints are keyed by a tenant-aware `thread_id`.** `store_items` / `vector_items` follow `RunAssignment.tenant_id` (and runner proxy calls send `X-Runkite-Tenant-Id` on `/internal/*`). LangGraph's Postgres checkpointer tables (`checkpoints`, `checkpoint_blobs`, `checkpoint_writes`) have no `tenant_id` column -- runners instead set `configurable.thread_id` to `{tenant_id}:{thread_id}` for non-default tenants (bare `thread_id` when tenant is `default` or absent, so pre-existing single-tenant checkpoints keep working). Control-plane / assignment `thread_id` stays unprefixed. Avoid `:` inside tenant ids (encoding uses a single colon). Historical non-default-tenant rows written under a bare `thread_id` before this encoding are not auto-rewritten.
- **Proxy-mode store/vector and connector `session`/`mcp` are run-bound.** After runner-token auth, those paths require `X-Runkite-Run-Id` + `X-Runkite-Generation` matching an in-flight assignment; tenant/agent come from the assignment, not from `X-Runkite-Tenant-Id`. Other `/internal/*` routes (schema publish, status, A2A, connector metadata) still honor the tenant header after kind-token auth — set `RUNNER_TENANTS_<kind>` in multi-tenant deployments so a leaked kind token cannot claim arbitrary tenants there. Direct-mode store access still bypasses the control plane entirely.
- **No central tenant registry.** A tenant "exists" the moment any resource is tagged with it -- there's no `POST /tenants` to pre-create one, list all known tenants, or manage tenant-level settings. Fine for a flat, claim-driven model; would matter if a full org/workspace/team hierarchy is built later. The Admin UI's Overview/Agents/Threads/Runs/Cron views do surface `tenant_id` per row across every tenant (see Admin UI below), which covers *visibility* -- there's just no dedicated tenant *management* surface yet.
- **A pre-existing (upgraded, not freshly created) database keeps its original single-column primary keys** on `agents`/`agent_schemas`/`store_items`/`cron_schedules`/`cron_claims` even after the migration adds `tenant_id` -- SQLite and Postgres both refuse to widen a primary key in place. Explicit unique indexes are created alongside so upserts still work correctly either way; every query still filters by `tenant_id` regardless, so isolation itself holds on both fresh and upgraded databases -- confirmed by running the full conformance suite against a real pre-existing (pre-multi-tenancy) Postgres database, not just fresh ones.
