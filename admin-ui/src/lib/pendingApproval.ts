interface PendingApprovalBinding {
  id: string;
  args_digest?: string;
  expires_at?: string | null;
  run_incarnation_id?: string;
  target_revision?: string;
}

export function validManagedTargetRevision(value?: string): boolean {
  return typeof value === "string" && value.length === 74 && /^target-v1-[0-9a-f]{64}$/.test(value);
}

/** Presentation mirrors the gate; the server still validates current authority. */
export function pendingApprovalBlockReason(action: PendingApprovalBinding): string | null {
  if (!action.args_digest || !action.id.startsWith("pending-v2-")) {
    return "Legacy argument binding. Resubmit the operation for fresh review.";
  }
  if (action.expires_at == null && !action.run_incarnation_id) {
    return "This managed approval has no original run identity. Resubmit the operation for fresh review.";
  }
  if (action.expires_at == null && !validManagedTargetRevision(action.target_revision)) {
    return "This managed approval has no supported connector target revision. Resubmit the operation for fresh review.";
  }
  return null;
}
