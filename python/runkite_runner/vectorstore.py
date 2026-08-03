"""Vector Store Dual Mode for the Python runner.

Mirrors store.py's dual-mode pattern for the control plane's semantic
store:

- proxy mode (http_base_url set): calls the control plane's /vectors/*
  HTTP API over httpx. Works for every VectorStore backend (pgvector,
  Qdrant, …) because the CP owns the backend choice.
- direct mode (postgres_dsn set, http_base_url unset): queries the
  control plane's own `vector_items` table straight over psycopg --
  same schema as internal/vectorstore/pgvector/pgvector.go. Zero HTTP
  hop. Only correct when the CP is actually on pgvector; always
  operates in the "default" tenant (see _TENANT_ID below), the same
  documented Direct Mode Trust Model trade-off as store.py.

When BOTH are provided, proxy wins. A common production shape is
Postgres for state/checkpoints (POSTGRES_DSN set on the runner) plus
Qdrant for vectors -- preferring DSN just because it exists would
silently write to a table the control plane never reads. Pass only
postgres_dsn (no http_base_url) to opt into direct mode deliberately.

Implements LangChain's `VectorStore` interface (`add_texts`,
`similarity_search`, `similarity_search_by_vector`,
`similarity_search_with_score`, `from_texts`) so it drops into existing
LangChain/LangGraph RAG code unchanged.
"""

from __future__ import annotations

import asyncio
import json
import uuid
from collections.abc import Iterable
from typing import Any

import httpx
from langchain_core.documents import Document
from langchain_core.embeddings import Embeddings
from langchain_core.vectorstores import VectorStore

from . import pg_pool, runner_loop
from .tls_utils import httpx_tls_kwargs

# Direct mode has no per-request tenant identity (a raw DB connection, not
# an authenticated HTTP call) -- see store.py's identical note. Must match
# internal/tenant.DefaultTenant on the Go side exactly.
_TENANT_ID = "default"


def _item_to_document(item: dict) -> Document:
    return Document(page_content=item.get("content") or "", metadata=item.get("metadata") or {}, id=item.get("id"))


class RunkiteVectorStore(VectorStore):
    """LangChain VectorStore backed by the Runkite control plane's vector store."""

    def __init__(
        self,
        embedding: Embeddings,
        *,
        namespace: str,
        postgres_dsn: str | None = None,
        http_base_url: str | None = None,
        runner_token: str | None = None,
        pool_size: int = 4,
    ):
        if not postgres_dsn and not http_base_url:
            raise ValueError("RunkiteVectorStore requires postgres_dsn or http_base_url")
        self._embedding = embedding
        self._namespace = namespace
        self._headers: dict[str, str] = {}
        if runner_token:
            self._headers["X-Runner-Kind"] = "python-langgraph"
            self._headers["X-Runner-Token"] = runner_token
        # Proxy wins when both are set -- see module docstring. Direct
        # mode only with an explicit DSN and no HTTP base URL.
        if http_base_url:
            self.mode = "proxy"
            self._base_url = http_base_url.rstrip("/")
            self._dsn = None
        else:
            self.mode = "direct"
            self._base_url = None
            self._dsn = postgres_dsn
        # Same pooling rationale and upgrade path as store.py: a single
        # shared connection serialized by a lock is correct under
        # concurrent runs but not actually parallel. Pool creation itself
        # (not every op) is guarded by _pool_init_lock.
        self._pool_size = pool_size
        self._pool = None
        self._loop: asyncio.AbstractEventLoop | None = None
        self._pool_init_lock = asyncio.Lock()

    @property
    def embeddings(self) -> Embeddings:
        return self._embedding

    async def _get_pool(self):
        if self._pool is None:
            async with self._pool_init_lock:
                if self._pool is None:
                    pool = pg_pool.make(self._dsn, max_size=self._pool_size)
                    await pool.open()
                    self._pool = pool
                    self._loop = asyncio.get_running_loop()
        if self._loop is None:
            self._loop = asyncio.get_running_loop()
        return self._pool

    async def _recreate_pool(self) -> None:
        async with self._pool_init_lock:
            old = self._pool
            self._pool = None
            if old is not None:
                try:
                    await old.close()
                except Exception:
                    pass
        await self._get_pool()

    async def warm(self) -> None:
        """Bind to the current event loop and open the direct-mode pool.

        Same contract as RunkiteStore.warm: run on the runner's main loop
        before sync methods can be called from asyncio.to_thread. Worker
        startup already binds the loop via store.warm() → runner_loop;
        call this on a direct-mode instance when running outside that path
        (tests, standalone scripts).
        """
        self._loop = asyncio.get_running_loop()
        runner_loop.bind(self._loop)
        if self.mode == "direct":
            await self._get_pool()

    async def aclose(self) -> None:
        if self._pool is not None:
            await self._pool.close()
            self._pool = None

    # -- sync entrypoints (LangChain's default interface) -------------------
    # Every sync method below hops to an event loop the same way
    # RunkiteStore.batch does: langgraph tool/node code typically runs in a
    # worker thread with no running loop, so asyncio.run is fine; if one is
    # somehow already running, fall back to a fresh thread rather than
    # raising "asyncio.run() cannot be called from a running event loop".

    def _run(self, factory):
        # Same cross-loop rule as RunkiteStore.batch: if the async pool
        # lives on a known running loop, schedule onto it instead of
        # asyncio.run() on a fresh loop (which times out on getconn).
        # Prefer this instance's loop, then the runner-wide loop published
        # by store.warm() / vectorstore.warm(). factory is a zero-arg
        # callable that builds the coroutine so the fail-fast path never
        # leaves an un-awaited coro behind.
        def _submit():
            loop = self._loop if self._loop is not None else runner_loop.get()
            if loop is not None and loop.is_running():
                return asyncio.run_coroutine_threadsafe(factory(), loop).result(timeout=120)
            if self.mode == "direct":
                raise RuntimeError(
                    "RunkiteVectorStore sync API used before warm() on the runner "
                    "event loop (direct-mode pool must not be opened via asyncio.run "
                    "in a worker thread)"
                )
            return asyncio.run(factory())

        try:
            asyncio.get_running_loop()
        except RuntimeError:
            return _submit()
        import concurrent.futures

        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
            return pool.submit(_submit).result(timeout=120)

    def add_documents(self, documents: list[Document], **kwargs: Any) -> list[str]:
        return self._run(lambda: self.aadd_documents(documents, **kwargs))

    def similarity_search(self, query: str, k: int = 4, **kwargs: Any) -> list[Document]:
        return self._run(lambda: self.asimilarity_search(query, k, **kwargs))

    def similarity_search_by_vector(self, embedding: list[float], k: int = 4, **kwargs: Any) -> list[Document]:
        return self._run(lambda: self.asimilarity_search_by_vector(embedding, k, **kwargs))

    def similarity_search_with_score(self, query: str, k: int = 4, **kwargs: Any) -> list[tuple[Document, float]]:
        return self._run(lambda: self.asimilarity_search_with_score(query, k, **kwargs))

    def delete(self, ids: list[str] | None = None, **kwargs: Any) -> bool | None:
        return self._run(lambda: self.adelete(ids, **kwargs))

    @classmethod
    def from_texts(
        cls,
        texts: list[str],
        embedding: Embeddings,
        metadatas: list[dict] | None = None,
        *,
        ids: list[str] | None = None,
        **kwargs: Any,
    ) -> RunkiteVectorStore:
        store = cls(embedding, **kwargs)
        store.add_texts(texts, metadatas, ids=ids)
        return store

    # -- async implementations -----------------------------------------------

    async def aadd_documents(self, documents: list[Document], **kwargs: Any) -> list[str]:
        texts = [d.page_content for d in documents]
        metadatas = [d.metadata for d in documents]
        ids = kwargs.get("ids") or [d.id or str(uuid.uuid4()) for d in documents]
        vectors = self._embedding.embed_documents(texts)
        for doc_id, text, meta, vec in zip(ids, texts, metadatas, vectors, strict=True):
            await self._upsert(doc_id, text, meta, vec)
        return ids

    async def aadd_texts(
        self,
        texts: Iterable[str],
        metadatas: list[dict] | None = None,
        *,
        ids: list[str] | None = None,
        **kwargs: Any,
    ) -> list[str]:
        texts = list(texts)
        docs = [
            Document(page_content=t, metadata=(metadatas[i] if metadatas else {}), id=(ids[i] if ids else None))
            for i, t in enumerate(texts)
        ]
        return await self.aadd_documents(docs, **kwargs)

    async def asimilarity_search(self, query: str, k: int = 4, **kwargs: Any) -> list[Document]:
        embedding = self._embedding.embed_query(query)
        return await self.asimilarity_search_by_vector(embedding, k, **kwargs)

    async def asimilarity_search_by_vector(self, embedding: list[float], k: int = 4, **kwargs: Any) -> list[Document]:
        results = await self._search(embedding, k, kwargs.get("filter"))
        return [_item_to_document(r["item"]) for r in results]

    async def asimilarity_search_with_score(
        self, query: str, k: int = 4, **kwargs: Any
    ) -> list[tuple[Document, float]]:
        embedding = self._embedding.embed_query(query)
        results = await self._search(embedding, k, kwargs.get("filter"))
        return [(_item_to_document(r["item"]), r["score"]) for r in results]

    async def adelete(self, ids: list[str] | None = None, **kwargs: Any) -> bool | None:
        if not ids:
            return None
        for doc_id in ids:
            if self.mode == "direct":
                await self._delete_direct(doc_id)
            else:
                await self._delete_proxy(doc_id)
        return True

    # -- proxy mode: HTTP calls to the control plane -------------------------

    async def _upsert(self, doc_id: str, content: str, metadata: dict, embedding: list[float]) -> None:
        if self.mode == "direct":
            await self._upsert_direct(doc_id, content, metadata, embedding)
        else:
            await self._upsert_proxy(doc_id, content, metadata, embedding)

    async def _search(self, embedding: list[float], k: int, filter_: dict | None) -> list[dict]:
        if self.mode == "direct":
            return await self._search_direct(embedding, k, filter_)
        return await self._search_proxy(embedding, k, filter_)

    async def _upsert_proxy(self, doc_id: str, content: str, metadata: dict, embedding: list[float]) -> None:
        async with httpx.AsyncClient(
            base_url=self._base_url, headers=self._headers, timeout=10.0, **httpx_tls_kwargs()
        ) as client:
            resp = await client.put(
                "/internal/vectors/items",
                json={
                    "namespace": self._namespace,
                    "id": doc_id,
                    "content": content,
                    "metadata": metadata,
                    "embedding": embedding,
                },
            )
            resp.raise_for_status()

    async def _delete_proxy(self, doc_id: str) -> None:
        async with httpx.AsyncClient(
            base_url=self._base_url, headers=self._headers, timeout=10.0, **httpx_tls_kwargs()
        ) as client:
            resp = await client.request(
                "DELETE", "/internal/vectors/items", json={"namespace": self._namespace, "id": doc_id}
            )
            resp.raise_for_status()

    async def _search_proxy(self, embedding: list[float], k: int, filter_: dict | None) -> list[dict]:
        async with httpx.AsyncClient(
            base_url=self._base_url, headers=self._headers, timeout=10.0, **httpx_tls_kwargs()
        ) as client:
            resp = await client.post(
                "/internal/vectors/search",
                json={"namespace": self._namespace, "embedding": embedding, "top_k": k, "filter": filter_ or {}},
            )
            resp.raise_for_status()
            return resp.json().get("results") or []

    # -- direct mode: psycopg straight to vector_items -----------------------

    async def _with_direct_conn(self, fn):
        """Run fn(conn) with one PoolTimeout → recreate-pool retry."""
        from psycopg_pool import PoolTimeout

        try:
            pool = await self._get_pool()
            async with pool.connection() as conn:
                return await fn(conn)
        except PoolTimeout:
            await self._recreate_pool()
            pool = await self._get_pool()
            async with pool.connection() as conn:
                return await fn(conn)

    async def _upsert_direct(self, doc_id: str, content: str, metadata: dict, embedding: list[float]) -> None:
        async def _do(conn):
            async with conn.cursor() as cur:
                await cur.execute(
                    """
                    INSERT INTO vector_items (tenant_id, namespace, id, content, embedding, metadata, created_at, updated_at)
                    VALUES (%s, %s, %s, %s, %s::vector, %s, NOW(), NOW())
                    ON CONFLICT (tenant_id, namespace, id) DO UPDATE SET
                        content = EXCLUDED.content, embedding = EXCLUDED.embedding,
                        metadata = EXCLUDED.metadata, updated_at = NOW()
                    """,
                    (_TENANT_ID, self._namespace, doc_id, content, _vector_literal(embedding), json.dumps(metadata)),
                )

        await self._with_direct_conn(_do)

    async def _delete_direct(self, doc_id: str) -> None:
        async def _do(conn):
            async with conn.cursor() as cur:
                await cur.execute(
                    "DELETE FROM vector_items WHERE tenant_id = %s AND namespace = %s AND id = %s",
                    (_TENANT_ID, self._namespace, doc_id),
                )

        await self._with_direct_conn(_do)

    async def _search_direct(self, embedding: list[float], k: int, filter_: dict | None) -> list[dict]:
        where = ["tenant_id = %s", "namespace = %s"]
        args: list[Any] = [_TENANT_ID, self._namespace]
        for key, val in (filter_ or {}).items():
            where.append("metadata->>%s = %s")
            args.append(key)
            args.append(val if isinstance(val, str) else json.dumps(val))
        query = (
            "SELECT id, content, metadata, 1 - (embedding <=> %s::vector) AS score FROM vector_items "
            f"WHERE {' AND '.join(where)} ORDER BY embedding <=> %s::vector LIMIT %s"
        )
        vec = _vector_literal(embedding)

        async def _do(conn):
            async with conn.cursor() as cur:
                await cur.execute(query, [vec, *args, vec, k])
                return await cur.fetchall()

        rows = await self._with_direct_conn(_do)
        results = []
        for doc_id, content, metadata, score in rows:
            meta = metadata if isinstance(metadata, dict) else json.loads(metadata or "{}")
            results.append({"item": {"id": doc_id, "content": content, "metadata": meta}, "score": score})
        return results


def _vector_literal(embedding: list[float]) -> str:
    """pgvector accepts its `vector` type as a plain text literal cast
    (`'[1,2,3]'::vector`) -- psycopg sends this as an ordinary string
    parameter with no special adapter needed, avoiding an extra Python
    dependency (the `pgvector` pip package) just for one query direction."""
    return "[" + ",".join(str(x) for x in embedding) + "]"
