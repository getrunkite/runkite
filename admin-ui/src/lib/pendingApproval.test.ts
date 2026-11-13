import assert from "node:assert/strict";
import test from "node:test";
import { pendingApprovalBlockReason } from "./pendingApproval.ts";

const current = { id: "pending-v2-fixture", args_digest: "bound-digest" };

test("managed approvals without an original run incarnation require fresh review", () => {
  assert.match(pendingApprovalBlockReason(current)!, /original run identity/);
  assert.match(pendingApprovalBlockReason({ ...current, run_incarnation_id: "" })!, /fresh review/);
  assert.match(pendingApprovalBlockReason({ ...current, expires_at: null })!, /fresh review/);
});

test("opaque managed incarnations are accepted without UUID or timestamp assumptions", () => {
  assert.equal(pendingApprovalBlockReason({ ...current, run_incarnation_id: "db-generated-opaque-proof" }), null);
  assert.equal(pendingApprovalBlockReason({ ...current, run_incarnation_id: "1234567890abcdef1234567890abcdef" }), null);
});

test("standalone decision approvals remain unbound to managed runs", () => {
  assert.equal(pendingApprovalBlockReason({ ...current, expires_at: "2026-11-14T00:00:00Z" }), null);
});

test("an incarnation or expiry cannot repair a legacy argument binding", () => {
  assert.match(pendingApprovalBlockReason({ ...current, args_digest: "", run_incarnation_id: "proof" })!, /Legacy argument binding/);
  assert.match(pendingApprovalBlockReason({ ...current, id: "pending-old", run_incarnation_id: "proof" })!, /Legacy argument binding/);
  assert.match(pendingApprovalBlockReason({ ...current, id: "pending-old", expires_at: "2026-11-14T00:00:00Z" })!, /Legacy argument binding/);
});
