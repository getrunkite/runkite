/**
 * Checkpoint tri-mode. Direct mirror of the Python runner\'s
 * checkpoint.py -- see that file\'s docstring for the full rationale,
 * repeated briefly here:
 *
 * Direct mode (POSTGRES_DSN set): PostgresSaver from
 * @langchain/langgraph-checkpoint-postgres, LangGraph.js\'s own native
 * Postgres checkpointer. Zero added latency, survives runner restarts.
 * Uses LangGraph\'s own checkpoint tables (not the control plane\'s
 * thread_checkpoints) -- completely separate schema, same as the Python
 * runner\'s AsyncPostgresSaver.
 *
 * Proxy mode (no POSTGRES_DSN, RUNKITE_HTTP_URL set): opaque blobs via
 * PUT/GET /internal/checkpoints/* (ProxyCheckpointSaver). Survives runner
 * restarts against any CP backend without giving the runner DB credentials.
 *
 * Local mode (no POSTGRES_DSN and no HTTP URL -- zero-dependency fallback):
 * MemorySaver. Ephemeral -- state does NOT survive a runner restart. Blank /
 * whitespace-only HTTP URLs count as "no URL" so a misconfigured empty
 * RUNKITE_HTTP_URL falls through to MemorySaver with a clear warning instead
 * of failing on proxy connection errors.
 */
import type { BaseCheckpointSaver } from "@langchain/langgraph-checkpoint";
export type CheckpointMode = "direct-postgres" | "proxy-http" | "memory";
/** HTTP base for proxy checkpointer, or undefined → MemorySaver.
 *
 * Prefer an explicit RUNKITE_HTTP_URL (including empty → memory). Fall
 * back to the worker --http-address only when that env var is unset.
 * Blank / whitespace never selects proxy — that path used to turn a
 * missing CP into opaque connection errors instead of in-memory mode.
 */
export declare function resolveCheckpointHttpUrl(httpAddress?: string): string | undefined;
export declare class CheckpointerManager {
    checkpointer: BaseCheckpointSaver | null;
    mode: CheckpointMode;
    private closeFn;
    /** poolSize mirrors the Python runner\'s `pool_size=concurrency` --
     * with --concurrency > 1, multiple jobs\' checkpoint reads/writes can
     * be genuinely in flight at once, and a single connection would
     * serialize them regardless of how many jobs the dispatcher allows
     * concurrently. Undefined leaves node-postgres\'s own default (10),
     * fine for the concurrency=1 common case. PostgresSaver.fromConnString
     * doesn\'t expose a pool-size option (only `schema`) -- it\'s a thin
     * wrapper around `new Pool({connectionString}); new
     * PostgresSaver(pool)` (see its own source), so constructing the pool
     * directly and passing it to PostgresSaver\'s other constructor gets
     * the same behavior with pool-size control added. */
    start(postgresDsn: string | undefined, poolSize?: number, opts?: {
        httpBaseUrl?: string;
        runnerToken?: string;
    }): Promise<void>;
    stop(): Promise<void>;
    /** Overrides a compiled graph\'s checkpointer with the shared one -- checkpoint
     * mode is a runner concern, not something agent authors configure in graph.ts. */
    attach(graph: {
        checkpointer?: BaseCheckpointSaver | boolean | null;
    }): void;
}
