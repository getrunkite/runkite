import { pendingApprovalBlockReason } from "../lib/pendingApproval.ts";
import type { ReviewIdentity, ReviewPendingAction } from "./contracts.ts";

export function reviewPendingControls(identity: ReviewIdentity, action: ReviewPendingAction, now = Date.now()) {
  const expired = action.expires_at != null && (!Number.isFinite(Date.parse(action.expires_at)) || Date.parse(action.expires_at) <= now);
  const showActions = identity.can_approve && identity.tenant_id === action.tenant_id && action.status === "pending";
  return {
    showActions,
    expired,
    approveBlocked: expired ? "Approval expired. Resubmit the operation for review." : pendingApprovalBlockReason(action) || (action.exclude_requester && action.requester_identity === identity.identity ? "The requester cannot approve this operation." : action.approvals?.some((vote) => vote.reviewer_identity === identity.identity) ? "Your review is already recorded." : null),
    denyBlocked: expired,
  };
}
