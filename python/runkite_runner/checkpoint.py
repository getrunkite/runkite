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

Concurrent-startup migration race (found live): `AsyncPostgresSaver.setup()` runs `CREATE TABLE IF NOT EXISTS` DDL
for its own `checkpoint_migrations` table, which is not actually race-free
on a truly fresh database -- the same class of bug this project's own
`internal/state/postgres/postgres.go` had and fixed with a session advisory
lock. When 2+ runner replicas start simultaneously against a fresh
Postgres, one `setup()` call can lose that race and crash with
`duplicate key value violates unique constraint "pg_type_typname_nsp_index"`.

Fixed here with an advisory lock too, but `pg_try_advisory_lock` polled in
a loop, NOT a blocking `pg_advisory_lock` -- tried the blocking version
first and hit a real deadlock, not just a slower path: `setup()` also runs
`CREATE INDEX CONCURRENTLY`, which must wait for every *other* backend's
in-flight statement in the whole database to finish before it can proceed
(a Postgres-wide barrier, unrelated to which table the other statement
touches). A losing replica blocked inside a single `SELECT
pg_advisory_lock(...)` call counts as an in-flight statement -- so the
winner's `CREATE INDEX CONCURRENTLY` waits on the losers, and the losers
wait on the winner to finish `setup()` and unlock. Circular wait,
confirmed live via `pg_stat_activity`/`pg_locks` (losers stuck on
`Lock/advisory`, winner stuck on `Lock/virtualxid` waiting on them).
Polling `pg_try_advisory_lock` avoids this because each poll is its own
complete, instantly-committed statement -- a losing replica is never
mid-statement between polls, so it never blocks the winner's
`CREATE INDEX CONCURRENTLY`.
"""

import asyncio
import logging

logger = logging.getLogger("runkite.runner")

# Distinct from the Go control plane's own schema-init advisory lock key
# (894127001, internal/state/postgres/postgres.go) -- unrelated schemas
# (runner checkpoint tables vs. control-plane state tables), no reason for
# one to block the other.
_CHECKPOINT_SETUP_ADVISORY_LOCK_KEY = 894127002
_LOCK_POLL_INTERVAL_S = 0.2
_LOCK_POLL_TIMEOUT_S = 60.0


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
            import psycopg
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

            # Serialize setup() across concurrently-starting runner replicas so
            # its CREATE TABLE IF NOT EXISTS / CREATE INDEX CONCURRENTLY DDL
            # can't race on a fresh DB (see module docstring). Deliberately a
            # standalone connection, NOT checked out from self._pool: setup()
            # itself checks out a connection from that same pool internally,
            # and with --concurrency 1 (pool max_size=1) holding the lock from
            # inside the pool would starve setup()'s own checkout of the only
            # connection available.
            async with await psycopg.AsyncConnection.connect(postgres_dsn, autocommit=True) as lock_conn:
                waited = 0.0
                while True:
                    row = await (
                        await lock_conn.execute(
                            "SELECT pg_try_advisory_lock(%s)", (_CHECKPOINT_SETUP_ADVISORY_LOCK_KEY,)
                        )
                    ).fetchone()
                    if row and row[0]:
                        break
                    if waited >= _LOCK_POLL_TIMEOUT_S:
                        raise TimeoutError(
                            "timed out waiting for checkpoint setup() advisory lock "
                            f"(held by another runner replica for over {_LOCK_POLL_TIMEOUT_S}s)"
                        )
                    await asyncio.sleep(_LOCK_POLL_INTERVAL_S)
                    waited += _LOCK_POLL_INTERVAL_S
                try:
                    await self._checkpointer.setup()
                finally:
                    await lock_conn.execute("SELECT pg_advisory_unlock(%s)", (_CHECKPOINT_SETUP_ADVISORY_LOCK_KEY,))
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
