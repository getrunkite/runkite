import { pendingApprovalBlockReason } from "../lib/pendingApproval.ts";
import type { ReviewIdentity, ReviewPendingAction } from "./contracts.ts";

export function reviewPendingControls(identity: ReviewIdentity, action: ReviewPendingAction, now = Date.now()) {
  const expired = action.expires_at !== undefined && (!Number.isFinite(Date.parse(action.expires_at)) || Date.parse(action.expires_at) <= now);
  const showActions = identity.can_approve && identity.tenant_id === action.tenant_id && action.status === "pending";
  return {
    showActions,
    expired,
    approveBlocked: expired ? "Approval expired. Resubmit the operation for review." : pendingApprovalBlockReason(action),
    denyBlocked: expired,
  };
}
