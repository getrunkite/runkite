/** Build /admin-api paths with optional tenant scoping. */
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
