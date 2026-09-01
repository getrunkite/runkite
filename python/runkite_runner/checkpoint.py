"""Checkpoint dual mode.

Direct mode (default when POSTGRES_DSN is set -- production, shared DB with
the control plane): the runner holds its own connection and writes
checkpoints with LangGraph's native AsyncPostgresSaver. Zero added latency,
survives runner restarts. Correct only when the control plane also uses
POSTGRES_DSN against the same database; MySQL/Mongo/SQLite control planes
must unset POSTGRES_DSN on the runner (see README Checkpoint dual mode).

Proxy mode (no POSTGRES_DSN, RUNKITE_HTTP_URL set): opaque blobs via
PUT/GET /internal/checkpoints/* (ProxyCheckpointSaver). Survives runner
restarts against any CP backend without giving the runner DB credentials.

Local mode (no POSTGRES_DSN and no HTTP URL -- zero-dependency fallback):
LangGraph's in-memory MemorySaver. Ephemeral -- state does NOT survive a
runner restart. Blank / whitespace-only HTTP URLs count as "no URL" so a
misconfigured empty RUNKITE_HTTP_URL falls through to MemorySaver with a
clear warning instead of failing on proxy connection errors.

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
import os

logger = logging.getLogger("runkite.runner")

# Distinct from the Go control plane's own schema-init advisory lock key
# (894127001, internal/state/postgres/postgres.go) -- unrelated schemas
# (runner checkpoint tables vs. control-plane state tables), no reason for
# one to block the other.
_CHECKPOINT_SETUP_ADVISORY_LOCK_KEY = 894127002
_LOCK_POLL_INTERVAL_S = 0.2
_LOCK_POLL_TIMEOUT_S = 60.0


def resolve_checkpoint_http_url(http_address: str | None = None) -> str | None:
    """HTTP base for proxy checkpointer, or None → MemorySaver.

    Prefer an explicit RUNKITE_HTTP_URL (including empty → memory). Fall
    back to the worker --http-address only when that env var is unset.
    Blank / whitespace never selects proxy — that path used to turn a
    missing CP into opaque connection errors instead of in-memory mode.
    """
    if "RUNKITE_HTTP_URL" in os.environ:
        return (os.environ.get("RUNKITE_HTTP_URL") or "").strip() or None
    return (http_address or "").strip() or None


class CheckpointerManager:
    """Owns the runner's single shared checkpointer for its whole lifetime.

    One checkpointer instance is created at worker startup and attached to
    every loaded graph (overriding whatever checkpointer, if any, the
    graph module itself compiled with) -- so checkpoint mode is a runner
    concern, not something agent authors need to configure in their own
    graph.py. Agent authors get this for free: zero changes to graph code.
    """

    def __init__(self):
        self._checkpointer = None
        self._pool = None  # AsyncConnectionPool, kept open for the runner's lifetime (postgres mode only)
        self._dsn: str | None = None
        self._pool_size: int = 4
        self._attached: list = []  # graphs whose .checkpointer we own
        self.mode = "none"

    async def start(
        self,
        postgres_dsn: str | None,
        pool_size: int = 4,
        *,
        http_base_url: str | None = None,
        runner_token: str | None = None,
    ):
        # Defense in depth: callers should already pass None for blank, but
        # never let whitespace select ProxyCheckpointSaver by accident.
        http_base_url = (http_base_url or "").strip() or None
        if postgres_dsn:
            import psycopg

            self._dsn = postgres_dsn
            self._pool_size = pool_size
            await self._open_pool()

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
            logger.info(
                "checkpoint mode: direct (postgres, pool_size=%s) -- LangGraph tables on "
                "POSTGRES_DSN; requires the control plane to use the same Postgres database "
                "(Supported profile). If the control plane is MySQL/Mongo/SQLite, unset "
                "POSTGRES_DSN on this runner and set RUNKITE_HTTP_URL for proxy mode.",
                pool_size,
            )
        elif http_base_url:
            from .proxy_checkpoint import ProxyCheckpointSaver

            self._checkpointer = ProxyCheckpointSaver(
                http_base_url=http_base_url,
                runner_token=runner_token,
            )
            self.mode = "proxy-http"
            logger.info(
                "checkpoint mode: proxy (HTTP opaque blobs via %s) -- "
                "persists across runner restarts without POSTGRES_DSN",
                http_base_url.rstrip("/"),
            )
        else:
            from langgraph.checkpoint.memory import MemorySaver

            self._checkpointer = MemorySaver()
            self.mode = "memory"
            logger.warning(
                "checkpoint mode: in-memory (no POSTGRES_DSN and no RUNKITE_HTTP_URL) -- "
                "thread state will NOT survive a runner restart. "
                "Set POSTGRES_DSN (direct) or RUNKITE_HTTP_URL (proxy) for persistence."
            )

    async def _open_pool(self) -> None:
        from langgraph.checkpoint.postgres.aio import AsyncPostgresSaver
        from psycopg.rows import dict_row

        from . import pg_pool

        # Same connection kwargs from_conn_string used
        # (autocommit/prepare_threshold/row_factory) -- AsyncPostgresSaver
        # expects dict-row results, not psycopg's tuple default.
        self._pool = pg_pool.make(
            self._dsn,
            max_size=self._pool_size,
            conn_kwargs={"prepare_threshold": 0, "row_factory": dict_row},
        )
        await self._pool.open()
        self._checkpointer = AsyncPostgresSaver(conn=self._pool)
        for graph in self._attached:
            graph.checkpointer = self._checkpointer

    async def recreate_pool(self) -> None:
        """Drop a wedged pool (idle overnight / laptop sleep) and open a new one.

        Rebinds every previously attach()'d graph so LangGraph does not keep
        calling through the closed pool.
        """
        if self._dsn is None:
            return
        old = self._pool
        self._pool = None
        if old is not None:
            try:
                await old.close()
            except Exception:
                logger.exception("error closing wedged checkpoint pool")
        await self._open_pool()

    async def recover_if_wedged(self) -> None:
        """Cheap pre-job probe: if getconn hangs/fails, recreate once.

        AsyncPostgresSaver owns the pool reference internally, so unlike
        store.abatch we cannot catch PoolTimeout inside each checkpoint
        op -- probe here before astream so an overnight-wedged pool does
        not burn a full run on a 30s timeout mid-graph.
        """
        if self._pool is None:
            return
        from psycopg_pool import PoolTimeout

        try:
            async with self._pool.connection(timeout=5.0) as conn:
                await conn.execute("SELECT 1")
        except PoolTimeout:
            logger.warning("checkpoint pool timed out on health probe; recreating pool")
            await self.recreate_pool()
        except Exception:
            logger.warning("checkpoint pool health probe failed; recreating pool", exc_info=True)
            await self.recreate_pool()

    async def stop(self):
        if self._pool is not None:
            await self._pool.close()
            self._pool = None

    def attach(self, graph):
        """Override a compiled graph's checkpointer with the shared one."""
        if graph not in self._attached:
            self._attached.append(graph)
        graph.checkpointer = self._checkpointer
        return graph
