import type { ExecutionAdapter } from "./execution.js";
import type { RunAssignment, RunEvent, RunStatus } from "./executeRun.js";
export declare class LangGraphExecutionAdapter implements ExecutionAdapter {
    private readonly options;
    private readonly adapter;
    private checkpointerManager?;
    private store?;
    constructor(configPath: string, options: {
        httpAddress: string;
        runnerToken?: string;
        concurrency?: number;
    });
    load(): Promise<void>;
    execute(assignment: RunAssignment, emit: (event: RunEvent) => Promise<void>, isCancelled: () => boolean): Promise<RunStatus>;
    close(): Promise<void>;
}
