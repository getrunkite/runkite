import type { RunAssignment, RunEvent, RunStatus } from "./executeRun.js";

/** Execution is separate from dispatch, authentication and completion delivery. */
export interface RunExecutor {
  execute(
    assignment: RunAssignment,
    emit: (event: RunEvent) => Promise<void>,
    isCancelled: () => boolean,
  ): Promise<RunStatus>;
}

/** One runner process loads its framework once, then executes isolated runs. */
export interface ExecutionAdapter extends RunExecutor {
  load(): Promise<void>;
  close(): Promise<void>;
}
