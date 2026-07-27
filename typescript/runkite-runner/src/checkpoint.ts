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

export type CheckpointMode = "direct-postgres" | "memory";

export class CheckpointerManager {
  checkpointer: BaseCheckpointSaver | null = null;
  mode: CheckpointMode = "memory";
  private closeFn: (() => Promise<void>) | null = null;

  async start(postgresDsn: string | undefined): Promise<void> {
    if (postgresDsn) {
      const { PostgresSaver } = await import("@langchain/langgraph-checkpoint-postgres");
      const saver = PostgresSaver.fromConnString(postgresDsn);
      await saver.setup();
      this.checkpointer = saver;
      this.mode = "direct-postgres";
      this.closeFn = async () => {
        await saver.end();
      };
      console.log("checkpoint mode: direct (postgres) -- state survives runner restarts");
    } else {
      const { MemorySaver } = await import("@langchain/langgraph");
      this.checkpointer = new MemorySaver();
      this.mode = "memory";
      console.warn(
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
