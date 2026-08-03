/**
 * Per-run tenant binding for direct-mode store SQL and proxy headers.
 * Mirrors python/runkite_runner/tenant_ctx.py -- see that module's docstring.
 */
import { AsyncLocalStorage } from "node:async_hooks";

const DEFAULT_TENANT = "default";
const als = new AsyncLocalStorage<string>();

/** Header runners send on /internal/* (matches internal/auth.HeaderTenantID). */
export const HEADER_TENANT_ID = "X-Runkite-Tenant-Id";

export function currentTenant(): string {
  return als.getStore() || DEFAULT_TENANT;
}

export function runWithTenant<T>(tenantId: string | undefined, fn: () => Promise<T>): Promise<T> {
  const tid = (tenantId ?? "").trim() || DEFAULT_TENANT;
  return als.run(tid, fn);
}

export function tenantHeaders(): Record<string, string> {
  return { [HEADER_TENANT_ID]: currentTenant() };
}
