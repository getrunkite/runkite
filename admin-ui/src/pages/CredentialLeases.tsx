import { useEffect, useState, type FormEvent } from "react";
import { Link, useParams, useSearchParams } from "react-router";
import { toast } from "sonner";
import { api, ApiError } from "../api/client";
import { useApi } from "../api/useApi";
import type { CredentialLease } from "../api/types";
import { CredentialLeaseRecord } from "../components/credential-lease-record";
import { ErrorState, PageHeader } from "../components/common";
import { Button } from "../components/ui/button";
import { Input } from "../components/ui/input";
import { Badge } from "../components/ui/badge";
import { credentialLeaseHref, credentialLeasePath, leaseControls, leaseStateLabel, leaseTime } from "../lib/securityRecords";

export function CredentialLeases() {
  const [params, setParams] = useSearchParams();
  const tenant = (params.get("tenant_id") ?? "").trim();
  const [enteredTenant, setEnteredTenant] = useState(tenant);
  useEffect(() => setEnteredTenant(tenant), [tenant]);
  const submit = (event: FormEvent) => { event.preventDefault(); setParams(enteredTenant.trim() ? { tenant_id: enteredTenant.trim() } : {}); };
  return <div className="space-y-4">
    <PageHeader title="Credential leases" subtitle="Short-lived externally issued database credential metadata. Select an explicit tenant to inspect lifecycle state." />
    <form onSubmit={submit} className="flex flex-wrap gap-3"><Input aria-label="Tenant ID" className="w-64" placeholder="Tenant ID" value={enteredTenant} onChange={(event) => setEnteredTenant(event.target.value)} /><Button type="submit" disabled={!enteredTenant.trim()}>Load tenant</Button></form>
    {tenant ? <LeaseResults key={tenant} tenant={tenant} /> : <p className="text-sm text-muted-foreground">No tenant selected. No credential records have been requested.</p>}
  </div>;
}
function LeaseResults({ tenant }: { tenant: string }) {
  const history = useApi<CredentialLease[]>(credentialLeasePath(tenant));
  const rows = history.data;
  const wrongScope = rows?.some((record) => record.owner?.tenant_id !== tenant);
  return <div className="space-y-4">
    <div className="flex items-center justify-between gap-3"><p className="text-xs text-muted-foreground">Tenant: {tenant}. Up to 100 records; this view does not claim complete historical pagination.</p><Button variant="outline" disabled={history.loading} onClick={history.reload}>Refresh</Button></div>
    {(history.error || wrongScope) && <ErrorState message={wrongScope ? "The response did not match the requested tenant. No records were displayed." : history.error!} />}
    {history.loading && !rows && <p className="text-sm text-muted-foreground">Loading lease metadata...</p>}
    {!wrongScope && rows && <div className="space-y-3">{rows.length === 0 && <p className="text-sm text-muted-foreground">No credential leases returned. This feature requires a configured trusted Vault database consumer.</p>}{rows.map((record) => {
      const href = credentialLeaseHref(record.lease_ref, tenant);
      return <div key={record.lease_ref} className="rounded-sm border border-border p-4"><div className="flex flex-wrap items-center justify-between gap-3">{href ? <Link to={href} className="break-all font-mono text-xs text-primary hover:underline">{record.lease_ref}</Link> : <code className="break-all text-xs">{record.lease_ref}</code>}<Badge variant="outline">{leaseStateLabel(record.state)}</Badge></div><p className="mt-2 text-xs text-muted-foreground">Binding {record.binding_id} · Attempt {record.owner.generation} · Expires {leaseTime(record.expires_at)}</p>{record.cleanup_requested && <p className="mt-1 text-xs text-amber-600 dark:text-amber-400">Cleanup requested. Inspect the provider-confirmed state.</p>}</div>;
    })}{rows.length === 100 && <p role="status" className="text-sm text-muted-foreground">The response reached its 100-record cap. Use the API or operator tooling for older records.</p>}</div>}
  </div>;
}

export function CredentialLeaseDetail() {
  const { leaseRef } = useParams<{ leaseRef: string }>();
  const [params] = useSearchParams();
  const tenant = (params.get("tenant_id") ?? "").trim();
  return leaseRef && tenant ? <LeaseDetail key={`${tenant}/${leaseRef}`} tenant={tenant} id={leaseRef} /> : <ErrorState message="A lease reference and explicit tenant are required. Open Credential leases and select a tenant." />;
}
function LeaseDetail({ tenant, id }: { tenant: string; id: string }) {
  const history = useApi<CredentialLease>(credentialLeasePath(tenant, id));
  const [busy, setBusy] = useState(false);
  const record = history.data;
  const wrongScope = record && (record.owner?.tenant_id !== tenant || record.lease_ref !== id);
  const controls = record ? leaseControls(record) : null;
  const operate = async (verb: "renew" | "revoke") => {
    setBusy(true);
    try {
      const updated = await api.post<CredentialLease>(credentialLeasePath(tenant, id, verb));
      toast.success(verb === "renew" ? "Renewal recorded" : updated?.state === "revoked" ? "Lease revoked" : "Revocation request recorded", { description: "Refresh the record to inspect provider-confirmed state. No agent operation was replayed." });
      history.reload();
    } catch (err) { toast.error(`${verb === "renew" ? "Renewal" : "Revocation"} could not be confirmed`, { description: err instanceof ApiError ? err.message : "Inspect current lease state before retrying." }); history.reload(); }
    finally { setBusy(false); }
  };
  return <div className="space-y-4"><Link to={`/admin/credential-leases?${new URLSearchParams({ tenant_id: tenant })}`} className="text-sm text-primary hover:underline">← Credential leases</Link>
    <PageHeader title="Credential lifecycle" subtitle={<code className="break-all text-xs">{id}</code>} actions={<Button variant="outline" disabled={history.loading || busy} onClick={history.reload}>Refresh</Button>} />
    {(history.error || wrongScope) && <ErrorState message={wrongScope ? "The response did not match this tenant and lease. No metadata was displayed." : history.error!} />}
    {history.loading && !record && <p className="text-sm text-muted-foreground">Loading lease metadata...</p>}
    {record && !wrongScope && <><CredentialLeaseRecord record={record} /><div className="flex flex-wrap gap-2"><Button variant="outline" disabled={busy || !controls?.canRenew} onClick={() => operate("renew")}>Renew lease</Button><Button variant="destructive" disabled={busy || !controls?.canRevoke} onClick={() => operate("revoke")}>Revoke lease</Button></div><p className="text-xs text-muted-foreground">Renewal requires the current owner, an active renewable lease and remaining authority. Revocation requests cleanup for this exact original lease, including after its run ends.</p></>}
  </div>;
}
