import assert from "node:assert/strict";
import { test } from "node:test";
import { isReviewRoute, reviewDetailHref, reviewDetailPath, reviewListPath, reviewListScope, type ReviewIdentity, type ReviewPendingAction } from "./contracts.ts";
import { reviewPendingControls } from "./pendingControls.ts";

test("Review is a distinct route namespace and exact IDs stay encoded within it", () => {
  assert.equal(isReviewRoute("/admin/review"), true);
  assert.equal(isReviewRoute("/admin/review/pending/id"), true);
  for (const path of ["/admin", "/admin/runs", "/admin/reviewer", "/review-api"]) assert.equal(isReviewRoute(path), false);
  const id = "pending-v2-a/../雪?x=1";
  assert.equal(reviewDetailPath("pending-actions", id), `/pending-actions/${encodeURIComponent(id)}`);
  assert.equal(reviewDetailHref("pending-actions", id), `/admin/review/pending/${encodeURIComponent(id)}`);
});

test("list filters never accept tenant selection and scope changes discard old cursors", () => {
  const params = new URLSearchParams({ tenant_id: "attacker-tenant", run_id: "run&exact", connector: "c?1", state: "outcome_unknown", cursor: "stale-cursor" });
  const result = new URL(reviewListPath("connector-actions", params, "next?cursor"), "https://example.test");
  assert.equal(result.searchParams.get("tenant_id"), null);
  assert.equal(result.searchParams.get("run_id"), "run&exact");
  assert.equal(result.searchParams.get("connector"), "c?1");
  assert.equal(result.searchParams.get("cursor"), "next?cursor");
  assert.equal(new URL(reviewListScope("connector-actions", params), "https://example.test").searchParams.get("cursor"), null);
  const changed = new URLSearchParams(params); changed.set("run_id", "replacement");
  assert.notEqual(reviewListScope("connector-actions", params), reviewListScope("connector-actions", changed));
});

test("permission controls preserve legacy review and expiry boundaries", () => {
  const identity = { identity: "reader", tenant_id: "a", can_read: true, can_approve: false, expires_at: "2030-01-01T00:00:00Z" } as ReviewIdentity;
  const action = { id: "pending-v2-test", tenant_id: "a", status: "pending", args_digest: "digest", run_incarnation_id: "opaque-db-proof" } as ReviewPendingAction;
  assert.equal(reviewPendingControls(identity, action).showActions, false);
  const approver = { ...identity, can_approve: true };
  assert.deepEqual(reviewPendingControls(approver, action), { showActions: true, expired: false, approveBlocked: null, denyBlocked: false });
  const legacy = reviewPendingControls(approver, { ...action, run_incarnation_id: undefined });
  assert.match(legacy.approveBlocked!, /fresh review/); assert.equal(legacy.denyBlocked, false);
  assert.equal(reviewPendingControls(approver, { ...action, run_incarnation_id: undefined, expires_at: "2030-01-01T00:00:00Z" }).approveBlocked, null);
  const expired = reviewPendingControls(approver, { ...action, expires_at: "2020-01-01T00:00:00Z" });
  assert.equal(expired.expired, true); assert.equal(expired.denyBlocked, true);
  assert.equal(reviewPendingControls(approver, { ...action, tenant_id: "foreign" }).showActions, false);
});
