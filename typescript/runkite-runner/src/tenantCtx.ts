/**
 * Per-run tenant + run-binding for direct-mode store SQL and proxy headers.
 * Mirrors python/runkite_runner/tenant_ctx.py -- see that module's docstring.
 *
 * Also owns the LangGraph checkpointer thread-id encoding (see
 * checkpointThreadId) so PostgresSaver rows do not collide across tenants.
 */
import { AsyncLocalStorage } from "node:async_hooks";

const DEFAULT_TENANT = "default";

export type RunBinding = {
  tenantId: string;
  runId: string;
  generation: number;
};

const als = new AsyncLocalStorage<RunBinding>();

/** Header runners send on /internal/* (matches internal/auth.HeaderTenantID). */
export const HEADER_TENANT_ID = "X-Runkite-Tenant-Id";
export const HEADER_RUN_ID = "X-Runkite-Run-Id";
export const HEADER_GENERATION = "X-Runkite-Generation";

export function currentTenant(): string {
  return als.getStore()?.tenantId || DEFAULT_TENANT;
}

export function runWithTenant<T>(tenantId: string | undefined, fn: () => Promise<T>): Promise<T> {
  const tid = (tenantId ?? "").trim() || DEFAULT_TENANT;
  const prev = als.getStore();
  return als.run(
    {
      tenantId: tid,
      runId: prev?.runId ?? "",
      generation: prev?.generation ?? 0,
    },
    fn,
  );
}

/** Bind tenant + run id/generation for the duration of fn (proxy headers). */
export function runWithBinding<T>(
  opts: { tenantId?: string; runId: string; generation?: number },
  fn: () => Promise<T>,
): Promise<T> {
  const tid = (opts.tenantId ?? "").trim() || DEFAULT_TENANT;
  return als.run(
    {
      tenantId: tid,
      runId: (opts.runId ?? "").trim(),
      generation: opts.generation ?? 0,
    },
    fn,
  );
}

export function tenantHeaders(): Record<string, string> {
  const store = als.getStore();
  const headers: Record<string, string> = {
    [HEADER_TENANT_ID]: store?.tenantId || DEFAULT_TENANT,
  };
  if (store?.runId) {
    headers[HEADER_RUN_ID] = store.runId;
    headers[HEADER_GENERATION] = String(store.generation ?? 0);
  }
  return headers;
}

/** LangGraph checkpointer key for this assignment's logical thread.
 * Empty / "default" → bare threadId (existing single-tenant rows stay
 * reachable). Any other tenant → `${tenantId}:${threadId}`. */
export function checkpointThreadId(tenantId: string | undefined | null, threadId: string): string {
  const tid = (tenantId ?? "").trim() || DEFAULT_TENANT;
  if (tid === DEFAULT_TENANT) return threadId;
  return `${tid}:${threadId}`;
}
