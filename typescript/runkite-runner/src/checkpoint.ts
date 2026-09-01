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
import { logger } from "./logger.js";
import { ProxyCheckpointSaver } from "./proxyCheckpoint.js";

export type CheckpointMode = "direct-postgres" | "proxy-http" | "memory";

/** HTTP base for proxy checkpointer, or undefined → MemorySaver.
 *
 * Prefer an explicit RUNKITE_HTTP_URL (including empty → memory). Fall
 * back to the worker --http-address only when that env var is unset.
 * Blank / whitespace never selects proxy — that path used to turn a
 * missing CP into opaque connection errors instead of in-memory mode.
 */
export function resolveCheckpointHttpUrl(httpAddress?: string): string | undefined {
  if (Object.prototype.hasOwnProperty.call(process.env, "RUNKITE_HTTP_URL")) {
    return (process.env.RUNKITE_HTTP_URL || "").trim() || undefined;
  }
  return (httpAddress || "").trim() || undefined;
}

export class CheckpointerManager {
  checkpointer: BaseCheckpointSaver | null = null;
  mode: CheckpointMode = "memory";
  private closeFn: (() => Promise<void>) | null = null;

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
  async start(
    postgresDsn: string | undefined,
    poolSize?: number,
    opts?: { httpBaseUrl?: string; runnerToken?: string },
  ): Promise<void> {
    // Defense in depth: callers should already pass undefined for blank,
    // but never let whitespace select ProxyCheckpointSaver by accident.
    const httpBaseUrl = (opts?.httpBaseUrl || "").trim() || undefined;

    if (postgresDsn) {
      const { PostgresSaver } = await import("@langchain/langgraph-checkpoint-postgres");
      const { Pool } = await import("pg");
      // idleTimeoutMillis / connectionTimeoutMillis: node-pg\'s default
      // idle eviction is already short (~10s); pin it explicitly and
      // bound connect so a wedged Postgres cannot hang job startup.
      // Unlike Python\'s psycopg_pool there is no checkout check_connection
      // hook -- stale sockets surface as query errors and the pool
      // discards them on the next checkout.
      const pool = new Pool({
        connectionString: postgresDsn,
        idleTimeoutMillis: 60_000,
        connectionTimeoutMillis: 10_000,
        ...(poolSize ? { max: poolSize } : {}),
      });
      // node-postgres emits \'error\' on idle client backend failures
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
      logger.info(
        "checkpoint mode: direct (postgres) -- LangGraph tables on POSTGRES_DSN; " +
          "requires the control plane to use the same Postgres database (Supported profile). " +
          "If the control plane is MySQL/Mongo/SQLite, unset POSTGRES_DSN on this runner and " +
          "set RUNKITE_HTTP_URL for proxy mode.",
      );
    } else if (httpBaseUrl) {
      const saver = new ProxyCheckpointSaver({
        httpBaseUrl,
        runnerToken: opts?.runnerToken,
      });
      this.checkpointer = saver;
      this.mode = "proxy-http";
      this.closeFn = async () => {
        await saver.close();
      };
      logger.info(
        `checkpoint mode: proxy (HTTP opaque blobs via ${httpBaseUrl.replace(/\/+$/, "")}) -- ` +
          "persists across runner restarts without POSTGRES_DSN",
      );
    } else {
      const { MemorySaver } = await import("@langchain/langgraph");
      this.checkpointer = new MemorySaver();
      this.mode = "memory";
      logger.warn(
        "checkpoint mode: in-memory (no POSTGRES_DSN and no RUNKITE_HTTP_URL) -- " +
          "thread state will NOT survive a runner restart. " +
          "Set POSTGRES_DSN (direct) or RUNKITE_HTTP_URL (proxy) for persistence.",
      );
    }
  }

  async stop(): Promise<void> {
    if (this.closeFn) await this.closeFn();
    this.closeFn = null;
  }

  /** Overrides a compiled graph\'s checkpointer with the shared one -- checkpoint
   * mode is a runner concern, not something agent authors configure in graph.ts. */
  attach(graph: { checkpointer?: BaseCheckpointSaver | boolean | null }): void {
    graph.checkpointer = this.checkpointer!;
  }
}
