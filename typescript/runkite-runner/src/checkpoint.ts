/**
 * Checkpoint dual mode (master plan: "Checkpoint Dual Mode"). Direct
 * mirror of the Python runner's checkpoint.py -- see that file's
 * docstring for the full rationale, repeated briefly here:
 *
 * Direct mode (POSTGRES_DSN set): PostgresSaver from
 * @langchain/langgraph-checkpoint-postgres, LangGraph.js's own native
 * Postgres checkpointer. Zero added latency, survives runner restarts.
 * Uses LangGraph's own checkpoint tables (not the control plane's
 * thread_checkpoints) -- completely separate schema, same as the Python
 * runner's AsyncPostgresSaver.
 *
 * Local mode (no POSTGRES_DSN): MemorySaver, honestly ephemeral -- state
 * does NOT survive a runner restart. Accepted trade-off for the
 * zero-dependency default, not a hidden gap.
 */
import type { BaseCheckpointSaver } from "@langchain/langgraph-checkpoint";
import { logger } from "./logger.js";

export type CheckpointMode = "direct-postgres" | "memory";

export class CheckpointerManager {
  checkpointer: BaseCheckpointSaver | null = null;
  mode: CheckpointMode = "memory";
  private closeFn: (() => Promise<void>) | null = null;

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
  async start(postgresDsn: string | undefined, poolSize?: number): Promise<void> {
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
    } else {
      const { MemorySaver } = await import("@langchain/langgraph");
      this.checkpointer = new MemorySaver();
      this.mode = "memory";
      logger.warn(
        "checkpoint mode: in-memory (no POSTGRES_DSN set) -- " +
          "thread state will NOT survive a runner restart. " +
          "Set POSTGRES_DSN for production persistence.",
      );
    }
  }

  async stop(): Promise<void> {
    if (this.closeFn) await this.closeFn();
  }

  /** Overrides a compiled graph's checkpointer with the shared one -- checkpoint
   * mode is a runner concern, not something agent authors configure in graph.ts. */
  attach(graph: { checkpointer?: BaseCheckpointSaver | boolean | null }): void {
    graph.checkpointer = this.checkpointer!;
  }
}
