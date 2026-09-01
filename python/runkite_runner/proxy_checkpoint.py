"""HTTP proxy BaseCheckpointSaver (opaque blobs via control plane §6.2).

Used when POSTGRES_DSN is unset but RUNKITE_HTTP_URL is set -- persistence
survives runner restarts against any CP backend (SQLite/MySQL/Mongo/Postgres)
without giving the runner DB credentials.

Blob format (owned by this saver, opaque to the CP):
  {
    "v": 1,
    "checkpoint": <serde dumps_typed>,
    "metadata": <serde dumps_typed>,
    "parent_checkpoint_id": <str|null>,
    "writes": [[task_id, channel, dumps_typed], ...]
  }

checkpoint_ns is folded into the CP path key as "{ns}\\x1f{checkpoint_id}"
(empty ns → bare checkpoint_id) so subgraphs do not collide.

HTTP paths use the *bare* assignment thread_id (not the tenant-prefixed
configurable.thread_id used by AsyncPostgresSaver). Prefixing is only for
direct Postgres tables that lack a tenant_id column; proxy rows already
carry tenant_id and run-binding checks against the assignment thread_id.

aput / aput_writes serialize per blob key with an in-process asyncio.Lock
so parallel LangGraph tasks in one runner do not lose writes via
GET-modify-PUT. Across processes/replicas, PUTs carry If-Match (ETag)
and retry on 412 so concurrent writers merge rather than silently clobber.
aput_writes without configurable.checkpoint_id raises ValueError (no
silent no-op that would drop mid-superstep channels).

LangGraph's Pregel order calls aput_writes for a *new* checkpoint_id
*before* aput creates that blob — so a 404 here is normal, not an error.
Both aput and aput_writes create with If-None-Match: * on 404; if a peer
already won, 412 triggers a GET+merge retry instead of clobbering (so an
unconditional aput cannot wipe an aput_writes shell across replicas).

aget_tuple without checkpoint_id uses GET .../latest?ns= (one round-trip)
instead of list+N get.
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import AsyncIterator, Iterator, Sequence
from typing import Any
from urllib.parse import quote, unquote

import httpx
from langchain_core.runnables import RunnableConfig
from langgraph.checkpoint.base import (
    BaseCheckpointSaver,
    ChannelVersions,
    Checkpoint,
    CheckpointMetadata,
    CheckpointTuple,
    get_checkpoint_id,
)

from .tenant_ctx import storage_thread_id, tenant_headers
from .tls_utils import httpx_tls_kwargs

logger = logging.getLogger("runkite.checkpoint.proxy")

_NS_SEP = "\x1f"
_HEADER_FRAMEWORK = "X-Runkite-Checkpoint-Framework"
_LIST_FETCH_LIMIT = 1000
_CAS_MAX_ATTEMPTS = 8


class _CASConflict(Exception):
    """If-Match lost the race; caller should re-read and retry."""


def _blob_key(checkpoint_ns: str, checkpoint_id: str) -> str:
    if not checkpoint_ns:
        return checkpoint_id
    return f"{checkpoint_ns}{_NS_SEP}{checkpoint_id}"


def _parse_blob_key(key: str) -> tuple[str, str]:
    if _NS_SEP in key:
        ns, _, cid = key.partition(_NS_SEP)
        return ns, cid
    return "", key


def _as_typed(value: Any) -> tuple[str, bytes]:
    """loads_typed wants (type, bytes); wire codecs may yield a 2-list."""
    if isinstance(value, tuple) and len(value) == 2:
        return value[0], value[1]
    if isinstance(value, list) and len(value) == 2:
        return value[0], value[1]
    raise TypeError(f"expected typed (type, bytes), got {type(value)!r}")


def _run(coro):
    try:
        asyncio.get_running_loop()
    except RuntimeError:
        return asyncio.run(coro)
    raise RuntimeError(
        "ProxyCheckpointSaver sync methods cannot run inside a running "
        "event loop; use aget_tuple/aput/alist/aput_writes instead"
    )


class ProxyCheckpointSaver(BaseCheckpointSaver):
    """LangGraph checkpointer backed by PUT/GET /internal/checkpoints/*."""

    def __init__(
        self,
        *,
        http_base_url: str,
        runner_token: str | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
    ):
        super().__init__()
        self._base = http_base_url.rstrip("/")
        self._headers: dict[str, str] = {_HEADER_FRAMEWORK: "langgraph"}
        if runner_token:
            self._headers["Authorization"] = f"Bearer {runner_token}"
            self._headers["X-Runner-Token"] = runner_token
        self._transport = transport
        self._client: httpx.AsyncClient | None = None
        self._locks: dict[str, asyncio.Lock] = {}
        self._locks_guard = asyncio.Lock()

    async def _http(self) -> httpx.AsyncClient:
        if self._client is None:
            kwargs: dict[str, Any] = {"timeout": 30.0, **httpx_tls_kwargs()}
            if self._transport is not None:
                kwargs["transport"] = self._transport
            self._client = httpx.AsyncClient(**kwargs)
        return self._client

    async def aclose(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    async def _lock_for(self, thread_id: str, key: str) -> asyncio.Lock:
        lock_key = f"{thread_id}\0{key}"
        async with self._locks_guard:
            lock = self._locks.get(lock_key)
            if lock is None:
                lock = asyncio.Lock()
                self._locks[lock_key] = lock
            return lock

    def _url(self, thread_id: str, checkpoint_id: str | None = None) -> str:
        base = f"{self._base}/internal/checkpoints/{quote(thread_id, safe='')}"
        if checkpoint_id is None:
            return base
        return f"{base}/{quote(checkpoint_id, safe='')}"

    def _latest_url(self, thread_id: str) -> str:
        return f"{self._base}/internal/checkpoints/{quote(thread_id, safe='')}/latest"

    def _client_headers(self) -> dict[str, str]:
        return {**self._headers, **tenant_headers()}

    def _encode(self, payload: dict[str, Any]) -> bytes:
        typ, data = self.serde.dumps_typed(payload)
        tb = typ.encode("utf-8")
        if len(tb) > 255:
            raise ValueError("serde type name too long for proxy envelope")
        return bytes([len(tb)]) + tb + data

    def _decode(self, raw: bytes) -> dict[str, Any]:
        if not raw:
            raise ValueError("empty checkpoint blob")
        n = raw[0]
        typ = raw[1 : 1 + n].decode("utf-8")
        return self.serde.loads_typed((typ, raw[1 + n :]))

    async def _put_cas(
        self,
        client: httpx.AsyncClient,
        thread_id: str,
        key: str,
        body: bytes,
        headers: dict[str, str],
        *,
        etag: str | None,
        create_only: bool = False,
    ) -> None:
        put_headers = {**headers, "Content-Type": "application/octet-stream"}
        if create_only:
            put_headers["If-None-Match"] = "*"
        elif etag:
            put_headers["If-Match"] = etag
        put = await client.put(
            self._url(thread_id, key), content=body, headers=put_headers
        )
        if put.status_code == 412:
            raise _CASConflict()
        put.raise_for_status()

    def _tuple_from_blob(
        self, config: RunnableConfig, key: str, blob: dict[str, Any]
    ) -> CheckpointTuple:
        ns, cid = _parse_blob_key(key)
        cfg_thread = config["configurable"]["thread_id"]
        parent_id = blob.get("parent_checkpoint_id")
        parent_config = None
        if parent_id:
            parent_config = {
                "configurable": {
                    "thread_id": cfg_thread,
                    "checkpoint_ns": ns,
                    "checkpoint_id": parent_id,
                }
            }
        pending_writes = []
        for item in blob.get("writes") or []:
            task_id, channel, value = item[0], item[1], item[2]
            pending_writes.append(
                (task_id, channel, self.serde.loads_typed(_as_typed(value)))
            )
        return CheckpointTuple(
            {
                "configurable": {
                    "thread_id": cfg_thread,
                    "checkpoint_ns": ns,
                    "checkpoint_id": cid,
                }
            },
            self.serde.loads_typed(_as_typed(blob["checkpoint"])),
            self.serde.loads_typed(_as_typed(blob["metadata"])),
            parent_config,
            pending_writes,
        )

    async def aget_tuple(self, config: RunnableConfig) -> CheckpointTuple | None:
        cfg_thread: str = config["configurable"]["thread_id"]
        thread_id = storage_thread_id(cfg_thread)
        ns = config["configurable"].get("checkpoint_ns", "")
        checkpoint_id = get_checkpoint_id(config)
        headers = self._client_headers()
        client = await self._http()

        if checkpoint_id:
            key = _blob_key(ns, checkpoint_id)
            resp = await client.get(self._url(thread_id, key), headers=headers)
            if resp.status_code == 404:
                return None
            resp.raise_for_status()
            return self._tuple_from_blob(config, key, self._decode(resp.content))

        resp = await client.get(
            self._latest_url(thread_id),
            headers=headers,
            params={"ns": ns},
        )
        if resp.status_code == 404:
            return None
        resp.raise_for_status()
        key = unquote(resp.headers.get("X-Runkite-Checkpoint-Id") or "")
        if not key:
            return await self._aget_latest_via_list(
                config, thread_id, ns, headers, client
            )
        return self._tuple_from_blob(config, key, self._decode(resp.content))

    async def _aget_latest_via_list(
        self,
        config: RunnableConfig,
        thread_id: str,
        ns: str,
        headers: dict[str, str],
        client: httpx.AsyncClient,
    ) -> CheckpointTuple | None:
        resp = await client.get(
            self._url(thread_id),
            headers=headers,
            params={"limit": _LIST_FETCH_LIMIT},
        )
        resp.raise_for_status()
        for item in resp.json() or []:
            key = item["checkpoint_id"]
            item_ns, _ = _parse_blob_key(key)
            if item_ns != ns:
                continue
            get = await client.get(self._url(thread_id, key), headers=headers)
            if get.status_code == 404:
                continue
            get.raise_for_status()
            return self._tuple_from_blob(config, key, self._decode(get.content))
        return None

    async def alist(
        self,
        config: RunnableConfig | None,
        *,
        filter: dict[str, Any] | None = None,
        before: RunnableConfig | None = None,
        limit: int | None = None,
    ) -> AsyncIterator[CheckpointTuple]:
        if filter:
            raise NotImplementedError(
                "ProxyCheckpointSaver.alist(filter=...) is not supported; "
                "omit filter or use POSTGRES_DSN direct mode"
            )
        if config is None:
            return
            yield  # pragma: no cover -- make this an async generator
        cfg_thread: str = config["configurable"]["thread_id"]
        thread_id = storage_thread_id(cfg_thread)
        ns = config["configurable"].get("checkpoint_ns", "")
        headers = self._client_headers()
        lim = limit or 10
        before_id = get_checkpoint_id(before) if before else None
        fetch = max(lim * 4, _LIST_FETCH_LIMIT)

        client = await self._http()
        resp = await client.get(
            self._url(thread_id),
            headers=headers,
            params={"limit": fetch},
        )
        resp.raise_for_status()
        n = 0
        skipping = before_id is not None
        for item in resp.json() or []:
            key = item["checkpoint_id"]
            item_ns, item_id = _parse_blob_key(key)
            if item_ns != ns:
                continue
            if skipping:
                if item_id == before_id:
                    skipping = False
                continue
            get = await client.get(self._url(thread_id, key), headers=headers)
            if get.status_code == 404:
                continue
            get.raise_for_status()
            yield self._tuple_from_blob(config, key, self._decode(get.content))
            n += 1
            if n >= lim:
                return

    async def aput(
        self,
        config: RunnableConfig,
        checkpoint: Checkpoint,
        metadata: CheckpointMetadata,
        new_versions: ChannelVersions,
    ) -> RunnableConfig:
        del new_versions
        cfg_thread: str = config["configurable"]["thread_id"]
        thread_id = storage_thread_id(cfg_thread)
        ns = config["configurable"].get("checkpoint_ns", "")
        checkpoint_id = checkpoint["id"]
        key = _blob_key(ns, checkpoint_id)
        parent_id = config["configurable"].get("checkpoint_id")
        headers = self._client_headers()
        lock = await self._lock_for(thread_id, key)

        async with lock:
            client = await self._http()
            for attempt in range(_CAS_MAX_ATTEMPTS):
                existing_writes: list = []
                etag: str | None = None
                create_only = False
                prev = await client.get(self._url(thread_id, key), headers=headers)
                if prev.status_code == 200:
                    etag = prev.headers.get("ETag")
                    try:
                        existing_writes = self._decode(prev.content).get("writes") or []
                    except Exception:
                        existing_writes = []
                elif prev.status_code == 404:
                    # Same Pregel race as aput_writes: a peer may create a
                    # writes shell first. Create-only so we never wipe it
                    # with an unconditional PUT of writes=[].
                    create_only = True
                else:
                    prev.raise_for_status()

                blob = {
                    "v": 1,
                    "checkpoint": self.serde.dumps_typed(checkpoint),
                    "metadata": self.serde.dumps_typed(metadata),
                    "parent_checkpoint_id": parent_id,
                    "writes": existing_writes,
                }
                try:
                    await self._put_cas(
                        client,
                        thread_id,
                        key,
                        self._encode(blob),
                        headers,
                        etag=etag,
                        create_only=create_only,
                    )
                    break
                except _CASConflict:
                    if attempt == _CAS_MAX_ATTEMPTS - 1:
                        raise RuntimeError(
                            f"checkpoint CAS failed after {_CAS_MAX_ATTEMPTS} attempts "
                            f"for {thread_id}/{key}"
                        ) from None
                    continue

        return {
            "configurable": {
                "thread_id": cfg_thread,
                "checkpoint_ns": ns,
                "checkpoint_id": checkpoint_id,
            }
        }

    async def aput_writes(
        self,
        config: RunnableConfig,
        writes: Sequence[tuple[str, Any]],
        task_id: str,
        task_path: str = "",
    ) -> None:
        del task_path
        cfg_thread: str = config["configurable"]["thread_id"]
        thread_id = storage_thread_id(cfg_thread)
        ns = config["configurable"].get("checkpoint_ns", "")
        checkpoint_id = get_checkpoint_id(config)
        if not checkpoint_id:
            # Fail loud: a silent no-op would drop mid-superstep channel
            # writes and make HITL / crash resume look "fine" while state
            # is incomplete. LangGraph always supplies checkpoint_id here.
            raise ValueError(
                "ProxyCheckpointSaver.aput_writes requires "
                "configurable.checkpoint_id"
            )
        key = _blob_key(ns, checkpoint_id)
        headers = self._client_headers()
        lock = await self._lock_for(thread_id, key)

        async with lock:
            client = await self._http()
            for attempt in range(_CAS_MAX_ATTEMPTS):
                etag: str | None = None
                create_only = False
                resp = await client.get(self._url(thread_id, key), headers=headers)
                if resp.status_code == 404:
                    # Normal Pregel order: aput_writes for the *next*
                    # checkpoint_id lands before aput creates that blob.
                    # Create a shell with If-None-Match:* so a concurrent
                    # aput that already wrote cannot be overwritten.
                    blob = {
                        "v": 1,
                        "checkpoint": self.serde.dumps_typed({}),
                        "metadata": self.serde.dumps_typed({}),
                        "parent_checkpoint_id": None,
                        "writes": [],
                    }
                    create_only = True
                else:
                    resp.raise_for_status()
                    etag = resp.headers.get("ETag")
                    blob = self._decode(resp.content)

                encoded = list(blob.get("writes") or [])
                for channel, value in writes:
                    encoded.append([task_id, channel, self.serde.dumps_typed(value)])
                blob["writes"] = encoded
                try:
                    await self._put_cas(
                        client,
                        thread_id,
                        key,
                        self._encode(blob),
                        headers,
                        etag=etag,
                        create_only=create_only,
                    )
                    return
                except _CASConflict:
                    if attempt == _CAS_MAX_ATTEMPTS - 1:
                        raise RuntimeError(
                            f"checkpoint writes CAS failed after {_CAS_MAX_ATTEMPTS} "
                            f"attempts for {thread_id}/{key}"
                        ) from None

    def get_tuple(self, config: RunnableConfig) -> CheckpointTuple | None:
        return _run(self.aget_tuple(config))

    def list(
        self,
        config: RunnableConfig | None,
        *,
        filter: dict[str, Any] | None = None,
        before: RunnableConfig | None = None,
        limit: int | None = None,
    ) -> Iterator[CheckpointTuple]:
        async def _collect():
            out = []
            async for item in self.alist(
                config, filter=filter, before=before, limit=limit
            ):
                out.append(item)
            return out

        return iter(_run(_collect()))

    def put(
        self,
        config: RunnableConfig,
        checkpoint: Checkpoint,
        metadata: CheckpointMetadata,
        new_versions: ChannelVersions,
    ) -> RunnableConfig:
        return _run(self.aput(config, checkpoint, metadata, new_versions))

    def put_writes(
        self,
        config: RunnableConfig,
        writes: Sequence[tuple[str, Any]],
        task_id: str,
        task_path: str = "",
    ) -> None:
        _run(self.aput_writes(config, writes, task_id, task_path))
