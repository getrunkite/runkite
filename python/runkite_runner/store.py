"""Store Dual Mode for the Python runner.

Mirrors the checkpoint dual-mode pattern in checkpoint.py, applied to the
control plane's unified key-value store instead of checkpoints:

- direct mode (POSTGRES_DSN set): queries the control plane's own
  `store_items` table straight over psycopg -- same schema, same \\x1F
  namespace encoding as internal/state/postgres/postgres.go. Zero HTTP hop.
  Always operates in the "default" tenant (see _TENANT_ID below) -- this
  bypasses the control plane's tenant-scoping entirely, a known trust-model
  trade-off: direct mode bypasses control-plane authz on checkpoint/store
  bytes in multi-tenant deployments. Proxy mode is the recommended
  mitigation for real per-tenant isolation.
- proxy mode (no POSTGRES_DSN): calls the control plane's /store/* HTTP
  API over httpx. Works against any backend (SQLite, Postgres).

Both modes read/write the exact same rows, so a value written by a Go
client via HTTP is immediately visible to a Python graph via direct mode,
and vice versa -- one store, not two competing systems.

`BaseStore` only requires `batch`/`abatch` to be implemented; every other
method (get/put/delete/search/list_namespaces, sync+async) is provided by
langgraph's base class in terms of these two.

Connection pooling (runner-side concurrency): direct mode used to hold
ONE shared psycopg connection serialized by an asyncio.Lock -- correct
under concurrent runs (the lock prevents interleaved writes on one
connection) but not actually parallel, since every store op from every
concurrent job queued behind that same lock. Now backed by a
psycopg_pool.AsyncConnectionPool sized to the runner's configured
concurrency (see worker.py's --concurrency / RunkiteStore's pool_size),
so concurrent jobs' store ops run on genuinely separate connections.

TTL support (real gap found via a live agent that called
store.aput(..., ttl=...): "TTL is not supported by RunkiteStore" --
deepagents / LangGraph BaseStore's own guard rejected the call before
it even reached batch/abatch, because this class never declared
supports_ttl). PutOp.ttl/GetOp.refresh_ttl/SearchOp.refresh_ttl
arrive already resolved by BaseStore (NOT_PROVIDED defaults applied via
self.ttl_config before construction) -- see _direct_one/_proxy_one's
PutOp/GetOp/SearchOp branches, which pass them straight through to the
control plane's store_items ttl_minutes/expires_at columns
(internal/state/*/*.go's PutItem/GetItem/SearchItems).
"""

from __future__ import annotations

import asyncio
import json
from collections.abc import Iterable
from datetime import datetime, timedelta, timezone
from typing import Any

import httpx
from langgraph.store.base import (
    BaseStore,
    GetOp,
    Item,
    ListNamespacesOp,
    Op,
    PutOp,
    Result,
    SearchItem,
    SearchOp,
)

from .tls_utils import httpx_tls_kwargs

_NS_DELIM = "\x1f"

# Direct mode has no per-request tenant identity to work with (it's a raw
# DB connection, not an authenticated HTTP call) -- see the module
# docstring's Direct Mode Trust Model note. Must match
# internal/tenant.DefaultTenant on the Go side exactly.
_TENANT_ID = "default"


def _ns_to_string(ns: tuple[str, ...]) -> str:
    return _NS_DELIM + _NS_DELIM.join(ns) + _NS_DELIM


def _string_to_ns(s: str) -> tuple[str, ...]:
    trimmed = s.strip(_NS_DELIM)
    return tuple(trimmed.split(_NS_DELIM)) if trimmed else ()


def _ns_prefix_pattern(prefix: tuple[str, ...]) -> str:
    if not prefix:
        return "%"
    return _NS_DELIM + _NS_DELIM.join(prefix) + _NS_DELIM + "%"


def _timedelta_minutes(minutes: float) -> timedelta:
    return timedelta(minutes=minutes)


def _parse_ts(value: Any) -> datetime:
    if isinstance(value, datetime):
        return value
    if isinstance(value, str) and value:
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    return datetime.now(timezone.utc)


def _item_from_json(d: dict) -> Item:
    return Item(
        namespace=tuple(d["namespace"] or ()),
        key=d["key"],
        value=d.get("value") or {},
        created_at=_parse_ts(d.get("created_at")),
        updated_at=_parse_ts(d.get("updated_at")),
    )


def _search_item_from_json(d: dict) -> SearchItem:
    return SearchItem(
        namespace=tuple(d["namespace"] or ()),
        key=d["key"],
        value=d.get("value") or {},
        created_at=_parse_ts(d.get("created_at")),
        updated_at=_parse_ts(d.get("updated_at")),
    )


def _split_match_conditions(
    match_conditions: tuple | None,
) -> tuple[tuple[str, ...] | None, tuple[str, ...] | None]:
    """Extract prefix/suffix paths, matching the control plane's LIKE-based
    matching (no wildcard-segment support on either side of the dual mode)."""
    prefix = suffix = None
    for cond in match_conditions or ():
        if cond.match_type == "prefix":
            prefix = tuple(cond.path)
        elif cond.match_type == "suffix":
            suffix = tuple(cond.path)
    return prefix, suffix


class RunkiteStore(BaseStore):
    """LangGraph BaseStore backed by the Runkite control plane's store_items table."""

    supports_ttl = True

    def __init__(
        self,
        *,
        postgres_dsn: str | None = None,
        http_base_url: str | None = None,
        runner_token: str | None = None,
        ttl_config: dict | None = None,
        pool_size: int = 4,
    ):
        if not postgres_dsn and not http_base_url:
            raise ValueError("RunkiteStore requires postgres_dsn or http_base_url")
        # BaseStore reads self.ttl_config to resolve NOT_PROVIDED ttl/
        # refresh_ttl defaults before PutOp/GetOp/SearchOp are even
        # constructed -- see this module's docstring.
        self.ttl_config = ttl_config
        self._dsn = postgres_dsn
        self._base_url = http_base_url.rstrip("/") if http_base_url else None
        self._headers: dict[str, str] = {}
        if runner_token:
            self._headers["X-Runner-Kind"] = "python-langgraph"
            self._headers["X-Runner-Token"] = runner_token
        self.mode = "direct" if postgres_dsn else "proxy"
        self._pool_size = pool_size
        self._pool = None  # AsyncConnectionPool, opened lazily on first direct-mode use
        # Guards only pool CREATION (a one-time, rare event), not every
        # store op -- unlike the old single-connection lock, this doesn't
        # serialize concurrent jobs' store ops against each other.
        self._pool_init_lock = asyncio.Lock()

    async def _get_pool(self):
        if self._pool is None:
            async with self._pool_init_lock:
                if self._pool is None:  # re-check: lost the race to open() below
                    from psycopg_pool import AsyncConnectionPool

                    pool = AsyncConnectionPool(
                        self._dsn,
                        min_size=1,
                        max_size=max(self._pool_size, 1),
                        kwargs={"autocommit": True},
                        open=False,
                    )
                    await pool.open()
                    self._pool = pool
        return self._pool

    async def aclose(self) -> None:
        if self._pool is not None:
            await self._pool.close()
            self._pool = None

    # -- BaseStore abstract interface --------------------------------------

    def batch(self, ops: Iterable[Op]) -> list[Result]:
        # LangGraph sync nodes (store.get/put) typically run in a worker
        # thread with no event loop, so asyncio.run is fine. If somehow
        # called from a thread that already has a loop, hop to a fresh
        # thread so we don't raise "asyncio.run() cannot be called from a
        # running event loop".
        try:
            asyncio.get_running_loop()
        except RuntimeError:
            return asyncio.run(self.abatch(ops))
        import concurrent.futures

        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
            return pool.submit(lambda: asyncio.run(self.abatch(list(ops)))).result()

    async def abatch(self, ops: Iterable[Op]) -> list[Result]:
        ops = list(ops)
        if self.mode == "direct":
            return await self._abatch_direct(ops)
        return await self._abatch_proxy(ops)

    # -- proxy mode: HTTP calls to the control plane -----------------------

    async def _abatch_proxy(self, ops: list[Op]) -> list[Result]:
        async with httpx.AsyncClient(
            base_url=self._base_url, headers=self._headers, timeout=10.0, **httpx_tls_kwargs()
        ) as client:
            return [await self._proxy_one(client, op) for op in ops]

    async def _proxy_one(self, client: httpx.AsyncClient, op: Op) -> Result:
        # /internal/store/* (not the client-facing /store/*): a runner
        # authenticates with its runner token, not a client API key/JWT it
        # may not have. Same handlers on the Go side, different auth
        # boundary -- see internal/auth/auth.go.
        if isinstance(op, GetOp):
            resp = await client.get(
                "/internal/store/items",
                params={
                    "namespace": ",".join(op.namespace),
                    "key": op.key,
                    "refresh_ttl": "true" if op.refresh_ttl else "false",
                },
            )
            if resp.status_code == 404:
                return None
            resp.raise_for_status()
            return _item_from_json(resp.json())

        if isinstance(op, PutOp):
            if op.value is None:
                resp = await client.request(
                    "DELETE",
                    "/internal/store/items",
                    json={"namespace": list(op.namespace), "key": op.key},
                )
            else:
                resp = await client.put(
                    "/internal/store/items",
                    json={
                        "namespace": list(op.namespace),
                        "key": op.key,
                        "value": op.value,
                        "ttl_minutes": op.ttl,
                    },
                )
            resp.raise_for_status()
            return None

        if isinstance(op, SearchOp):
            resp = await client.post(
                "/internal/store/items/search",
                json={
                    "namespace_prefix": list(op.namespace_prefix),
                    "filter": op.filter or {},
                    "limit": op.limit,
                    "offset": op.offset,
                    "refresh_ttl": op.refresh_ttl,
                },
            )
            resp.raise_for_status()
            items = resp.json().get("items") or []
            return [_search_item_from_json(i) for i in items]

        if isinstance(op, ListNamespacesOp):
            prefix, suffix = _split_match_conditions(op.match_conditions)
            resp = await client.post(
                "/internal/store/namespaces",
                json={
                    "prefix": list(prefix) if prefix else None,
                    "suffix": list(suffix) if suffix else None,
                    "max_depth": op.max_depth,
                    "limit": op.limit,
                    "offset": op.offset,
                },
            )
            resp.raise_for_status()
            return [tuple(ns) for ns in (resp.json() or [])]

        raise TypeError(f"unsupported store op: {type(op)!r}")

    # -- direct mode: psycopg straight to store_items ------------------------

    async def _abatch_direct(self, ops: list[Op]) -> list[Result]:
        pool = await self._get_pool()
        async with pool.connection() as conn:
            results: list[Result] = []
            for op in ops:
                results.append(await self._direct_one(conn, op))
            return results

    async def _direct_one(self, conn, op: Op) -> Result:
        if isinstance(op, GetOp):
            ns = _ns_to_string(op.namespace)
            async with conn.cursor() as cur:
                # Mirrors internal/state/postgres/postgres.go's GetItem:
                # an expired item (expires_at in the past) reads as
                # absent, and a refreshing read extends expires_at from
                # now using the item's own stored ttl_minutes.
                await cur.execute(
                    "SELECT namespace, key, value, created_at, updated_at, ttl_minutes "
                    "FROM store_items WHERE tenant_id = %s AND namespace = %s AND key = %s "
                    "AND (expires_at IS NULL OR expires_at > NOW())",
                    (_TENANT_ID, ns, op.key),
                )
                row = await cur.fetchone()
                if row is None:
                    return None
                ttl_minutes = row[5]
                if op.refresh_ttl and ttl_minutes is not None:
                    new_expiry = datetime.now(timezone.utc) + _timedelta_minutes(ttl_minutes)
                    await cur.execute(
                        "UPDATE store_items SET expires_at = %s WHERE tenant_id = %s AND namespace = %s AND key = %s",
                        (new_expiry, _TENANT_ID, ns, op.key),
                    )
                return _item_from_row(row[:5])

        if isinstance(op, PutOp):
            ns = _ns_to_string(op.namespace)
            # Computed in Python (not SQL interval arithmetic) to match
            # storeItemExpiresAt's approach on the Go side exactly, and
            # to avoid psycopg parameter-type ambiguity from reusing a
            # possibly-None placeholder in both a NULL check and interval
            # math.
            expires_at = None if op.ttl is None else datetime.now(timezone.utc) + _timedelta_minutes(op.ttl)
            async with conn.cursor() as cur:
                if op.value is None:
                    await cur.execute(
                        "DELETE FROM store_items WHERE tenant_id = %s AND namespace = %s AND key = %s",
                        (_TENANT_ID, ns, op.key),
                    )
                else:
                    await cur.execute(
                        """
                        INSERT INTO store_items (tenant_id, namespace, key, value, created_at, updated_at, ttl_minutes, expires_at)
                        VALUES (%s, %s, %s, %s, NOW(), NOW(), %s, %s)
                        ON CONFLICT (tenant_id, namespace, key)
                        DO UPDATE SET value = EXCLUDED.value, updated_at = NOW(),
                            ttl_minutes = EXCLUDED.ttl_minutes, expires_at = EXCLUDED.expires_at
                        """,
                        (_TENANT_ID, ns, op.key, json.dumps(op.value), op.ttl, expires_at),
                    )
            return None

        if isinstance(op, SearchOp):
            where = ["tenant_id = %s", "namespace LIKE %s", "(expires_at IS NULL OR expires_at > NOW())"]
            args: list[Any] = [_TENANT_ID, _ns_prefix_pattern(op.namespace_prefix)]
            for k, v in (op.filter or {}).items():
                where.append("value->>%s = %s")
                args.append(k)
                args.append(v if isinstance(v, str) else json.dumps(v))
            limit = op.limit or 10
            query = (
                "SELECT namespace, key, value, created_at, updated_at, ttl_minutes FROM store_items "
                f"WHERE {' AND '.join(where)} ORDER BY updated_at DESC LIMIT %s OFFSET %s"
            )
            args.extend([limit, op.offset])
            async with conn.cursor() as cur:
                await cur.execute(query, args)
                rows = await cur.fetchall()
                items = [_search_item_from_row(r[:5]) for r in rows]
                if op.refresh_ttl:
                    now = datetime.now(timezone.utc)
                    for r in rows:
                        ttl_minutes = r[5]
                        if ttl_minutes is not None:
                            await cur.execute(
                                "UPDATE store_items SET expires_at = %s WHERE tenant_id = %s AND namespace = %s AND key = %s",
                                (now + _timedelta_minutes(ttl_minutes), _TENANT_ID, r[0], r[1]),
                            )
                return items

        if isinstance(op, ListNamespacesOp):
            prefix, _suffix = _split_match_conditions(op.match_conditions)
            where = ["tenant_id = %s"]
            args = [_TENANT_ID]
            if prefix:
                where.append("namespace LIKE %s")
                args.append(_ns_prefix_pattern(prefix))
            query = "SELECT DISTINCT namespace FROM store_items WHERE " + " AND ".join(where)
            limit = op.limit or 100
            query += " ORDER BY namespace LIMIT %s OFFSET %s"
            args.extend([limit, op.offset])
            async with conn.cursor() as cur:
                await cur.execute(query, args)
                rows = await cur.fetchall()
                return [_string_to_ns(r[0]) for r in rows]

        raise TypeError(f"unsupported store op: {type(op)!r}")


def _item_from_row(row) -> Item:
    ns_str, key, value, created_at, updated_at = row
    return Item(
        namespace=_string_to_ns(ns_str),
        key=key,
        value=value if isinstance(value, dict) else json.loads(value or "{}"),
        created_at=_parse_ts(created_at),
        updated_at=_parse_ts(updated_at),
    )


def _search_item_from_row(row) -> SearchItem:
    item = _item_from_row(row)
    return SearchItem(
        namespace=item.namespace,
        key=item.key,
        value=item.value,
        created_at=item.created_at,
        updated_at=item.updated_at,
    )
