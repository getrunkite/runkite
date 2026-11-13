import { AlertTriangle, Ban, CircleDot, Clock, Eye, RefreshCw } from "lucide-react";
import { Link } from "react-router";
import type { ColumnDef } from "@tanstack/react-table";
import type { AdminConnectorAction, AdminRun } from "../api/types";
import { useConnectorEvidence } from "../api/useConnectorEvidence";
import {
  connectorActionMatchesRun,
  connectorActionPath,
  connectorActionsHref,
  connectorActionsPath,
  connectorActionState,
} from "../lib/connectorActions";
import { DataTable } from "./data-table";
import { ErrorState, formatTimestamp } from "./common";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "./ui/card";

export function ConnectorActionBadge({ state }: { state: string }) {
  const meta = connectorActionState(state);
  const Icon =
    state === "response_observed"
      ? Eye
      : state === "blocked"
        ? Ban
        : state === "approval_required"
          ? Clock
          : state === "outcome_unknown" || state === "dispatch_intent"
            ? AlertTriangle
            : CircleDot;
  return (
    <Badge variant={meta.tone} title={meta.description}>
      <Icon aria-hidden="true" />
      {meta.label}
    </Badge>
  );
}

export function ConnectorEvidenceSetup() {
  return (
    <p className="text-sm text-muted-foreground">
      Recording is opt-in: enable <code>policy.action_evidence: true</code> with{" "}
      <code>policy.audit_required: true</code> on Postgres, MySQL or SQLite. Earlier calls are not backfilled. Local
      tools and standalone tool decisions are outside this history.
    </p>
  );
}

export function EvidenceRefresh({ refresh, loading }: { refresh: () => void; loading: boolean }) {
  return (
    <Button type="button" variant="outline" size="sm" onClick={refresh} disabled={loading}>
      <RefreshCw className="size-3.5" aria-hidden="true" />
      Refresh
    </Button>
  );
}

const previewColumns: ColumnDef<AdminConnectorAction, unknown>[] = [
  {
    accessorKey: "created_at",
    header: "Recorded",
    cell: ({ getValue }) => (
      <span className="text-xs text-muted-foreground">{formatTimestamp(getValue() as string)}</span>
    ),
  },
  {
    id: "operation",
    header: "Connector / tool",
    cell: ({ row }) => (
      <Link
        className="break-all text-sm text-primary hover:underline"
        to={`/admin${connectorActionPath(row.original.action_id)}`}
      >
        {row.original.connector}.{row.original.tool}
      </Link>
    ),
  },
  { accessorKey: "generation", header: "Attempt" },
  {
    accessorKey: "state",
    header: "Evidence",
    cell: ({ getValue }) => <ConnectorActionBadge state={getValue() as string} />,
  },
];

export function RunConnectorActions({ run }: { run: AdminRun }) {
  const path = connectorActionsPath({ tenant_id: run.tenant_id, run_id: run.run_id }, undefined, 5);
  const active = run.status === "pending" || run.status === "running";
  const history = useConnectorEvidence<AdminConnectorAction[]>(path, active);
  const rows = (history.data ?? []).filter((action) => connectorActionMatchesRun(action, run));
  const omitted = (history.data?.length ?? 0) - rows.length;
  return (
    <Card className="mb-4">
      <CardHeader className="flex-row flex-wrap items-start justify-between gap-3">
        <div>
          <CardTitle>Managed connector actions</CardTitle>
          <p className="mt-1 text-sm text-muted-foreground">Recent recorded MCP tool requests for this run instance.</p>
        </div>
        <div className="flex items-center gap-3">
          <Link
            className="text-sm text-primary hover:underline"
            to={connectorActionsHref({ tenant_id: run.tenant_id, run_id: run.run_id })}
          >
            View retained history →
          </Link>
          <EvidenceRefresh refresh={history.reload} loading={history.loading} />
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        {history.error && <ErrorState message={history.error} />}
        {history.data && rows.length === 0 && (
          <>
            <p className="text-sm">No recorded connector calls for this run instance.</p>
            <ConnectorEvidenceSetup />
          </>
        )}
        {(history.loading || rows.length > 0) && (
          <DataTable
            columns={previewColumns}
            data={rows}
            getRowId={(action) => action.action_id}
            loading={history.loading}
          />
        )}
        {omitted > 0 && (
          <p className="text-xs text-muted-foreground">
            Older retained records for a reused run ID are excluded from this preview.
          </p>
        )}
        <p className="text-xs text-muted-foreground">
          A response records a reply, not proof of a completed business effect. Unknown outcomes need downstream
          investigation before retry.
        </p>
      </CardContent>
    </Card>
  );
}
