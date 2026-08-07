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
