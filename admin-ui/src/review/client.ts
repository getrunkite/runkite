import { pendingApprovalBlockReason } from "../lib/pendingApproval.ts";
import { reviewDetailPath } from "./contracts.ts";
import type { ReviewIdentity, ReviewPendingAction, ReviewSessionStatus, ReviewSnapshot } from "./contracts.ts";

const BASE = "/review-api";
function assertReviewScope(data: unknown, tenant: string): void {
  const check = (row: unknown) => {
    if (!row || typeof row !== "object" || !("tenant_id" in row) || row.tenant_id !== tenant)
      throw new ReviewError(503, "Review returned an unexpected tenant scope. No records were displayed.");
  };
  if (Array.isArray(data)) data.forEach(check);
  else if (data && typeof data === "object" && "action" in data) {
    check(data.action);
    if (!("events" in data) || !Array.isArray(data.events)) throw new ReviewError(503, "Review returned an invalid action timeline.");
    data.events.forEach(check);
  } else check(data);
}
export class ReviewError extends Error {
  status: number;
  constructor(status: number, message: string) { super(message); this.status = status; }
}

/** Independent session and CSRF memory. Never reads or writes Admin credentials. */
export function createReviewClient() {
  let csrf: string | null = null;
  let snapshot: ReviewSnapshot = { checked: false, configured: true, identity: null, revision: 0 };
  const listeners = new Set<() => void>();
  const controllers = new Set<AbortController>();
  const publish = (identity: ReviewIdentity | null, configured = snapshot.configured) => {
    csrf = identity ? csrf : null;
    snapshot = { checked: true, configured, identity, revision: snapshot.revision + 1 };
    for (const controller of controllers) controller.abort();
    listeners.forEach((listener) => listener());
  };
  const expire = () => publish(null);
  function current() {
    if (snapshot.identity && Date.parse(snapshot.identity.expires_at) <= Date.now()) expire();
    return snapshot.identity;
  }
  function accept(status: ReviewSessionStatus, revision: number) {
    if (revision !== snapshot.revision) throw new ReviewError(409, "An obsolete Review session response was discarded.");
    if (status.auth_required !== true || typeof status.configured !== "boolean") throw new ReviewError(503, "Review authentication configuration could not be verified.");
    if (!status.authenticated) { publish(null, status.configured); return; }
    if (!status.configured || typeof status.identity !== "string" || !status.identity || typeof status.tenant_id !== "string" || !status.tenant_id || status.can_read !== true || typeof status.can_approve !== "boolean" || typeof status.csrf_token !== "string" || !status.csrf_token || !status.expires_at || !Number.isFinite(Date.parse(status.expires_at)) || Date.parse(status.expires_at) <= Date.now()) {
      expire();
      throw new ReviewError(403, "Review requires a verified identity, explicit tenant and governance permission.");
    }
    csrf = status.csrf_token;
    publish({ identity: status.identity, tenant_id: status.tenant_id, can_read: true, can_approve: status.can_approve, expires_at: status.expires_at }, true);
  }
  async function request<T>(path: string, init: RequestInit = {}, session = false): Promise<{ data: T; nextCursor: string | null; revision: number }> {
    if (!/^\/(?:connector-actions|pending-actions)(?:\/[^/?#]+)?(?:\?[^#]*)?$/.test(path) && !/^\/pending-actions\/[^/?#]+\/(?:approve|deny)$/.test(path) && path !== "/session") throw new ReviewError(400, "Unsupported Review endpoint.");
    const identity = session ? null : current();
    if (!session && !identity) throw new ReviewError(401, "Your Review session ended. Sign in again.");
    const revision = snapshot.revision;
    const controller = new AbortController();
    controllers.add(controller);
    const signal = AbortSignal.any([controller.signal, AbortSignal.timeout(10_000), ...(init.signal ? [init.signal] : [])]);
    const headers = new Headers(init.headers);
    const method = init.method ?? "GET";
    if (!["GET", "HEAD"].includes(method) && csrf) headers.set("X-CSRF-Token", csrf);
    try {
      const response = await fetch(`${BASE}${path}`, { ...init, headers, signal, credentials: "include", redirect: "error" });
      signal.throwIfAborted();
      if (!session && revision !== snapshot.revision) throw new ReviewError(401, "An obsolete Review response was discarded.");
      if (!response.ok) {
        let message = `${response.status} ${response.statusText}`;
        try { const body = await response.json(); if (body && typeof body === "object" && "message" in body && typeof body.message === "string") message = body.message; } catch { /* Keep the status line. */ }
        if (response.status === 401 && revision === snapshot.revision) expire();
        throw new ReviewError(response.status, message);
      }
      const data = response.status === 204 ? undefined as T : await response.json() as T;
      signal.throwIfAborted();
      if (!session && (revision !== snapshot.revision || !current())) throw new ReviewError(401, "An expired Review response was discarded.");
      if (!session) assertReviewScope(data, identity!.tenant_id);
      return { data, nextCursor: response.headers.get("X-Next-Cursor"), revision };
    } finally { controllers.delete(controller); }
  }
  return {
    snapshot: () => snapshot,
    subscribe: (listener: () => void) => { listeners.add(listener); return () => { listeners.delete(listener); }; },
    expire,
    async status(signal?: AbortSignal) {
      const result = await request<ReviewSessionStatus>("/session", { signal }, true);
      accept(result.data, result.revision);
    },
    async login(credential: string) {
      publish(null);
      const result = await request<ReviewSessionStatus>("/session", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ credential }) }, true);
      accept(result.data, result.revision);
    },
    async logout() {
      await request<void>("/session", { method: "DELETE" }, true);
      publish(null);
    },
    getWithMeta: <T>(path: string, signal?: AbortSignal) => request<T>(path, { signal }),
    async resolve(action: ReviewPendingAction, verb: "approve" | "deny", signal?: AbortSignal) {
      if (!current()?.can_approve) throw new ReviewError(403, "This Review session has read-only access.");
      if (action.tenant_id !== snapshot.identity?.tenant_id) throw new ReviewError(403, "This approval is outside your Review tenant.");
      if (action.status !== "pending") throw new ReviewError(409, "This approval is no longer pending.");
      if (action.expires_at && Date.parse(action.expires_at) <= Date.now()) throw new ReviewError(409, "This approval has expired. Resubmit the operation for review.");
      const blocked = verb === "approve" ? pendingApprovalBlockReason(action) : null;
      if (blocked) throw new ReviewError(409, blocked);
      return (await request<ReviewPendingAction>(`${reviewDetailPath("pending-actions", action.id)}/${verb}`, { method: "POST", signal })).data;
    },
  };
}

export const reviewClient = createReviewClient();
