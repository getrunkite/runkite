"""Shared AsyncConnectionPool construction for direct-mode store /
vectorstore / checkpoint.

Enables check_connection so a laptop-sleep or Postgres restart cannot
hand out a half-open TCP socket that then hangs the first query. Callers
that see PoolTimeout should close and rebuild via a fresh make() -- a
closed pool cannot be reopen()'d.
"""

from __future__ import annotations

from typing import Any


def make(dsn: str, *, max_size: int, conn_kwargs: dict[str, Any] | None = None):
    from psycopg_pool import AsyncConnectionPool

    kwargs: dict[str, Any] = {"autocommit": True}
    if conn_kwargs:
        kwargs.update(conn_kwargs)
    return AsyncConnectionPool(
        dsn,
        min_size=1,
        max_size=max(max_size, 1),
        kwargs=kwargs,
        open=False,
        check=AsyncConnectionPool.check_connection,
    )
