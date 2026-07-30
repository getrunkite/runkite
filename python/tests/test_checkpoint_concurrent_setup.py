"""Self-check for the concurrent-startup checkpoint migration race:
with 2+ runner replicas starting
simultaneously against a fresh Postgres, AsyncPostgresSaver.setup()'s own
CREATE TABLE IF NOT EXISTS DDL is not race-free, and used to crash one of
them with "duplicate key value violates unique constraint
pg_type_typname_nsp_index" on checkpoint_migrations.

Drops the checkpoint tables to force a truly fresh setup(), then races
several CheckpointerManager.start() calls concurrently -- without the
advisory-lock fix in checkpoint.py, this reliably reproduces the crash;
with it, all replicas succeed.
"""

from __future__ import annotations

import asyncio
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner.checkpoint import CheckpointerManager  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


async def _drop_checkpoint_tables(dsn: str) -> None:
    import psycopg

    async with await psycopg.AsyncConnection.connect(dsn, autocommit=True) as conn:
        for table in ("checkpoint_writes", "checkpoint_blobs", "checkpoints", "checkpoint_migrations"):
            await conn.execute(f"DROP TABLE IF EXISTS {table} CASCADE")


async def test_concurrent_setup_on_fresh_database(dsn: str, concurrency: int = 5):
    await _drop_checkpoint_tables(dsn)

    managers = [CheckpointerManager() for _ in range(concurrency)]
    results = await asyncio.gather(
        *[m.start(dsn, pool_size=1) for m in managers], return_exceptions=True
    )

    errors = [r for r in results if isinstance(r, Exception)]
    if errors:
        print(f"  first error: {errors[0]!r}")
    check(f"all {concurrency} concurrent start() calls succeeded (no migration race)", not errors)
    check("every manager landed in direct-postgres mode", all(m.mode == "direct-postgres" for m in managers))

    await asyncio.gather(*[m.stop() for m in managers])


async def main():
    dsn = os.environ.get("POSTGRES_DSN")
    if not dsn:
        print("Skipping (set POSTGRES_DSN to run against a live Postgres).")
        return
    await test_concurrent_setup_on_fresh_database(dsn)
    print("\nAll checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
