import { connectorActionRunHref } from "./connectorActions.ts";

/** Build Admin API paths and exact UI record links. */
export function adminTenantQuery(tenantId?: string): string {
  const tid = tenantId?.trim();
  if (!tid) return "";
  return `?tenant_id=${encodeURIComponent(tid)}`;
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

export function adminRunPath(runId: string): string {
  return `/runs/${encodeURIComponent(runId)}`;
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
  return `/threads/${encodeURIComponent(threadId)}${threadTenantQuery(tenantId)}`;
}

export function adminThreadRunsPath(threadId: string, tenantId?: string): string {
  return `/threads/${encodeURIComponent(threadId)}/runs${threadTenantQuery(tenantId)}`;
}

// A selected tenant is an exact identifier. Preserve even an empty selection
// so a scoped thread operation cannot become a global Admin operation.
function threadTenantQuery(tenantId?: string): string {
  return tenantId === undefined ? "" : `?tenant_id=${encodeURIComponent(tenantId)}`;
}

/** UI route to the immutable approval record, independent of a reused run ID. */
export function adminPendingActionHref(pendingId: string): string {
  return `/admin/pending/${encodeURIComponent(pendingId)}`;
}
