import { useEffect, useState, type FormEvent } from "react";
import { Link, useSearchParams } from "react-router";
import { toast } from "sonner";
import { api, ApiError } from "../api/client";
import { useApi } from "../api/useApi";
import type { ActionDelivery } from "../api/types";
import { ErrorState, formatTimestamp, PageHeader } from "../components/common";
import { Button } from "../components/ui/button";
import { Input } from "../components/ui/input";
import { Badge } from "../components/ui/badge";
import { DELIVERY_STATUSES, actionDeliveriesPath, actionDeliveryRetryPath } from "../lib/securityRecords";

export function ActionDeliveries() {
  const [params, setParams] = useSearchParams();
  const tenant = (params.get("tenant_id") ?? "").trim();
  const status = params.get("status") ?? "";
  const sink = params.get("sink_id") ?? "";
  const rawOffset = params.get("offset") ?? "0";
  const offset = /^\d+$/.test(rawOffset) ? Number(rawOffset) : -1;
  const [enteredTenant, setEnteredTenant] = useState(tenant);
  const [enteredSink, setEnteredSink] = useState(sink);
  const [enteredStatus, setEnteredStatus] = useState(status);
  useEffect(() => { setEnteredTenant(tenant); setEnteredSink(sink); setEnteredStatus(status); }, [tenant, sink, status]);
  const submit = (event: FormEvent) => { event.preventDefault(); const next = new URLSearchParams(); if (enteredTenant.trim()) next.set("tenant_id", enteredTenant.trim()); if (enteredSink.trim()) next.set("sink_id", enteredSink.trim()); if (enteredStatus) next.set("status", enteredStatus); setParams(next); };
  const page = (next: number) => { const query = new URLSearchParams(params); query.set("offset", String(next)); setParams(query); };
  let path = "", invalid = false;
  if (tenant) { try { path = actionDeliveriesPath(tenant, status, sink, offset); } catch { invalid = true; } }
  return <div className="space-y-4"><PageHeader title="Action evidence deliveries" subtitle="Durable exports of existing action metadata. Delivery retries resend evidence over HTTP and never execute or retry a tool." />
    <p className="text-xs text-muted-foreground">Delivery is at least once. A lost acknowledgement can resend the same delivery ID; receivers must deduplicate it. Events can arrive out of order.</p>
    <form onSubmit={submit} className="flex flex-wrap items-center gap-3"><Input aria-label="Tenant ID" className="w-52" placeholder="Tenant ID" value={enteredTenant} onChange={(event) => setEnteredTenant(event.target.value)} /><Input aria-label="Sink ID" className="w-52" placeholder="Sink ID (optional)" value={enteredSink} onChange={(event) => setEnteredSink(event.target.value)} /><select aria-label="Delivery status" className="h-9 rounded-sm border border-input bg-background px-3 text-sm" value={enteredStatus} onChange={(event) => setEnteredStatus(event.target.value)}><option value="">All delivery states</option>{DELIVERY_STATUSES.map((value) => <option key={value} value={value}>{value.replaceAll("_", " ")}</option>)}</select><Button disabled={!enteredTenant.trim()}>Load tenant</Button></form>
    {invalid ? <ErrorState message="Unsupported status or offset. Load a tenant with valid filters." /> : path ? <DeliveryResults key={path} path={path} tenant={tenant} offset={offset} page={page} /> : <p className="text-sm text-muted-foreground">No tenant selected. No evidence deliveries have been requested.</p>}
  </div>;
}
function DeliveryResults({ path, tenant, offset, page }: { path: string; tenant: string; offset: number; page: (next: number) => void }) {
  const history = useApi<ActionDelivery[]>(path);
  const [busy, setBusy] = useState<string | null>(null);
  const rows = history.data;
  const wrongScope = rows?.some((row) => row.tenant_id !== tenant);
  const retry = async (row: ActionDelivery) => { setBusy(row.id); try { await api.post(actionDeliveryRetryPath(tenant, row.id)); toast.success("Evidence delivery queued", { description: "The existing delivery ID is queued for its configured sink. No tool was executed or retried." }); history.reload(); } catch (err) { toast.error("Delivery retry could not be recorded", { description: err instanceof ApiError ? err.message : "Request failed." }); } finally { setBusy(null); } };
  return <div className="space-y-4"><div className="flex items-center justify-between gap-3"><p className="text-xs text-muted-foreground">Tenant: {tenant} · Offset {offset}. Paging can shift while deliveries update.</p><Button variant="outline" disabled={history.loading || Boolean(busy)} onClick={history.reload}>Refresh</Button></div>
    {(history.error || wrongScope) && <ErrorState message={wrongScope ? "The response did not match the selected tenant. No evidence was displayed." : history.error!} />}
    {history.loading && !rows && <p className="text-sm text-muted-foreground">Loading delivery metadata...</p>}
    {!wrongScope && rows && <>{rows.length === 0 && <p className="text-sm text-muted-foreground">No matching deliveries returned. Export must be configured before new action events enter the outbox.</p>}<div className="space-y-3">{rows.map((row) => <div key={row.id} className="space-y-2 rounded-sm border border-border p-4"><div className="flex flex-wrap items-center justify-between gap-3"><code className="break-all text-xs">{row.id}</code><Badge variant="outline">{row.status.replaceAll("_", " ")}</Badge></div><p className="text-xs">Sink <code>{row.sink_id}</code> · Event #{row.sequence} · {row.attempts} delivery attempts</p><p className="text-xs">Action <Link to={`/admin/connector-actions/${encodeURIComponent(row.action_id)}`} className="break-all font-mono text-primary hover:underline">{row.action_id}</Link></p><p className="text-xs text-muted-foreground">Available {formatTimestamp(row.available_at)} · Updated {formatTimestamp(row.updated_at)}</p>{row.last_reason_code && <p className="text-xs">Reason code: <code>{row.last_reason_code}</code></p>}{row.claim_until && row.status === "pending" && <p className="text-xs text-muted-foreground">Worker claim until {formatTimestamp(row.claim_until)}. A claim does not establish receiver acceptance.</p>}{row.status === "dead_letter" && <Button variant="outline" size="sm" disabled={Boolean(busy)} onClick={() => retry(row)}>{busy === row.id ? "Queuing..." : "Retry evidence delivery"}</Button>}</div>)}</div><div className="flex items-center justify-between"><Button variant="outline" disabled={offset === 0 || history.loading} onClick={() => page(Math.max(0, offset - 50))}>Previous</Button><Button variant="outline" disabled={rows.length < 50 || offset + 50 > 10000 || history.loading} onClick={() => page(offset + 50)}>Next</Button></div></>}
  </div>;
}
