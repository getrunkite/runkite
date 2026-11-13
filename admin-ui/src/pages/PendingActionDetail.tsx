import { useState } from "react";
import { Link, useParams } from "react-router";
import { toast } from "sonner";
import { api, ApiError } from "../api/client";
import { useApi } from "../api/useApi";
import type { AdminPendingAction } from "../api/types";
import { ApprovalPlan } from "../components/approval-plan";
import { ApprovalTargetRevision } from "../components/approval-target-revision";
import { EvidenceField } from "../components/connector-action-record";
import { ErrorState, formatTimestamp, PageHeader, StatusBadge } from "../components/common";
import { Button } from "../components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "../components/ui/card";
import { pendingApprovalBlockReason, pendingDecisionFeedback } from "../lib/pendingApproval";

export function PendingActionDetail() {
  const { pendingId } = useParams<{ pendingId: string }>();
  return pendingId ? <PendingRecord key={pendingId} id={pendingId} /> : <ErrorState message="No approval ID provided." />;
}
function PendingRecord({ id }: { id: string }) {
  const history = useApi<AdminPendingAction>(`/pending-actions/${encodeURIComponent(id)}`);
  const [busy, setBusy] = useState(false);
  const action = history.data;
  const expired = action?.expires_at != null && (!Number.isFinite(Date.parse(action.expires_at)) || Date.parse(action.expires_at) <= Date.now());
  const blocked = action ? (expired ? "Approval expired. Resubmit for fresh review." : pendingApprovalBlockReason(action)) : null;
  const resolve = async (verb: "approve" | "deny") => {
    setBusy(true);
    try {
      const updated = await api.post<AdminPendingAction>(`/pending-actions/${encodeURIComponent(id)}/${verb}`);
      const feedback = pendingDecisionFeedback(updated, verb);
      toast.success(feedback.title, { description: feedback.description });
      history.reload();
    } catch (err) { toast.error("Review could not be recorded", { description: err instanceof ApiError ? err.message : "Request failed." }); }
    finally { setBusy(false); }
  };
  return <div className="space-y-4">
    <Link to="/admin/pending" className="text-sm text-primary hover:underline">← Pending actions</Link>
    <PageHeader title="Approval record" subtitle={<code className="break-all text-xs">{id}</code>} actions={<Button variant="outline" disabled={history.loading || busy} onClick={history.reload}>Refresh</Button>} />
    {history.error && <ErrorState message={history.error} />}
    {history.loading && !action && <p className="text-sm text-muted-foreground">Loading approval...</p>}
    {action && <><Card><CardHeader><CardTitle>{action.connector}.{action.tool}</CardTitle></CardHeader><CardContent><dl className="space-y-3 text-sm">
      <EvidenceField label="Status"><StatusBadge status={action.status} /></EvidenceField>
      <EvidenceField label="Tenant">{action.tenant_id}</EvidenceField><EvidenceField label="Agent">{action.agent_id}</EvidenceField>
      <EvidenceField label="Run / operation"><code className="text-xs">{action.run_id}</code></EvidenceField>
      <EvidenceField label="Run instance"><code className="text-xs">{action.run_incarnation_id || "Standalone or legacy unbound record"}</code></EvidenceField>
      <EvidenceField label="Generation">{action.generation}</EvidenceField><EvidenceField label="Argument digest"><code className="text-xs">{action.args_digest || "Not recorded"}</code></EvidenceField>
      <EvidenceField label="Stored target"><ApprovalTargetRevision revision={action.target_revision} standalone={action.expires_at != null} /></EvidenceField>
      <EvidenceField label="Reason code">{action.reason_code || "Not recorded"}</EvidenceField><EvidenceField label="Created">{formatTimestamp(action.created_at)}</EvidenceField>
      <EvidenceField label="Expiry">{action.expires_at ? formatTimestamp(action.expires_at) : "No expiry (managed approval)"}</EvidenceField>
    </dl></CardContent></Card>
    <Card><CardHeader><CardTitle>Approval plan and history</CardTitle></CardHeader><CardContent><ApprovalPlan action={action} /></CardContent></Card>
    {action.status === "pending" && <div className="space-y-3"><p className="text-sm text-muted-foreground">{blocked || "Record one review for this operation. The caller remains pending until its quorum is reached."}</p><div className="flex gap-2"><Button disabled={busy || Boolean(blocked)} onClick={() => resolve("approve")}>Approve</Button><Button variant="outline" disabled={busy || expired} onClick={() => resolve("deny")}>Deny</Button></div></div>}
    </>}
  </div>;
}
