import type { LanguageModel, ToolLoopAgentSettings, ToolSet } from "ai";
import type { ExecutionAdapter } from "./execution.js";
import { type RunAssignment, type RunEvent, type RunStatus } from "./executeRun.js";
export type AiSdkAgentDefinition = Pick<ToolLoopAgentSettings<never, ToolSet>, "instructions" | "tools" | "stopWhen" | "maxRetries" | "maxOutputTokens" | "temperature" | "providerOptions"> & {
    model: LanguageModel;
};
export interface AiSdkRunContext {
    readonly runId: string;
    readonly threadId: string;
    readonly tenantId: string;
    readonly generation: number;
    readonly signal: AbortSignal;
    /** Existing connector helpers accept this run-bound configuration. */
    readonly runkiteConfig: Record<string, unknown>;
}
export type AiSdkAgentFactory = (context: AiSdkRunContext) => AiSdkAgentDefinition | Promise<AiSdkAgentDefinition>;
/** Loads per-run factories. Full native approval/resume and binary state are outside v1. */
export declare class AiSdkAdapter implements ExecutionAdapter {
    private readonly configPath;
    private readonly options;
    private readonly factories;
    private readonly state;
    constructor(configPath: string, options: {
        httpAddress: string;
        runnerKind?: string;
        runnerToken?: string;
        fetch?: typeof fetch;
    });
    load(): Promise<void>;
    close(): Promise<void>;
    execute(assignment: RunAssignment, emit: (event: RunEvent) => Promise<void>, isCancelled: () => boolean): Promise<RunStatus>;
    private executeBound;
}
