"""Self-check that RunkiteStore's direct-mode AsyncConnectionPool actually
serves concurrent ops without serializing on a single connection.

The runner-side concurrency work replaced a shared connection + Lock with
psycopg_pool; without this check, a silent regression back to one
connection would still pass every functional store test.
"""

from __future__ import annotations

import asyncio
import os
import sys
import uuid

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner.store import RunkiteStore  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


async def test_pool_serves_concurrent_ops(postgres_dsn: str):
    store = RunkiteStore(postgres_dsn=postgres_dsn, pool_size=4)
    ns = ("pool-test", uuid.uuid4().hex[:8])

    # Warm the pool once so open() cost isn't inside the gather.
    await store.aput(ns, "warm", {"v": 0})

    async def one(i: int):
        key = f"k{i}"
        await store.aput(ns, key, {"v": i})
        item = await store.aget(ns, key)
        return item is not None and item.value == {"v": i}

    results = await asyncio.gather(*[one(i) for i in range(8)])
    check("all 8 concurrent put/get pairs succeeded", all(results))
    check("pool was opened (not still None)", store._pool is not None)
    check("pool max_size matches configured pool_size", store._pool.max_size == 4)

    for i in range(8):
        await store.adelete(ns, f"k{i}")
    await store.adelete(ns, "warm")
    await store.aclose()


async def main():
    dsn = os.environ.get("POSTGRES_DSN")
    if not dsn:
        print("Skipping (set POSTGRES_DSN to run against a live Postgres).")
        return
    await test_pool_serves_concurrent_ops(dsn)
    print("\nAll checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
