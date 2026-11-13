import type { AdminConnectorAction, AdminConnectorActionEvent, AdminPendingAction } from "../api/types.ts";

export interface ReviewSessionStatus {
  authenticated: boolean;
  auth_required: true;
  configured: boolean;
  identity: string;
  tenant_id: string;
  can_read: boolean;
  can_approve: boolean;
  csrf_token?: string;
  expires_at?: string | null;
}

export interface ReviewIdentity {
  identity: string;
  tenant_id: string;
  can_read: true;
  can_approve: boolean;
  expires_at: string;
}

export interface ReviewSnapshot {
  checked: boolean;
  configured: boolean;
  identity: ReviewIdentity | null;
  revision: number;
}

export type ReviewPendingAction = Omit<AdminPendingAction, "reason" | "args"> & { args?: Record<string, unknown> };
export interface ReviewActionDetail {
  action: AdminConnectorAction;
  events: AdminConnectorActionEvent[];
  events_truncated?: boolean;
}

export const REVIEW_ROOT = "/admin/review";
export const REVIEW_HELP = "https://getrunkite.com/support/tenant-review.html";
export function isReviewRoute(path: string): boolean {
  return path === REVIEW_ROOT || path.startsWith(`${REVIEW_ROOT}/`);
}

export function reviewDetailPath(kind: "connector-actions" | "pending-actions", id: string): string {
  if (!id || id === "." || id === "..") throw new Error("Invalid review record ID.");
  return `/${kind}/${encodeURIComponent(id)}`;
}

export function reviewDetailHref(kind: "connector-actions" | "pending-actions", id: string): string {
  return `${REVIEW_ROOT}/${kind === "pending-actions" ? "pending" : kind}/${encodeURIComponent(id)}`;
}

/** Tenant selection is never inferred from a URL or sent as a query filter. */
export function reviewListPath(kind: "connector-actions" | "pending-actions", params: URLSearchParams, cursor?: string): string {
  const query = new URLSearchParams({ limit: "50" });
  for (const key of ["run_id", "connector", kind === "pending-actions" ? "status" : "state"]) {
    const value = params.get(key);
    if (value && value !== "all") query.set(key, value);
  }
  if (cursor) query.set("cursor", cursor);
  return `/${kind}?${query}`;
}

export function reviewListScope(kind: "connector-actions" | "pending-actions", params: URLSearchParams): string {
  return reviewListPath(kind, params);
}
