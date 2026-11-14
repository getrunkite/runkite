export declare function shouldSkipRun(httpAddress: string, runId: string, opts?: {
    runnerKind?: string;
    runnerToken?: string;
    generation?: number;
}): Promise<boolean>;
