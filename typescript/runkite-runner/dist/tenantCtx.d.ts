export type RunBinding = {
    tenantId: string;
    runId: string;
    generation: number;
};
/** Header runners send on /internal/* (matches internal/auth.HeaderTenantID). */
export declare const HEADER_TENANT_ID = "X-Runkite-Tenant-Id";
export declare const HEADER_RUN_ID = "X-Runkite-Run-Id";
export declare const HEADER_GENERATION = "X-Runkite-Generation";
export declare function currentTenant(): string;
export declare function runWithTenant<T>(tenantId: string | undefined, fn: () => Promise<T>): Promise<T>;
/** Bind tenant + run id/generation for the duration of fn (proxy headers). */
export declare function runWithBinding<T>(opts: {
    tenantId?: string;
    runId: string;
    generation?: number;
}, fn: () => Promise<T>): Promise<T>;
export declare function tenantHeaders(): Record<string, string>;
/** LangGraph checkpointer key for this assignment's logical thread.
 * Empty / "default" → bare threadId (existing single-tenant rows stay
 * reachable). Any other tenant → `${tenantId}:${threadId}`. */
export declare function checkpointThreadId(tenantId: string | undefined | null, threadId: string): string;
/** Bare threads.thread_id for /internal/checkpoints HTTP paths.
 * Inverse of checkpointThreadId for the active tenant: direct
 * PostgresSaver needs the prefixed configurable.thread_id (no
 * tenant column on LangGraph's tables), but proxy mode stores under the
 * real threads.thread_id PK and run-binding compares against that bare
 * id. Stripping here keeps both modes correct. */
export declare function storageThreadId(configThreadId: string): string;
