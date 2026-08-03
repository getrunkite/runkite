"""Regression: CheckpointerManager recreates a wedged pool on health probe."""

from __future__ import annotations

import asyncio
import os
import sys
from types import SimpleNamespace

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from psycopg_pool import PoolTimeout  # noqa: E402
from runkite_runner.checkpoint import CheckpointerManager  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


async def test_recover_recreates_and_rebinds_graphs():
    mgr = CheckpointerManager()
    mgr._dsn = "postgresql://unused"
    mgr._pool_size = 2
    mgr.mode = "direct-postgres"

    class FakePool:
        def connection(self, timeout=None):
            raise PoolTimeout("couldn't get a connection after 5.00 sec")

        async def close(self):
            return None

    mgr._pool = FakePool()
    graph = SimpleNamespace(checkpointer="old")
    mgr._attached = [graph]
    mgr._checkpointer = "old-saver"

    calls = {"n": 0}

    async def fake_open():
        calls["n"] += 1
        mgr._pool = object()
        mgr._checkpointer = "new-saver"
        for g in mgr._attached:
            g.checkpointer = mgr._checkpointer

    mgr._open_pool = fake_open  # type: ignore[method-assign]

    await mgr.recover_if_wedged()
    check("pool reopened", calls["n"] == 1)
    check("graph rebound to new saver", graph.checkpointer == "new-saver")


async def main():
    await test_recover_recreates_and_rebinds_graphs()
    print("\nAll checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
