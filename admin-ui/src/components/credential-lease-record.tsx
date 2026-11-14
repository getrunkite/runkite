import { Link } from "react-router";
import type { CredentialLease } from "../api/types";
import { EvidenceField } from "./connector-action-record";
import { leaseStateLabel, leaseTime } from "../lib/securityRecords";
import { Badge } from "./ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "./ui/card";

export function CredentialLeaseRecord({ record, review = false }: { record: CredentialLease; review?: boolean }) {
  const actionHref = `${review ? "/admin/review" : "/admin"}/connector-actions/${encodeURIComponent(record.owner.action_id)}`;
  return <Card><CardHeader><CardTitle className="flex flex-wrap items-center gap-3">Credential lease <Badge variant="outline">{leaseStateLabel(record.state)}</Badge></CardTitle></CardHeader><CardContent className="space-y-4">
    <p className="text-sm text-muted-foreground">Provider-confirmed lifecycle metadata. Credential values and protected cleanup handles are not displayed. Unknown states require investigation and do not prove revocation or non-issuance.</p>
    <dl className="space-y-3 text-sm">
      <EvidenceField label="Lease reference"><code className="text-xs">{record.lease_ref}</code></EvidenceField>
      <EvidenceField label="Tenant">{record.owner.tenant_id}</EvidenceField><EvidenceField label="Binding">{record.binding_id}</EvidenceField><EvidenceField label="Binding revision"><code className="text-xs">{record.binding_revision}</code></EvidenceField>
      <EvidenceField label="Action"><Link to={actionHref} className="font-mono text-xs text-primary hover:underline">{record.owner.action_id}</Link></EvidenceField>
      <EvidenceField label="Original run"><code className="text-xs">{record.owner.run_id}</code></EvidenceField><EvidenceField label="Run instance"><code className="text-xs">{record.owner.run_incarnation_id}</code></EvidenceField><EvidenceField label="Generation">{record.owner.generation}</EvidenceField><EvidenceField label="Trusted consumer">{record.owner.consumer_id}</EvidenceField>
      <EvidenceField label="Expires">{leaseTime(record.expires_at)}</EvidenceField><EvidenceField label="Granted TTL">{record.granted_ttl_seconds} seconds</EvidenceField><EvidenceField label="Renewable">{record.renewable ? "Yes, subject to current owner authority" : "No"}</EvidenceField>
      <EvidenceField label="Cleanup requested">{record.cleanup_requested ? "Yes. Inspect provider-confirmed state." : "No"}</EvidenceField><EvidenceField label="Use started">{leaseTime(record.use_started_at)}</EvidenceField><EvidenceField label="Operation deadline">{leaseTime(record.operation_deadline)}</EvidenceField>
      <EvidenceField label="Reason code"><code className="text-xs">{record.reason_code || "Not recorded"}</code></EvidenceField><EvidenceField label="Version">{record.version}</EvidenceField><EvidenceField label="Created">{leaseTime(record.created_at)}</EvidenceField><EvidenceField label="Updated">{leaseTime(record.updated_at)}</EvidenceField>
    </dl>
    <p className="text-xs text-muted-foreground">This record remains tied to the original run instance and attempt. Revocation does not undo external effects or terminate every existing database session.</p>
  </CardContent></Card>;
}
