/**
 * Per-run tenant binding for direct-mode store SQL and proxy headers.
 * Mirrors python/runkite_runner/tenant_ctx.py -- see that module's docstring.
 *
 * Also owns the LangGraph checkpointer thread-id encoding (see
 * checkpointThreadId) so PostgresSaver rows do not collide across tenants.
 */
import { AsyncLocalStorage } from "node:async_hooks";
const DEFAULT_TENANT = "default";
const als = new AsyncLocalStorage();
/** Header runners send on /internal/* (matches internal/auth.HeaderTenantID). */
export const HEADER_TENANT_ID = "X-Runkite-Tenant-Id";
export function currentTenant() {
    return als.getStore() || DEFAULT_TENANT;
}
export function runWithTenant(tenantId, fn) {
    const tid = (tenantId ?? "").trim() || DEFAULT_TENANT;
    return als.run(tid, fn);
}
export function tenantHeaders() {
    return { [HEADER_TENANT_ID]: currentTenant() };
}
/** LangGraph checkpointer key for this assignment's logical thread.
 * Empty / "default" → bare threadId (existing single-tenant rows stay
 * reachable). Any other tenant → `${tenantId}:${threadId}`. */
export function checkpointThreadId(tenantId, threadId) {
    const tid = (tenantId ?? "").trim() || DEFAULT_TENANT;
    if (tid === DEFAULT_TENANT)
        return threadId;
    return `${tid}:${threadId}`;
}
