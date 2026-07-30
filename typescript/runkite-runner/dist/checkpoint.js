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
            const pool = new Pool({ connectionString: postgresDsn, ...(poolSize ? { max: poolSize } : {}) });
            const saver = new PostgresSaver(pool);
            await saver.setup();
            this.checkpointer = saver;
            this.mode = "direct-postgres";
            this.closeFn = async () => {
                await saver.end();
            };
            logger.info("checkpoint mode: direct (postgres) -- state survives runner restarts");
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
