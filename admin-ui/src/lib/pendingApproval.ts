export interface PendingApprovalBinding {
  id: string;
  status?: string;
  args_digest?: string;
  expires_at?: string | null;
  run_incarnation_id?: string;
  target_revision?: string;
  policy_revision?: string;
  tool_revision?: string;
  requester_identity?: string;
  required_approvals?: number;
  approval_count?: number;
  exclude_requester?: boolean;
  approvals?: { reviewer_identity: string; reviewed_at: string }[];
}

export function validManagedTargetRevision(value?: string): boolean {
  return typeof value === "string" && value.length === 74 && /^target-v[12]-[0-9a-f]{64}$/.test(value);
}

export function approvalProgress(action: PendingApprovalBinding) {
  const required = Number.isInteger(action.required_approvals) && action.required_approvals! >= 1 && action.required_approvals! <= 5 ? action.required_approvals! : 1;
  const count = Number.isInteger(action.approval_count) && action.approval_count! >= 0 ? action.approval_count! : 0;
  return { required, count, label: `${count}/${required} reviews` };
}

/** Presentation mirrors static gates; the server validates current authority. */
export function pendingApprovalBlockReason(action: PendingApprovalBinding): string | null {
  if (!action.args_digest || !action.id.startsWith("pending-v2-")) return "Legacy argument binding. Resubmit the operation for fresh review.";
  if (action.required_approvals !== undefined && (!Number.isInteger(action.required_approvals) || action.required_approvals < 1 || action.required_approvals > 5)) return "Unsupported approval quorum. Resubmit the operation for fresh review.";
  if (action.exclude_requester && !action.requester_identity?.trim()) return "Requester identity is unavailable for required separation. Resubmit for fresh review.";
  if (action.expires_at == null) {
    if (!action.run_incarnation_id) return "This managed approval has no original run identity. Resubmit the operation for fresh review.";
    if (!validManagedTargetRevision(action.target_revision)) return "This managed approval has no supported connector target revision. Resubmit the operation for fresh review.";
    if (typeof action.policy_revision !== "string" || action.policy_revision.length !== 74 || !/^policy-v1-[0-9a-f]{64}$/.test(action.policy_revision)) return "This managed approval has no supported policy revision. Resubmit the operation for fresh review.";
    if (action.tool_revision && (action.tool_revision.length !== 72 || !/^tool-v1-[0-9a-f]{64}$/.test(action.tool_revision))) return "This managed approval has an unsupported tool schema revision. Resubmit the operation for fresh review.";
  }
  return null;
}

export function pendingDecisionFeedback(action: PendingApprovalBinding, verb: "approve" | "deny") {
  if (verb === "deny") return { title: "Denied", description: "This operation remains blocked. No external effect was undone." };
  const { count, required } = approvalProgress(action);
  if (action.status === "approved" && count >= required) return { title: "Approved", description: "The required reviews are recorded. The caller may submit its matching operation, subject to current policy. No tool was executed here." };
  if (action.status === "pending") return { title: "Review recorded", description: `${count} of ${required} required reviews are recorded. The caller remains pending.` };
  return { title: "Decision recorded", description: "Refresh the operation to inspect its current authority. No tool was executed here." };
}
