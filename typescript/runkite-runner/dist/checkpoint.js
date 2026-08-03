import { logger } from "./logger.js";
export class CheckpointerManager {
    checkpointer = null;
    mode = "memory";
    closeFn = null;
    /** poolSize mirrors the Python runner's `pool_size=concurrency` --
     * with --concurrency > 1, multiple jobs' checkpoint reads/writes can
     * be genuinely in flight at once, and a single connection would
     * serialize them regardless of how many jobs the dispatcher allows
     * concurrently. Undefined leaves node-postgres's own default (10),
     * fine for the concurrency=1 common case. PostgresSaver.fromConnString
     * doesn't expose a pool-size option (only `schema`) -- it's a thin
     * wrapper around `new Pool({connectionString}); new
     * PostgresSaver(pool)` (see its own source), so constructing the pool
     * directly and passing it to PostgresSaver's other constructor gets
     * the same behavior with pool-size control added. */
    async start(postgresDsn, poolSize) {
        if (postgresDsn) {
            const { PostgresSaver } = await import("@langchain/langgraph-checkpoint-postgres");
            const { Pool } = await import("pg");
            // idleTimeoutMillis / connectionTimeoutMillis: node-pg's default
            // idle eviction is already short (~10s); pin it explicitly and
            // bound connect so a wedged Postgres cannot hang job startup.
            // Unlike Python's psycopg_pool there is no checkout check_connection
            // hook -- stale sockets surface as query errors and the pool
            // discards them on the next checkout.
            const pool = new Pool({
                connectionString: postgresDsn,
                idleTimeoutMillis: 60_000,
                connectionTimeoutMillis: 10_000,
                ...(poolSize ? { max: poolSize } : {}),
            });
            // node-postgres emits 'error' on idle client backend failures
            // (laptop sleep, server restart). Unhandled, that crashes the
            // process -- same handler store.ts already installs.
            pool.on("error", (err) => {
                logger.error(`checkpoint pool idle client error: ${err.message}`);
            });
            const saver = new PostgresSaver(pool);
            await saver.setup();
            this.checkpointer = saver;
            this.mode = "direct-postgres";
            this.closeFn = async () => {
                await saver.end();
            };
            logger.info("checkpoint mode: direct (postgres) -- LangGraph tables on POSTGRES_DSN; " +
                "requires the control plane to use the same Postgres database (Supported profile). " +
                "If the control plane is MySQL/Mongo/SQLite, unset POSTGRES_DSN on this runner and " +
                "set RUNKITE_HTTP_URL for store proxy mode.");
        }
        else {
            const { MemorySaver } = await import("@langchain/langgraph");
            this.checkpointer = new MemorySaver();
            this.mode = "memory";
            logger.warn("checkpoint mode: in-memory (no POSTGRES_DSN set) -- " +
                "thread state will NOT survive a runner restart. " +
                "Set POSTGRES_DSN for production persistence.");
        }
    }
    async stop() {
        if (this.closeFn)
            await this.closeFn();
    }
    /** Overrides a compiled graph's checkpointer with the shared one -- checkpoint
     * mode is a runner concern, not something agent authors configure in graph.ts. */
    attach(graph) {
        graph.checkpointer = this.checkpointer;
    }
}
