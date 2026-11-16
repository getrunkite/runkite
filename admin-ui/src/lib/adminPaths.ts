import { connectorActionRunHref } from "./connectorActions.ts";

/** Build Admin API paths and exact UI record links. */
export function adminTenantQuery(tenantId?: string): string {
  if (tenantId === undefined) return "";
  return `?tenant_id=${encodeURIComponent(tenantId)}`;
}

export function adminAgentPath(agentId: string, tenantId?: string): string {
  return `/agents/${encodeURIComponent(agentId)}${adminTenantQuery(tenantId)}`;
}

// Deliberately separate functions rather than string-suffixing adminAgentPath's
// result: that result already has the ?tenant_id= query appended, so gluing
// "/schemas" onto the end would land inside the query string instead of the
// path (e.g. "?tenant_id=default/schemas") and 404 against the base agent
// route with a garbage tenant id.
export function adminAgentSchemasPath(agentId: string, tenantId?: string): string {
  return `/agents/${encodeURIComponent(agentId)}/schemas${adminTenantQuery(tenantId)}`;
}

export function adminAgentVersionsPath(agentId: string, tenantId?: string): string {
  return `/agents/${encodeURIComponent(agentId)}/versions${adminTenantQuery(tenantId)}`;
}

export function adminRegistryPath(name: string, tenantId?: string): string {
  return `/registry/${encodeURIComponent(name)}${adminTenantQuery(tenantId)}`;
}

export function adminRegistryVersionsPath(name: string, tenantId?: string): string {
  return `/registry/${encodeURIComponent(name)}/versions${adminTenantQuery(tenantId)}`;
}

/** Existing registry records use an explicit edit route, even when named new. */
export function adminRegistryEntryHref(name: string, tenantId?: string): string {
  return `/admin/registry/${encodeURIComponent(name)}/edit${adminTenantQuery(tenantId)}`;
}

export function adminRunPath(runId: string): string {
  return `/runs/${encodeURIComponent(runId)}`;
}

/** Retained-reference views can inspect a guarded snapshot, but bare-ID live
 * operations cannot promise to target that same stored row after replacement.
 */
export function adminRunViewPaths(runId: string, expectedTenant: string | null, expectedIncarnation: string | null) {
  const base = adminRunPath(runId);
  const retained = expectedTenant !== null || expectedIncarnation !== null;
  const query = new URLSearchParams();
  if (expectedTenant !== null) query.set("expected_tenant_id", expectedTenant);
  if (expectedIncarnation !== null) query.set("expected_incarnation_id", expectedIncarnation);
  return {
    retained,
    detail: retained ? `${base}?${query}` : base,
    stream: retained ? null : `${base}/stream`,
    cancel: retained ? null : `${base}/cancel`,
  };
}

/** Reuse Run Detail's existing exact retained-record guard for history links. */
export function adminRetainedRunHref(run: { run_id: string; tenant_id: string; incarnation_id?: string }): string | null {
  return connectorActionRunHref({
    run_id: run.run_id,
    tenant_id: run.tenant_id,
    run_incarnation_id: run.incarnation_id ?? "",
  }, true);
}

export function adminCronRowId(schedule: { tenant_id?: string; name: string }): string {
  return JSON.stringify([schedule.tenant_id ?? "default", schedule.name]);
}

export function adminThreadPath(threadId: string, tenantId?: string): string {
  return `/threads/${encodeURIComponent(threadId)}${adminTenantQuery(tenantId)}`;
}

export function adminThreadRunsPath(threadId: string, tenantId?: string): string {
  return `/threads/${encodeURIComponent(threadId)}/runs${adminTenantQuery(tenantId)}`;
}

/** UI route to the immutable approval record, independent of a reused run ID. */
export function adminPendingActionHref(pendingId: string): string {
  return `/admin/pending/${encodeURIComponent(pendingId)}`;
}
