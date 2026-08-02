"""Regression: sync store.batch from a worker thread must use the pool's
owning event loop, not asyncio.run() on a fresh loop.

Live SA soak failure: deepagents SkillsMiddleware called store.get via
asyncio.to_thread → batch → asyncio.run(abatch) → PoolTimeout 30s because
AsyncConnectionPool was opened on the runner's main loop.
"""

from __future__ import annotations

import asyncio
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner.store import RunkiteStore  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


async def test_batch_from_to_thread_uses_store_loop():
    store = RunkiteStore(http_base_url="http://127.0.0.1:9")
    await store.warm()
    main_loop = asyncio.get_running_loop()

    seen: list[asyncio.AbstractEventLoop] = []

    async def fake_abatch(ops):
        seen.append(asyncio.get_running_loop())
        return [None for _ in ops]

    store.abatch = fake_abatch  # type: ignore[method-assign]

    result = await asyncio.to_thread(store.batch, [])
    check("batch from to_thread returned", result == [])
    check("abatch ran on the store's main loop", len(seen) == 1 and seen[0] is main_loop)


async def test_direct_batch_before_warm_fails_loudly():
    store = RunkiteStore(postgres_dsn="postgresql://unused", http_base_url=None)
    # Force direct mode even if both were somehow set -- constructor picks
    # direct when postgres_dsn is set.
    check("test store is direct mode", store.mode == "direct")
    try:
        await asyncio.to_thread(store.batch, [])
        check("direct batch before warm raised", False)
    except RuntimeError as e:
        check("direct batch before warm raised", "warm()" in str(e))


async def main():
    await test_batch_from_to_thread_uses_store_loop()
    await test_direct_batch_before_warm_fails_loudly()
    print("\nAll checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
