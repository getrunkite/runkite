"""Regression: sync RunkiteVectorStore APIs from a worker thread must use
the pool's owning event loop — same bug class as store.batch PoolTimeout.
"""

from __future__ import annotations

import asyncio
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from langchain_core.embeddings import Embeddings  # noqa: E402
from runkite_runner import runner_loop  # noqa: E402
from runkite_runner.store import RunkiteStore  # noqa: E402
from runkite_runner.vectorstore import RunkiteVectorStore  # noqa: E402


class _NoopEmbeddings(Embeddings):
    def embed_documents(self, texts):
        return [[0.0] * 8 for _ in texts]

    def embed_query(self, text):
        return [0.0] * 8


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


async def test_sync_from_to_thread_uses_runner_loop():
    # Mimic worker startup: store.warm publishes the main loop; a graph
    # then constructs a VectorStore with no warm() of its own.
    store = RunkiteStore(http_base_url="http://127.0.0.1:9")
    await store.warm()
    main_loop = asyncio.get_running_loop()
    check("runner_loop bound by store.warm", runner_loop.get() is main_loop)

    vs = RunkiteVectorStore(
        _NoopEmbeddings(),
        namespace="cross-loop",
        http_base_url="http://127.0.0.1:9",
    )
    seen: list[asyncio.AbstractEventLoop] = []

    async def fake_asimilarity_search(query, k=4, **kwargs):
        seen.append(asyncio.get_running_loop())
        return []

    vs.asimilarity_search = fake_asimilarity_search  # type: ignore[method-assign]

    result = await asyncio.to_thread(vs.similarity_search, "q")
    check("sync search from to_thread returned", result == [])
    check("coro ran on runner main loop", len(seen) == 1 and seen[0] is main_loop)


async def test_direct_sync_before_warm_fails_loudly():
    runner_loop.bind(None)  # type: ignore[arg-type]
    vs = RunkiteVectorStore(
        _NoopEmbeddings(),
        namespace="cross-loop-direct",
        postgres_dsn="postgresql://unused",
    )
    check("test store is direct mode", vs.mode == "direct")
    try:
        await asyncio.to_thread(vs.similarity_search, "q")
        check("direct sync before warm raised", False)
    except RuntimeError as e:
        check("direct sync before warm raised", "warm()" in str(e))


async def main():
    await test_sync_from_to_thread_uses_runner_loop()
    await test_direct_sync_before_warm_fails_loudly()
    print("\nAll checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
