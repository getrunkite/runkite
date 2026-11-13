import { useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";
import type { ColumnDef } from "@tanstack/react-table";
import type { AdminConnectorAction } from "../api/types";
import { ConnectorActionRecord, EvidenceField } from "../components/connector-action-record";
import { ConnectorActionBadge, ConnectorEvidenceSetup, EvidenceRefresh } from "../components/connector-action-evidence";
import { DataTable } from "../components/data-table";
import { ListPager } from "../components/list-pager";
import { ErrorState, formatTimestamp, PageHeader, StatusBadge } from "../components/common";
import { Button } from "../components/ui/button";
import { Input } from "../components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../components/ui/select";
import { Card, CardContent, CardHeader, CardTitle } from "../components/ui/card";
import { CONNECTOR_ACTION_STATES, connectorActionState } from "../lib/connectorActions";
import { reviewClient } from "./client";
import { useReview } from "./context";
import { useReviewRead } from "./useReviewRead";
import { reviewPendingControls } from "./pendingControls";
import { REVIEW_ROOT, reviewDetailHref, reviewDetailPath, reviewListPath, reviewListScope, type ReviewActionDetail, type ReviewPendingAction } from "./contracts";

type Kind = "connector-actions" | "pending-actions";
type Row = AdminConnectorAction | ReviewPendingAction;

export function ReviewList({ kind }: { kind: Kind }) {
  const [params, setParams] = useSearchParams();
  const field = kind === "pending-actions" ? "status" : "state";
  const choices = kind === "pending-actions" ? ["pending", "approved", "denied", "consumed"] : [...CONNECTOR_ACTION_STATES];
  const update = (name: string, value: string) => {
    const next = new URLSearchParams();
    for (const key of ["run_id", "connector", field]) { const old = params.get(key); if (old) next.set(key, old); }
    if (value && value !== "all") next.set(name, value); else next.delete(name);
    setParams(next, { replace: true });
  };
  const scope = reviewListScope(kind, params);
  return <div className="space-y-4">
    <PageHeader title={kind === "pending-actions" ? "Approval review" : "Connector evidence"} subtitle={kind === "pending-actions" ? "Pending and historical decisions for your verified tenant. Approval does not execute a tool." : "Decisions, authority and observed responses for your verified tenant. A response is not proof of business success."} />
    <div className="flex flex-wrap items-center gap-3">
      <Input aria-label="Filter by original run or operation ID" placeholder="Run / operation ID" className="w-56" value={params.get("run_id") ?? ""} onChange={(event) => update("run_id", event.target.value)} />
      <Input aria-label="Filter by connector" placeholder="Connector" className="w-44" value={params.get("connector") ?? ""} onChange={(event) => update("connector", event.target.value)} />
      <Select value={params.get(field) || "all"} onValueChange={(value) => update(field, value)}><SelectTrigger aria-label={`Filter by ${field}`} className="w-52"><SelectValue /></SelectTrigger><SelectContent><SelectItem value="all">All {field === "state" ? "evidence states" : "statuses"}</SelectItem>{choices.map((value) => <SelectItem key={value} value={value}>{field === "state" ? connectorActionState(value).label : value}</SelectItem>)}</SelectContent></Select>
      <Button size="sm" variant="ghost" onClick={() => setParams({})}>Clear filters</Button>
    </div>
    <ReviewResults key={scope} kind={kind} params={params} />
  </div>;
}

function ReviewResults({ kind, params }: { kind: Kind; params: URLSearchParams }) {
  const [cursors, setCursors] = useState<string[]>([]);
  const cursor = cursors.at(-1);
  const history = useReviewRead<Row[]>(reviewListPath(kind, params, cursor), !cursor);
  const navigate = useNavigate();
  const isAction = kind === "connector-actions";
  const id = (row: Row) => isAction ? (row as AdminConnectorAction).action_id : (row as ReviewPendingAction).id;
  const columns: ColumnDef<Row, unknown>[] = [
    { id: "identity", header: isAction ? "Action ID" : "Approval ID", cell: ({ row }) => <Link className="break-all font-mono text-xs text-primary hover:underline" to={reviewDetailHref(kind, id(row.original))}>{id(row.original)}</Link> },
    { accessorKey: "created_at", header: "Recorded", cell: ({ getValue }) => <span className="text-xs">{formatTimestamp(getValue() as string)}</span> },
    { id: "operation", header: "Connector / tool", cell: ({ row }) => <span className="font-mono text-xs">{row.original.connector}.{row.original.tool}</span> },
    { id: "status", header: isAction ? "Evidence" : "Status", cell: ({ row }) => isAction ? <ConnectorActionBadge state={(row.original as AdminConnectorAction).state} /> : <PendingStatus action={row.original as ReviewPendingAction} /> },
    { accessorKey: "run_id", header: "Original run / operation", cell: ({ getValue }) => <code className="text-xs">{getValue() as string}</code> },
    { accessorKey: "reason_code", header: "Reason code", cell: ({ getValue }) => <code className="text-xs">{getValue() as string || "Not recorded"}</code> },
  ];
  return <div className="space-y-4">
    <div className="flex items-center justify-between gap-3"><p className="text-xs text-muted-foreground">First page refreshes every 8s for up to 2 minutes. Refresh restarts read-only updates.</p><EvidenceRefresh refresh={history.reload} loading={history.loading} /></div>
    {history.error && <ErrorState message={history.error} />}
    {history.data?.length === 0 && <div className="space-y-2 rounded-sm border border-border p-4"><p>No matching retained {isAction ? "connector actions" : "approvals"}. This does not establish that no tools ran.</p>{isAction && <ConnectorEvidenceSetup />}</div>}
    {(history.loading || Boolean(history.data?.length)) && <DataTable columns={columns} data={history.data ?? []} loading={history.loading} getRowId={id} onRowClick={(row) => navigate(reviewDetailHref(kind, id(row)))} />}
    <ListPager pageIndex={cursors.length} pageCount={history.data?.length ?? 0} hasNext={Boolean(history.nextCursor)} onPrev={() => setCursors((old) => old.slice(0, -1))} onNext={() => { if (history.nextCursor) setCursors((old) => [...old, history.nextCursor!]); }} />
  </div>;
}

function PendingStatus({ action }: { action: ReviewPendingAction }) {
  const { identity } = useReview();
  if (!identity) return null;
  const controls = reviewPendingControls(identity, action);
  return <div className="space-y-1"><StatusBadge status={action.status} />{controls.expired && <p className="text-xs text-amber-600 dark:text-amber-400">Expired</p>}{!controls.expired && controls.approveBlocked && ["pending", "approved"].includes(action.status) && <p className="text-xs text-amber-600 dark:text-amber-400" title={controls.approveBlocked}>Fresh review required</p>}</div>;
}

export function ReviewEvidenceDetail() {
  const { actionId } = useParams<{ actionId: string }>();
  return actionId ? <EvidenceDetail key={actionId} id={actionId} /> : <ErrorState message="No action ID provided." />;
}
function EvidenceDetail({ id }: { id: string }) {
  const history = useReviewRead<ReviewActionDetail>(reviewDetailPath("connector-actions", id));
  return <div className="space-y-4">
    <Link to={REVIEW_ROOT} className="text-sm text-primary hover:underline">← Connector evidence</Link>
    <PageHeader title="Connector action" subtitle={<code className="break-all text-xs">{id}</code>} actions={<EvidenceRefresh refresh={history.reload} loading={history.loading} />} />
    {history.error && <ErrorState message={history.error} />}
    {history.loading && !history.data && <p className="text-sm text-muted-foreground">Loading evidence...</p>}
    {history.data && <ConnectorActionRecord data={history.data} review pendingHref={history.data.action.pending_action_id ? reviewDetailHref("pending-actions", history.data.action.pending_action_id) : `${REVIEW_ROOT}/pending`} />}
  </div>;
}

export function ReviewPendingDetail() {
  const { pendingId } = useParams<{ pendingId: string }>();
  return pendingId ? <PendingDetail key={pendingId} id={pendingId} /> : <ErrorState message="No approval ID provided." />;
}
function PendingDetail({ id }: { id: string }) {
  const { identity } = useReview();
  const history = useReviewRead<ReviewPendingAction>(reviewDetailPath("pending-actions", id));
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<string | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const mutation = useRef<AbortController | null>(null);
  useEffect(() => () => mutation.current?.abort(), []);
  const action = history.data;
  if (!identity) return null;
  const controls = action ? reviewPendingControls(identity, action) : null;
  const resolve = async (verb: "approve" | "deny") => {
    if (!action) return;
    mutation.current?.abort();
    mutation.current = new AbortController();
    setBusy(true); setFailure(null); setResult(null);
    try { await reviewClient.resolve(action, verb, mutation.current.signal); setResult(verb === "approve" ? "Approved. The caller must submit its matching operation; no tool was executed here." : "Denied. This decision does not undo any external effects."); history.reload(); }
    catch (err) { setFailure(err instanceof Error ? err.message : "Review decision failed."); }
    finally { setBusy(false); }
  };
  return <div className="space-y-4">
    <Link to={`${REVIEW_ROOT}/pending`} className="text-sm text-primary hover:underline">← Approval review</Link>
    <PageHeader title="Approval record" subtitle={<code className="break-all text-xs">{id}</code>} actions={<EvidenceRefresh refresh={history.reload} loading={history.loading || busy} />} />
    {(history.error || failure) && <ErrorState message={failure || history.error!} />}{result && <p role="status" className="rounded-sm border border-border p-3 text-sm">{result}</p>}
    {history.loading && !action && <p className="text-sm text-muted-foreground">Loading approval...</p>}
    {action && <><Card><CardHeader><CardTitle>{action.connector}.{action.tool}</CardTitle></CardHeader><CardContent><dl className="space-y-3 text-sm">
      <EvidenceField label="Status"><PendingStatus action={action} /></EvidenceField>
      <EvidenceField label="Tenant">{action.tenant_id}</EvidenceField><EvidenceField label="Agent">{action.agent_id}</EvidenceField><EvidenceField label="Run / operation"><code>{action.run_id}</code></EvidenceField><EvidenceField label="Run instance"><code>{action.run_incarnation_id || "Standalone or legacy unbound record"}</code></EvidenceField><EvidenceField label="Generation">{action.generation}</EvidenceField><EvidenceField label="Argument digest"><code>{action.args_digest || "Not recorded"}</code></EvidenceField><EvidenceField label="Reason code">{action.reason_code || "Not recorded"}</EvidenceField><EvidenceField label="Rule">{action.rule_id || "Not recorded"}</EvidenceField><EvidenceField label="Reviewer">{action.decided_by || "Not decided"}</EvidenceField><EvidenceField label="Expires">{action.expires_at ? formatTimestamp(action.expires_at) : "No expiry (managed approval)"}</EvidenceField>
    </dl></CardContent></Card>
    {identity.can_approve && action.args !== undefined && <Card><CardHeader><CardTitle>Capped argument display</CardTitle></CardHeader><CardContent><p className="mb-2 text-xs text-muted-foreground">Existing redacted review metadata, not the raw argument payload.</p><pre className="overflow-auto whitespace-pre-wrap break-all text-xs">{JSON.stringify(action.args, null, 2)}</pre></CardContent></Card>}
    {!identity.can_approve && <p className="text-sm text-muted-foreground">Your role can inspect approval metadata. Argument display and decision controls require governance:approve.</p>}
    {controls?.showActions && <div className="space-y-3"><p className="text-sm text-muted-foreground">{controls.approveBlocked || "Approval permits one matching operation subject to current policy. It does not execute or retry a tool."}</p><div className="flex gap-2"><Button disabled={busy || Boolean(controls.approveBlocked)} onClick={() => resolve("approve")}>Approve</Button><Button variant="outline" disabled={busy || controls.denyBlocked} onClick={() => resolve("deny")}>Deny</Button></div></div>}
    </>}
  </div>;
}
