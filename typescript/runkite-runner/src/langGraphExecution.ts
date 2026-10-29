/** LangGraph resource ownership, independent of the Runner Protocol worker. */
import { LangGraphAdapter } from "./adapter.js";
import { CheckpointerManager, resolveCheckpointHttpUrl } from "./checkpoint.js";
import type { ExecutionAdapter } from "./execution.js";
import type { RunAssignment, RunEvent, RunStatus } from "./executeRun.js";
import { logger } from "./logger.js";
import { RunkiteStore } from "./store.js";

export class LangGraphExecutionAdapter implements ExecutionAdapter {
  private readonly adapter: LangGraphAdapter;
  private checkpointerManager?: CheckpointerManager;
  private store?: RunkiteStore;

  constructor(
    configPath: string,
    private readonly options: { httpAddress: string; runnerToken?: string; concurrency?: number },
  ) {
    this.adapter = new LangGraphAdapter(configPath);
  }

  async load(): Promise<void> {
    await this.adapter.load();
    // Preserve node-postgres's default pool floor for single-run graphs that
    // fan out into concurrent store/checkpoint calls.
    const poolSize = Math.max(this.options.concurrency ?? 1, 10);
    const postgresDsn = process.env.POSTGRES_DSN;
    const checkpointHttp = postgresDsn ? undefined : resolveCheckpointHttpUrl(this.options.httpAddress);
    const runnerToken = this.options.runnerToken;
    const checkpointerManager = new CheckpointerManager();
    this.checkpointerManager = checkpointerManager;
    try {
      await checkpointerManager.start(postgresDsn, poolSize, {
        httpBaseUrl: checkpointHttp,
        runnerToken: checkpointHttp ? runnerToken || undefined : undefined,
      });
      this.adapter.attachCheckpointer(checkpointerManager);
      const store = new RunkiteStore({
        postgresDsn,
        httpBaseUrl: this.options.httpAddress,
        runnerToken: runnerToken || undefined,
        poolSize,
      });
      this.store = store;
      this.adapter.attachStore(store);
      logger.info(`Store mode: ${store.mode}`);
    } catch (error) {
      await this.close();
      throw error;
    }
  }

  execute(
    assignment: RunAssignment,
    emit: (event: RunEvent) => Promise<void>,
    isCancelled: () => boolean,
  ): Promise<RunStatus> {
    return this.adapter.execute(assignment, emit, isCancelled);
  }

  async close(): Promise<void> {
    try {
      await this.checkpointerManager?.stop();
    } finally {
      await this.store?.close();
      this.checkpointerManager = undefined;
      this.store = undefined;
    }
  }
}
