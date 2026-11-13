import { Link, useParams } from "react-router";
import type { CredentialLease } from "../api/types";
import { CredentialLeaseRecord } from "../components/credential-lease-record";
import { ErrorState, PageHeader } from "../components/common";
import { EvidenceRefresh } from "../components/connector-action-evidence";
import { Badge } from "../components/ui/badge";
import { credentialLeaseHref, reviewCredentialLeasePath, leaseStateLabel, leaseTime } from "../lib/securityRecords";
import { useReviewRead } from "./useReviewRead";
import { useReview } from "./context";
import { REVIEW_ROOT } from "./contracts";

export function ReviewCredentialLeases() {
  const history = useReviewRead<CredentialLease[]>(reviewCredentialLeasePath());
  const { identity } = useReview();
  return <div className="space-y-4"><PageHeader title="Credential lease metadata" subtitle="Read-only lifecycle evidence for your verified tenant. Review does not reveal credential values or authorize renewal and revocation." actions={<EvidenceRefresh refresh={history.reload} loading={history.loading} />} />
    {history.error && <ErrorState message={history.error} />}
    {history.loading && !history.data && <p className="text-sm text-muted-foreground">Loading credential metadata...</p>}
    {history.data && <div className="space-y-3">{history.data.length === 0 && <p className="text-sm text-muted-foreground">No lease records returned for your verified tenant.</p>}{history.data.map((record) => {
      const href = credentialLeaseHref(record.lease_ref, identity?.tenant_id ?? "", true);
      return <div key={record.lease_ref} className="rounded-sm border border-border p-4"><div className="flex flex-wrap items-center justify-between gap-3">{href ? <Link to={href} className="break-all font-mono text-xs text-primary hover:underline">{record.lease_ref}</Link> : <code className="text-xs">{record.lease_ref}</code>}<Badge variant="outline">{leaseStateLabel(record.state)}</Badge></div><p className="mt-2 text-xs text-muted-foreground">Binding {record.binding_id} · Attempt {record.owner.generation} · Expires {leaseTime(record.expires_at)}</p></div>;
    })}{history.data.length === 100 && <p role="status" className="text-xs text-muted-foreground">Only the first 100 records were returned. Ask the operator for older lifecycle evidence.</p>}</div>}
  </div>;
}
export function ReviewCredentialLeaseDetail() {
  const { leaseRef } = useParams<{ leaseRef: string }>();
  return leaseRef ? <LeaseDetail key={leaseRef} id={leaseRef} /> : <ErrorState message="No lease reference was provided." />;
}
function LeaseDetail({ id }: { id: string }) {
  const history = useReviewRead<CredentialLease>(reviewCredentialLeasePath(id));
  const mismatch = history.data && history.data.lease_ref !== id;
  return <div className="space-y-4"><Link to={`${REVIEW_ROOT}/credential-leases`} className="text-sm text-primary hover:underline">← Credential lease metadata</Link><PageHeader title="Credential lifecycle" subtitle={<code className="break-all text-xs">{id}</code>} actions={<EvidenceRefresh refresh={history.reload} loading={history.loading} />} />
    {(history.error || mismatch) && <ErrorState message={mismatch ? "The response did not match this lease. No metadata was displayed." : history.error!} />}
    {history.loading && !history.data && <p className="text-sm text-muted-foreground">Loading credential metadata...</p>}
    {history.data && !mismatch && <CredentialLeaseRecord record={history.data} review />}
  </div>;
}
