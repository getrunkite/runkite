import assert from "node:assert/strict";
import test from "node:test";
import { pendingApprovalBlockReason, validManagedTargetRevision, approvalProgress, pendingDecisionFeedback, pendingApprovalGuidance, approvalAuthorityDescription } from "./pendingApproval.ts";

const target = `target-v1-${"a".repeat(64)}`;
const current = { id: "pending-v2-fixture", args_digest: "bound-digest", target_revision: target, policy_revision: `policy-v1-${"a".repeat(64)}` };

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
  assert.equal(pendingApprovalBlockReason({ ...current, target_revision: undefined, expires_at: "2026-11-14T00:00:00Z" }), null);
  assert.equal(pendingApprovalBlockReason({ ...current, target_revision: "not-managed", expires_at: "2026-11-14T00:00:00Z" }), null);
});

test("managed target bindings require the exact supported lowercase digest shape", () => {
  assert.equal(validManagedTargetRevision(target), true);
  assert.equal(validManagedTargetRevision(`target-v2-${"a".repeat(64)}`), true);
  assert.equal(validManagedTargetRevision(`target-v3-${"a".repeat(64)}`), true);
  assert.equal(validManagedTargetRevision(`target-v4-${"a".repeat(64)}`), true);
  assert.equal(pendingApprovalBlockReason({ ...current, run_incarnation_id: "original-run", target_revision: `target-v4-${"a".repeat(64)}` }), null);
  for (const target_revision of [undefined, "", "a".repeat(64), `target-v5-${"a".repeat(64)}`, `target-v1-${"A".repeat(64)}`, `target-v1-${"a".repeat(63)}`, `target-v1-${"a".repeat(65)}`, `${target}\n`, `${target}\r`, ` ${target}`]) {
    assert.equal(validManagedTargetRevision(target_revision), false);
    assert.match(pendingApprovalBlockReason({ ...current, run_incarnation_id: "original-run", target_revision })!, /connector target revision.*fresh review/);
  }
  assert.equal(pendingApprovalBlockReason({ ...current, run_incarnation_id: "original-run" }), null);
});

test("an incarnation or expiry cannot repair a legacy argument binding", () => {
  assert.match(pendingApprovalBlockReason({ ...current, args_digest: "", run_incarnation_id: "proof" })!, /Legacy argument binding/);
  assert.match(pendingApprovalBlockReason({ ...current, id: "pending-old", run_incarnation_id: "proof" })!, /Legacy argument binding/);
  assert.match(pendingApprovalBlockReason({ ...current, id: "pending-old", expires_at: "2026-11-14T00:00:00Z" })!, /Legacy argument binding/);
});


test("managed policy and optional tool contract require exact versioned shapes", () => {
  const managed = { ...current, run_incarnation_id: "opaque-db-proof" };
  for (const policy_revision of [undefined, "", `policy-v2-${"a".repeat(64)}`, `policy-v1-${"A".repeat(64)}`, `${current.policy_revision}\n`]) assert.match(pendingApprovalBlockReason({ ...managed, policy_revision })!, /policy revision/);
  assert.equal(pendingApprovalBlockReason({ ...managed, target_revision: `target-v2-${"a".repeat(64)}`, tool_revision: `tool-v1-${"a".repeat(64)}` }), null);
  for (const tool_revision of [`tool-v2-${"a".repeat(64)}`, `tool-v1-${"A".repeat(64)}`, `tool-v1-${"a".repeat(63)}`]) assert.match(pendingApprovalBlockReason({ ...managed, tool_revision })!, /tool schema revision/);
  assert.equal(pendingApprovalBlockReason({ ...managed, expires_at: "2030-01-01", policy_revision: undefined, tool_revision: "app-owned" }), null);
});

test("partial review never claims approval or tells the caller to continue", () => {
  const action = { ...current, status: "pending", required_approvals: 3, approval_count: 1 };
  assert.deepEqual(approvalProgress(action), { required: 3, count: 1, label: "1/3 reviews" });
  const partial = pendingDecisionFeedback(action, "approve");
  assert.equal(partial.title, "Review recorded"); assert.match(partial.description, /1 of 3/); assert.match(partial.description, /remains pending/); assert.doesNotMatch(partial.description, /may submit|can continue/);
  assert.equal(pendingDecisionFeedback({ ...action, status: "approved", approval_count: 3 }, "approve").title, "Approved");
  assert.notEqual(pendingDecisionFeedback({ ...action, status: "approved" }, "approve").title, "Approved");
  assert.equal(pendingDecisionFeedback(action, "deny").title, "Denied");
  assert.match(pendingApprovalBlockReason({ ...action, required_approvals: 6 })!, /quorum/);
  assert.match(pendingApprovalBlockReason({ ...action, exclude_requester: true })!, /Requester identity/);
});

test("managed approval copy retains matching connector retry and current-policy checks", () => {
  for (const expires_at of [undefined, null]) {
    const action = { ...current, expires_at, status: "approved", required_approvals: 1, approval_count: 1 };
    const feedback = pendingDecisionFeedback(action, "approve");
    assert.equal(feedback.title, "Approved");
    assert.match(feedback.description, /one matching connector operation.*original run.*current policy/);
    assert.match(feedback.description, /No tool was executed/);
    assert.match(pendingApprovalGuidance(action), /one matching connector retry.*original run.*current policy/);
    assert.match(approvalAuthorityDescription(action), /rechecks current authority before approval and dispatch/);
    assert.doesNotMatch(feedback.description, /does not consume approval|app owns continuation/);
  }
});

test("app-owned approval copy leaves continuation, execution deadlines and idempotency with the app", () => {
  const action = { ...current, expires_at: "2030-01-01T00:00:00Z", status: "approved", required_approvals: 2, approval_count: 2 };
  const feedback = pendingDecisionFeedback(action, "approve");
  assert.equal(feedback.title, "Approved");
  for (const description of [feedback.description, pendingApprovalGuidance(action)]) {
    assert.match(description, /exact saved proposal/);
    assert.match(description, /app owns continuation, execution deadlines and idempotency/);
    assert.match(description, /Polling does not consume approval/);
    assert.doesNotMatch(description, /one matching|subject to current policy|before approval and dispatch/);
  }
  const authority = approvalAuthorityDescription(action);
  assert.match(authority, /checks policy before recording approval/);
  assert.match(authority, /Polling checks the stored approval binding and current review requirements/);
  assert.match(authority, /does not recheck current tool grants or consume approval/);
  assert.doesNotMatch(authority, /before approval and dispatch/);
  const partial = pendingDecisionFeedback({ ...action, status: "pending", approval_count: 1 }, "approve");
  assert.equal(partial.title, "Review recorded");
  assert.match(partial.description, /remains pending/);
  assert.doesNotMatch(partial.description, /may continue|may retry|may submit/);
});
