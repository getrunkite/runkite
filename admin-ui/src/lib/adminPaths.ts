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

export function adminThreadPath(threadId: string): string {
  return `/threads/${encodeURIComponent(threadId)}`;
}

/** UI route to the immutable approval record, independent of a reused run ID. */
export function adminPendingActionHref(pendingId: string): string {
  return `/admin/pending/${encodeURIComponent(pendingId)}`;
}
