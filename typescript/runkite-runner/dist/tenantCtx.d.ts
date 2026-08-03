/** Header runners send on /internal/* (matches internal/auth.HeaderTenantID). */
export declare const HEADER_TENANT_ID = "X-Runkite-Tenant-Id";
export declare function currentTenant(): string;
export declare function runWithTenant<T>(tenantId: string | undefined, fn: () => Promise<T>): Promise<T>;
export declare function tenantHeaders(): Record<string, string>;
/** LangGraph checkpointer key for this assignment's logical thread.
 * Empty / "default" → bare threadId (existing single-tenant rows stay
 * reachable). Any other tenant → `${tenantId}:${threadId}`. */
export declare function checkpointThreadId(tenantId: string | undefined | null, threadId: string): string;
