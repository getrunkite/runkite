import type { CredentialLease } from "../api/types.ts";

export const DELIVERY_STATUSES = ["pending", "delivered", "dead_letter"] as const;

function recordId(id: string): string {
  if (!id || id === "." || id === "..") throw new Error("Invalid record identity.");
  return encodeURIComponent(id);
}
function explicitTenant(tenant: string): string {
  if (!tenant.trim()) throw new Error("Select an explicit tenant.");
  return new URLSearchParams({ tenant_id: tenant.trim() }).toString();
}
export function credentialLeasePath(tenant: string, id?: string, verb?: "renew" | "revoke") {
  if (verb && !id) throw new Error("A lease identity is required.");
  return `/credential-leases${id ? `/${recordId(id)}` : ""}${verb ? `/${verb}` : ""}?${explicitTenant(tenant)}`;
}
export function reviewCredentialLeasePath(id?: string) {
  return `/credential-leases${id ? `/${recordId(id)}` : ""}`;
}
export function credentialLeaseHref(ref: string | null | undefined, tenant: string, review = false): string | null {
  if (typeof ref !== "string" || ref.length !== 61 || !/^cred-[0-9a-f]{56}$/.test(ref) || !tenant.trim()) return null;
  return review ? `/admin/review/credential-leases/${recordId(ref)}` : `/admin/credential-leases/${recordId(ref)}?${explicitTenant(tenant)}`;
}
export function actionDeliveriesPath(tenant: string, status = "", sink = "", offset = 0, limit = 50) {
  if ((status && !DELIVERY_STATUSES.includes(status as typeof DELIVERY_STATUSES[number])) || !Number.isInteger(offset) || offset < 0 || offset > 10000 || !Number.isInteger(limit) || limit < 1 || limit > 100) throw new Error("Invalid delivery filters.");
  const params = new URLSearchParams(explicitTenant(tenant));
  params.set("limit", String(limit)); params.set("offset", String(offset));
  if (status) params.set("status", status);
  if (sink) params.set("sink_id", sink);
  return `/action-deliveries?${params}`;
}
export function actionDeliveryRetryPath(tenant: string, id: string) { return `/action-deliveries/${recordId(id)}/retry?${explicitTenant(tenant)}`; }
export function leaseControls(record: CredentialLease, now = Date.now()) {
  return {
    canRenew: record.state === "active" && record.renewable && !record.cleanup_requested && Number.isFinite(Date.parse(record.expires_at)) && Date.parse(record.expires_at) > now,
    canRevoke: !["revoked", "not_issued"].includes(record.state),
  };
}

const LEASE_STATES: Record<string, string> = { issuance_intent: "Issuance intent", active: "Active", issuance_unknown: "Issuance unknown", not_issued: "Not issued", renewal_intent: "Renewal intent", renewal_unknown: "Renewal unknown", revocation_intent: "Revocation intent", revocation_unknown: "Revocation unknown", revoked: "Revoked" };
export function leaseStateLabel(state: string) { return Object.hasOwn(LEASE_STATES, state) ? LEASE_STATES[state] : "Unrecognized state"; }
export function leaseTime(value?: string) { return value && Date.parse(value) > 0 ? new Date(value).toLocaleString() : "Not established"; }
