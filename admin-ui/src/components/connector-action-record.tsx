import type { ReactNode } from "react";
import { Link } from "react-router";
import type { AdminConnectorAction, AdminConnectorActionEvent } from "../api/types";
import { ConnectorActionBadge } from "./connector-action-evidence";
import { formatTimestamp, StatusBadge } from "./common";
import { credentialLeaseHref } from "../lib/securityRecords";
import { connectorActionState, connectorAuthority } from "../lib/connectorActions";
import { Badge } from "./ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "./ui/card";

export function EvidenceField({ label, children }: { label: string; children?: ReactNode }) {
  return (
    <div className="grid gap-1 sm:grid-cols-[9rem_minmax(0,1fr)] sm:gap-3">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0 break-all">{children ?? "Not recorded"}</dd>
    </div>
  );
}

export function ConnectorActionRecord({ data, runHref = null, pendingHref, review = false }: {
  data: { action: AdminConnectorAction; events: AdminConnectorActionEvent[]; events_truncated?: boolean };
  runHref?: string | null;
  pendingHref: string;
  review?: boolean;
}) {
  const { action, events } = data;
  const visibleRunHref = review ? null : runHref;
  const leaseHref = credentialLeaseHref(action.credential_ref, action.tenant_id, review);
  return <div className="space-y-4">
      <div className="rounded-sm border border-border p-4">
        <div className="flex flex-wrap items-center gap-3">
          <ConnectorActionBadge state={action.state} />
          <span className="font-mono text-sm">
            {action.connector}.{action.tool}
          </span>
          <Badge variant="outline">Attempt {action.generation}</Badge>
        </div>
        <p className="mt-2 text-sm text-muted-foreground">{connectorActionState(action.state).description}</p>
        <p className="mt-1 text-xs text-muted-foreground">
          This page shows control-plane observations. It does not execute or retry tools.
        </p>
      </div>
      <div className="grid gap-4 xl:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Recorded identity</CardTitle>
          </CardHeader>
          <CardContent>
            <dl className="space-y-3 text-sm">
              <EvidenceField label="Tenant">{action.tenant_id}</EvidenceField>
              <EvidenceField label="Principal">{action.principal || "No principal recorded"}</EvidenceField>
              <EvidenceField label="Agent">
                {action.agent_id}{" "}
                <span className="text-muted-foreground">
                  {action.agent_version > 0 ? `v${action.agent_version}` : "version unavailable"}
                </span>
              </EvidenceField>
              <EvidenceField label="Original run">
                {visibleRunHref ? (
                  <Link to={visibleRunHref} className="font-mono text-xs text-primary hover:underline">
                    {action.run_id}
                  </Link>
                ) : (
                  <>
                    <code className="text-xs">{action.run_id}</code>
                    <p className="mt-1 text-xs text-muted-foreground">
                      {review ? "Conversation navigation is outside this Review workspace." : "Original run unavailable. This record does not link to a replacement run with the same ID."}
                    </p>
                  </>
                )}
              </EvidenceField>
              <EvidenceField label="Run instance">
                <code className="text-xs">{action.run_incarnation_id}</code>
              </EvidenceField>
              <EvidenceField label="Run created">{formatTimestamp(action.run_created_at)}</EvidenceField>
              <EvidenceField label="Original thread">
                <code className="text-xs">{action.thread_id}</code>
              </EvidenceField>
              <EvidenceField label="Recorded">{formatTimestamp(action.created_at)}</EvidenceField>
              <EvidenceField label="Last update">{formatTimestamp(action.updated_at)}</EvidenceField>
            </dl>
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Decision and final authority</CardTitle>
          </CardHeader>
          <CardContent>
            <dl className="space-y-3 text-sm">
              <EvidenceField label="Recorded decision">
                {action.decision ? <StatusBadge status={action.decision} /> : "No decision recorded"}
              </EvidenceField>
              <EvidenceField label="Reason code">
                <code className="text-xs">{action.reason_code || "Not recorded"}</code>
              </EvidenceField>
              <EvidenceField label="Rule ID">
                <code className="text-xs">{action.rule_id || "Not recorded"}</code>
              </EvidenceField>
              <EvidenceField label="Final authority">{connectorAuthority(action.authorization_source)}</EvidenceField>
              <EvidenceField label="Approval reference">
                {action.pending_action_id ? (
                  <>
                    <code className="text-xs">{action.pending_action_id}</code>
                    <p className="mt-1">
                      <Link to={pendingHref} className="text-xs text-primary hover:underline">
                        {review ? "Open approval reference →" : "Browse this run's approvals →"}
                      </Link>
                    </p>
                  </>
                ) : (
                  "No approval reference recorded"
                )}
              </EvidenceField>
              <EvidenceField label="Break-glass ID">
                <code className="text-xs">{action.break_glass_id || "Not recorded"}</code>
              </EvidenceField>
            </dl>
            <p className="mt-4 text-xs text-muted-foreground">
              A pending decision may use an approved capability. A deny requires audited break-glass authority. An approval reference identifies an approval record, not this connector action.
            </p>
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Dispatch and response</CardTitle>
          </CardHeader>
          <CardContent>
            <dl className="space-y-3 text-sm">
              <EvidenceField label="Dispatch ID">
                <code className="text-xs">{action.dispatch_id || "Not recorded"}</code>
              </EvidenceField>
              <EvidenceField label="Dispatch deadline">
                {action.dispatch_deadline_at ? formatTimestamp(action.dispatch_deadline_at) : "Not recorded"}
              </EvidenceField>
              <EvidenceField label="Transport">{action.transport}</EvidenceField>
              <EvidenceField label="Response kind">{action.response_kind || "No response recorded"}</EvidenceField>
              <EvidenceField label="HTTP status">{action.http_status ?? "Not recorded"}</EvidenceField>
              <EvidenceField label="JSON-RPC error">{action.rpc_error_code ?? "Not recorded"}</EvidenceField>
              <EvidenceField label="Response bytes">{action.response_bytes ?? "Not recorded"}</EvidenceField>
              <EvidenceField label="Response digest">
                <code className="text-xs">{action.response_digest || "Not recorded"}</code>
              </EvidenceField>
            </dl>
            <p className="mt-4 text-xs text-muted-foreground">
              A received response, including HTTP 200, does not establish the business outcome. A missing response does
              not establish that no side effect occurred.
            </p>
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Metadata only</CardTitle>
          </CardHeader>
          <CardContent>
            <dl className="space-y-3 text-sm">
              <EvidenceField label="Argument digest">
                <code className="text-xs">{action.args_digest || "Not recorded"}</code>
              </EvidenceField>
              <EvidenceField label="Trace ID">
                <code className="text-xs">{action.trace_id || "Not recorded"}</code>
              </EvidenceField>
              <EvidenceField label="Policy revision">{action.policy_revision || "Not recorded"}</EvidenceField>
              <EvidenceField label="Target revision">{action.target_revision || "Not recorded"}</EvidenceField>
              <EvidenceField label="Tool revision">{action.tool_revision || "Not recorded"}</EvidenceField>
              <EvidenceField label="Credential reference">{leaseHref ? <Link to={leaseHref} className="font-mono text-xs text-primary hover:underline">{action.credential_ref}</Link> : action.credential_ref || "Not recorded"}</EvidenceField>
              <EvidenceField label="Export sink">{!review && action.export_sink_id ? <Link to={`/admin/action-deliveries?${new URLSearchParams({ tenant_id: action.tenant_id, sink_id: action.export_sink_id })}`} className="text-primary hover:underline">{action.export_sink_id}</Link> : action.export_sink_id || "Export not recorded"}</EvidenceField>
            </dl>
            <p className="mt-4 text-xs text-muted-foreground">
              This history excludes arguments, request and response bodies, and credentials. Missing revision or
              credential references do not establish that those checks were performed.
            </p>
            <p className="mt-2 text-xs text-muted-foreground">A recorded target revision identifies the configured connector target. It does not identify the upstream tool schema or implementation, and does not confirm the target's current configuration on another replica.</p>
          </CardContent>
        </Card>
      </div>
      <Card>
        <CardHeader>
          <CardTitle>Recorded timeline</CardTitle>
          <p className="text-sm text-muted-foreground">
            Ordered by durable sequence. Read-only refresh uses an 8s interval for up to 2 minutes after opening or
            Refresh.
          </p>
        </CardHeader>
        <CardContent>
          {data.events_truncated && (
            <p role="status" className="mb-4 text-sm text-warning">
              Timeline truncated: only the first 200 recorded events were returned. Additional events are outside this
              view.
            </p>
          )}
          {events.length === 0 ? (
            <p className="text-sm text-muted-foreground">No timeline events were returned.</p>
          ) : (
            <ol className="space-y-5">
              {[...events]
                .sort((a, b) => a.sequence - b.sequence)
                .map((event) => (
                  <TimelineEvent key={event.sequence} event={event} />
                ))}
            </ol>
          )}
        </CardContent>
      </Card>
  </div>;
}

function TimelineEvent({ event }: { event: AdminConnectorActionEvent }) {
  return (
    <li className="border-l-2 border-border pl-4">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs text-muted-foreground">#{event.sequence}</span>
        <ConnectorActionBadge state={event.state} />
        <code className="text-xs">{event.kind}</code>
        <time dateTime={event.observed_at} className="text-xs text-muted-foreground">
          {formatTimestamp(event.observed_at)}
        </time>
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-2 text-xs">
        <span>Observer: {event.observer}</span>
        {event.decision && (
          <span className="inline-flex items-center gap-1">
            Recorded decision: <StatusBadge status={event.decision} />
          </span>
        )}
        {event.authorization_source && <span>Authority: {connectorAuthority(event.authorization_source)}</span>}
        {event.reason_code && (
          <span>
            Reason: <code>{event.reason_code}</code>
          </span>
        )}
        {event.http_status != null && <span>HTTP: {event.http_status}</span>}
        {event.response_kind && <span>Response: {event.response_kind}</span>}
      </div>
      {event.pending_action_id && (
        <p className="mt-1 break-all text-xs text-muted-foreground">
          Approval reference: <code>{event.pending_action_id}</code>
        </p>
      )}
      {event.break_glass_id && (
        <p className="mt-1 break-all text-xs text-muted-foreground">
          Break-glass reference: <code>{event.break_glass_id}</code>
        </p>
      )}
    </li>
  );
}
