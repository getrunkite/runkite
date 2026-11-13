import type { ReactNode } from "react";
import { Link, useParams } from "react-router";
import type { AdminConnectorActionDetail, AdminConnectorActionEvent } from "../api/types";
import { useConnectorEvidence } from "../api/useConnectorEvidence";
import {
  connectorActionPath,
  connectorActionRunHref,
  connectorActionsHref,
  connectorActionState,
  connectorAuthority,
  connectorApprovalQueueHref,
} from "../lib/connectorActions";
import { ConnectorActionBadge, EvidenceRefresh } from "../components/connector-action-evidence";
import { DocsLink, ErrorState, formatTimestamp, PageHeader, StatusBadge, supportPage } from "../components/common";
import { Badge } from "../components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "../components/ui/card";
import { Skeleton } from "../components/ui/skeleton";

export function ConnectorActionDetail() {
  const { actionId } = useParams<{ actionId: string }>();
  return actionId ? (
    <ActionDetail key={actionId} actionId={actionId} />
  ) : (
    <ErrorState message="No connector action ID was provided." />
  );
}

function Field({ label, children }: { label: string; children?: ReactNode }) {
  return (
    <div className="grid gap-1 sm:grid-cols-[9rem_minmax(0,1fr)] sm:gap-3">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0 break-all">{children ?? "Not recorded"}</dd>
    </div>
  );
}

function ActionDetail({ actionId }: { actionId: string }) {
  const history = useConnectorEvidence<AdminConnectorActionDetail>(connectorActionPath(actionId));
  if (history.loading && !history.data)
    return (
      <div className="space-y-4">
        <Skeleton className="h-16 w-full" />
        <Skeleton className="h-64 w-full" />
      </div>
    );
  if (!history.data)
    return (
      <div className="space-y-4">
        <Link to="/admin/connector-actions" className="text-primary hover:underline">
          ← Connector actions
        </Link>
        <ErrorState message={history.error || "No connector action was returned."} />
        <EvidenceRefresh refresh={history.reload} loading={history.loading} />
      </div>
    );
  const { action, events, run_available } = history.data;
  const runHref = connectorActionRunHref(action, run_available);
  const pendingHref = connectorApprovalQueueHref(action.tenant_id, action.run_id);
  return (
    <div className="space-y-4">
      <PageHeader
        title="Connector action"
        subtitle={
          <>
            <Link
              to={connectorActionsHref({ tenant_id: action.tenant_id, run_id: action.run_id })}
              className="text-primary hover:underline"
            >
              ← Retained history
            </Link>
            <span className="mx-2">·</span>
            <code className="break-all text-xs">{action.action_id}</code>
          </>
        }
        actions={
          <>
            <DocsLink href={supportPage("connector-actions.html")}>Docs: connector actions →</DocsLink>
            <EvidenceRefresh refresh={history.reload} loading={history.loading} />
          </>
        }
      />
      {history.error && <ErrorState message={history.error} />}
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
              <Field label="Tenant">{action.tenant_id}</Field>
              <Field label="Principal">{action.principal || "No principal recorded"}</Field>
              <Field label="Agent">
                {action.agent_id}{" "}
                <span className="text-muted-foreground">
                  {action.agent_version > 0 ? `v${action.agent_version}` : "version unavailable"}
                </span>
              </Field>
              <Field label="Original run">
                {runHref ? (
                  <Link to={runHref} className="font-mono text-xs text-primary hover:underline">
                    {action.run_id}
                  </Link>
                ) : (
                  <>
                    <code className="text-xs">{action.run_id}</code>
                    <p className="mt-1 text-xs text-muted-foreground">
                      Original run unavailable. This record does not link to a replacement run with the same ID.
                    </p>
                  </>
                )}
              </Field>
              <Field label="Run instance">
                <code className="text-xs">{action.run_incarnation_id}</code>
              </Field>
              <Field label="Run created">{formatTimestamp(action.run_created_at)}</Field>
              <Field label="Original thread">
                <code className="text-xs">{action.thread_id}</code>
              </Field>
              <Field label="Recorded">{formatTimestamp(action.created_at)}</Field>
              <Field label="Last update">{formatTimestamp(action.updated_at)}</Field>
            </dl>
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Decision and final authority</CardTitle>
          </CardHeader>
          <CardContent>
            <dl className="space-y-3 text-sm">
              <Field label="Recorded decision">
                {action.decision ? <StatusBadge status={action.decision} /> : "No decision recorded"}
              </Field>
              <Field label="Reason code">
                <code className="text-xs">{action.reason_code || "Not recorded"}</code>
              </Field>
              <Field label="Rule ID">
                <code className="text-xs">{action.rule_id || "Not recorded"}</code>
              </Field>
              <Field label="Final authority">{connectorAuthority(action.authorization_source)}</Field>
              <Field label="Approval reference">
                {action.pending_action_id ? (
                  <>
                    <code className="text-xs">{action.pending_action_id}</code>
                    <p className="mt-1">
                      <Link to={pendingHref} className="text-xs text-primary hover:underline">
                        Browse this run's approvals →
                      </Link>
                    </p>
                  </>
                ) : (
                  "No approval reference recorded"
                )}
              </Field>
              <Field label="Break-glass ID">
                <code className="text-xs">{action.break_glass_id || "Not recorded"}</code>
              </Field>
            </dl>
            <p className="mt-4 text-xs text-muted-foreground">
              The recorded decision can remain pending or deny when a separate approved capability or break-glass window
              supplies final authority. An approval reference identifies an approval record, not this connector action.
            </p>
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Dispatch and response</CardTitle>
          </CardHeader>
          <CardContent>
            <dl className="space-y-3 text-sm">
              <Field label="Dispatch ID">
                <code className="text-xs">{action.dispatch_id || "Not recorded"}</code>
              </Field>
              <Field label="Dispatch deadline">
                {action.dispatch_deadline_at ? formatTimestamp(action.dispatch_deadline_at) : "Not recorded"}
              </Field>
              <Field label="Transport">{action.transport}</Field>
              <Field label="Response kind">{action.response_kind || "No response recorded"}</Field>
              <Field label="HTTP status">{action.http_status ?? "Not recorded"}</Field>
              <Field label="JSON-RPC error">{action.rpc_error_code ?? "Not recorded"}</Field>
              <Field label="Response bytes">{action.response_bytes ?? "Not recorded"}</Field>
              <Field label="Response digest">
                <code className="text-xs">{action.response_digest || "Not recorded"}</code>
              </Field>
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
              <Field label="Argument digest">
                <code className="text-xs">{action.args_digest || "Not recorded"}</code>
              </Field>
              <Field label="Trace ID">
                <code className="text-xs">{action.trace_id || "Not recorded"}</code>
              </Field>
              <Field label="Policy revision">{action.policy_revision || "Not recorded"}</Field>
              <Field label="Target revision">{action.target_revision || "Not recorded"}</Field>
              <Field label="Tool revision">{action.tool_revision || "Not recorded"}</Field>
              <Field label="Credential reference">{action.credential_ref || "Not recorded"}</Field>
            </dl>
            <p className="mt-4 text-xs text-muted-foreground">
              This history excludes arguments, request and response bodies, and credentials. Missing revision or
              credential references do not establish that those checks were performed.
            </p>
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
          {history.data.events_truncated && (
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
    </div>
  );
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
