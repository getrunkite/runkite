import type { AdminPendingAction } from "../api/types";
import { approvalProgress } from "../lib/pendingApproval";
import { EvidenceField } from "./connector-action-record";
import { formatTimestamp } from "./common";

export function ApprovalProgress({ action }: { action: AdminPendingAction }) {
  return <span className="text-xs" title={action.exclude_requester ? "Distinct authenticated reviewers required. Requester cannot approve." : "Counted reviewers for this immutable operation."}>{approvalProgress(action).label}</span>;
}

export function ApprovalPlan({ action }: { action: AdminPendingAction }) {
  return <div className="space-y-4">
    <dl className="space-y-3 text-sm">
      <EvidenceField label="Requester">{action.requester_identity || "No authenticated requester recorded"}</EvidenceField>
      <EvidenceField label="Reviews"><ApprovalProgress action={action} /></EvidenceField>
      <EvidenceField label="Separation">{action.exclude_requester ? "Requester excluded from approval" : "Requester exclusion is not required"}</EvidenceField>
      <EvidenceField label="Policy revision"><code className="text-xs">{action.policy_revision || (action.expires_at != null ? "App-owned decision" : "Legacy unbound policy")}</code></EvidenceField>
      <EvidenceField label="Tool schema"><code className="text-xs">{action.tool_revision || "No tool schema pin recorded"}</code></EvidenceField>
    </dl>
    <p className="text-xs text-muted-foreground">Reviews count exact authenticated identities. A recorded schema revision does not pin the remote implementation. The server rechecks current authority before approval and dispatch.</p>
    <div><h3 className="mb-2 text-sm font-medium">Recorded reviews</h3>
      {!action.approvals?.length ? <p className="text-xs text-muted-foreground">No reviewer history recorded.</p> : <ol className="space-y-2">{action.approvals.map((vote) => <li key={vote.reviewer_identity} className="flex flex-wrap justify-between gap-2 rounded-sm border border-border p-2 text-xs"><code className="break-all">{vote.reviewer_identity || "Local single-reviewer mode (no identity)"}</code><time dateTime={vote.reviewed_at}>{formatTimestamp(vote.reviewed_at)}</time></li>)}</ol>}
    </div>
  </div>;
}
