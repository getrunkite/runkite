interface PendingApprovalBinding {
  id: string;
  args_digest?: string;
  expires_at?: string | null;
  run_incarnation_id?: string;
}

/** Presentation mirrors the gate; the server still validates current authority. */
export function pendingApprovalBlockReason(action: PendingApprovalBinding): string | null {
  if (!action.args_digest || !action.id.startsWith("pending-v2-")) {
    return "Legacy argument binding. Resubmit the operation for fresh review.";
  }
  if (action.expires_at == null && !action.run_incarnation_id) {
    return "This managed approval has no original run identity. Resubmit the operation for fresh review.";
  }
  return null;
}
