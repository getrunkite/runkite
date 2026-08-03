"""Regression: after PoolTimeout, RunkiteStore recreates the pool and retries.

Overnight soak / laptop sleep can wedge AsyncConnectionPool workers so every
getconn hits the 30s timeout; a closed pool cannot be reopen()'d.
"""

from __future__ import annotations

import asyncio
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from psycopg_pool import PoolTimeout  # noqa: E402
from runkite_runner.store import RunkiteStore  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


async def test_abatch_recreates_pool_on_timeout():
    store = RunkiteStore(postgres_dsn="postgresql://unused")
    store._loop = asyncio.get_running_loop()

    calls = {"n": 0, "recreated": 0}

    async def fake_once(ops):
        calls["n"] += 1
        if calls["n"] == 1:
            raise PoolTimeout("couldn't get a connection after 30.00 sec")
        return [None for _ in ops]

    async def fake_recreate():
        calls["recreated"] += 1

    store._abatch_direct_once = fake_once  # type: ignore[method-assign]
    store._recreate_pool = fake_recreate  # type: ignore[method-assign]

    result = await store._abatch_direct([])
    check("retried after timeout", calls["n"] == 2)
    check("pool was recreated once", calls["recreated"] == 1)
    check("second attempt result returned", result == [])


async def main():
    await test_abatch_recreates_pool_on_timeout()
    print("\nAll checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
