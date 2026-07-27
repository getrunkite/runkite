"""Checkpoint dual mode (master plan: "Checkpoint Dual Mode").

Direct mode (default when POSTGRES_DSN is set -- production, shared DB with
the control plane): the runner holds its own connection and writes
checkpoints with LangGraph's native AsyncPostgresSaver. Zero added latency,
survives runner restarts, matches what the control plane's own Postgres
state store already uses.

Local mode (no POSTGRES_DSN -- zero-dependency dev default): falls back to
LangGraph's in-memory MemorySaver. This is honestly ephemeral -- state does
NOT survive a runner restart. That's an accepted trade-off for the
zero-dependency default (same spirit as the control plane's own in-process
transport), not a hidden gap. Proxy mode (opaque-blob checkpoints via the
control plane's HTTP API, for non-Python runners or runners without DB
credentials) is not implemented by this Python runner -- it always has
direct DB access when Postgres is available, so proxy mode has no benefit
here; it exists in the protocol for other-language runners.

Connection pooling (runner-side concurrency): a runner process can now
process multiple jobs at once (see worker.py's --concurrency), so `start`
takes a `pool_size` and builds an `AsyncConnectionPool` instead of the
single connection `AsyncPostgresSaver.from_conn_string` opens -- otherwise
every concurrent job's checkpoint I/O would serialize on that one
connection's internal lock (correct, but not actually parallel).
`AsyncPostgresSaver.__init__`'s `conn` parameter accepts a pool directly
(`Conn = AsyncConnection | AsyncConnectionPool` in langgraph's own
`checkpoint/postgres/_ainternal.py`) -- confirmed it checks out a
connection per operation via that same module's `get_connection` helper,
so this is a supported usage, not a hack.
"""

import logging

logger = logging.getLogger("runkite.runner")


class CheckpointerManager:
    """Owns the runner's single shared checkpointer for its whole lifetime.

    One checkpointer instance is created at worker startup and attached to
    every loaded graph (overriding whatever checkpointer, if any, the
    graph module itself compiled with) -- so checkpoint mode is a runner
    concern, not something agent authors need to configure in their own
    graph.py. This matches the master plan's "zero changes to graph code"
    principle.
    """

    def __init__(self):
        self._checkpointer = None
        self._pool = None  # AsyncConnectionPool, kept open for the runner's lifetime (postgres mode only)
        self.mode = "none"

    async def start(self, postgres_dsn: str | None, pool_size: int = 4):
        if postgres_dsn:
            from langgraph.checkpoint.postgres.aio import AsyncPostgresSaver
            from psycopg.rows import dict_row
            from psycopg_pool import AsyncConnectionPool

            # Same connection kwargs from_conn_string used
            # (autocommit/prepare_threshold/row_factory) -- AsyncPostgresSaver
            # expects dict-row results, not psycopg's tuple default.
            self._pool = AsyncConnectionPool(
                postgres_dsn,
                min_size=1,
                max_size=max(pool_size, 1),
                kwargs={"autocommit": True, "prepare_threshold": 0, "row_factory": dict_row},
                open=False,
            )
            await self._pool.open()
            self._checkpointer = AsyncPostgresSaver(conn=self._pool)
            await self._checkpointer.setup()
            self.mode = "direct-postgres"
            logger.info(f"checkpoint mode: direct (postgres, pool_size={pool_size}) -- state survives runner restarts")
        else:
            from langgraph.checkpoint.memory import MemorySaver

            self._checkpointer = MemorySaver()
            self.mode = "memory"
            logger.warning(
                "checkpoint mode: in-memory (no POSTGRES_DSN set) -- "
                "thread state will NOT survive a runner restart. "
                "Set POSTGRES_DSN for production persistence."
            )

    async def stop(self):
        if self._pool is not None:
            await self._pool.close()

    def attach(self, graph):
        """Override a compiled graph's checkpointer with the shared one."""
        graph.checkpointer = self._checkpointer
        return graph
