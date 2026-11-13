import { Link, useNavigate, useSearchParams } from "react-router";
import { Shield } from "lucide-react";
import { useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import type { AdminConnectorAction } from "../api/types";
import { useConnectorEvidence } from "../api/useConnectorEvidence";
import {
  CONNECTOR_ACTION_STATES,
  connectorActionPath,
  connectorActionScope,
  connectorActionsPath,
  connectorActionState,
  connectorAuthority,
  type ConnectorActionFilters,
} from "../lib/connectorActions";
import { ConnectorActionBadge, ConnectorEvidenceSetup, EvidenceRefresh } from "../components/connector-action-evidence";
import { DataTable } from "../components/data-table";
import { ListPager } from "../components/list-pager";
import {
  DocsLink,
  EmptyState,
  ErrorState,
  formatTimestamp,
  PageHeader,
  StatusBadge,
  supportPage,
} from "../components/common";
import { Badge } from "../components/ui/badge";
import { Input } from "../components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../components/ui/select";

const columns: ColumnDef<AdminConnectorAction, unknown>[] = [
  {
    accessorKey: "created_at",
    header: "Recorded",
    cell: ({ getValue }) => (
      <span className="whitespace-nowrap text-xs text-muted-foreground">{formatTimestamp(getValue() as string)}</span>
    ),
  },
  {
    accessorKey: "action_id",
    header: "Action",
    cell: ({ row }) => (
      <Link
        to={`/admin${connectorActionPath(row.original.action_id)}`}
        onClick={(event) => event.stopPropagation()}
        className="font-mono text-xs text-primary hover:underline"
        title={row.original.action_id}
      >
        {row.original.action_id}
      </Link>
    ),
  },
  {
    accessorKey: "tenant_id",
    header: "Tenant",
    cell: ({ getValue }) => <Badge variant="outline">{getValue() as string}</Badge>,
  },
  {
    id: "operation",
    header: "Connector / tool",
    cell: ({ row }) => (
      <span className="font-mono text-xs">
        {row.original.connector}.{row.original.tool}
      </span>
    ),
  },
  {
    accessorKey: "state",
    header: "Evidence",
    cell: ({ getValue }) => <ConnectorActionBadge state={getValue() as string} />,
  },
  {
    accessorKey: "decision",
    header: "Recorded decision",
    cell: ({ getValue }) =>
      getValue() ? (
        <StatusBadge status={getValue() as string} />
      ) : (
        <span className="text-xs text-muted-foreground">Not recorded</span>
      ),
  },
  {
    accessorKey: "authorization_source",
    header: "Final authority",
    cell: ({ getValue }) => <span className="text-xs">{connectorAuthority(getValue() as string | undefined)}</span>,
  },
  {
    accessorKey: "run_id",
    header: "Original run",
    cell: ({ getValue }) => <span className="font-mono text-xs">{getValue() as string}</span>,
  },
  { accessorKey: "generation", header: "Attempt" },
];

export function ConnectorActions() {
  const [params, setParams] = useSearchParams();
  const filters: ConnectorActionFilters = {
    tenant_id: params.get("tenant_id") ?? "",
    run_id: params.get("run_id") ?? "",
    connector: params.get("connector") ?? "",
    state: params.get("state") ?? "",
  };
  const update = (name: keyof ConnectorActionFilters, value: string) => {
    const next = new URLSearchParams(params);
    if (value) next.set(name, value);
    else next.delete(name);
    setParams(next, { replace: true });
  };
  return (
    <div>
      <PageHeader
        title="Connector actions"
        subtitle="Retained metadata from managed MCP tool calls across tenants. Decisions, authority and observed responses are separate evidence."
        actions={<DocsLink href={supportPage("connector-actions.html")}>Docs: connector actions →</DocsLink>}
      />
      <div className="mb-4 flex flex-wrap items-center gap-3">
        <Input
          className="w-44"
          aria-label="Filter by tenant ID"
          placeholder="Tenant ID"
          value={filters.tenant_id}
          onChange={(event) => update("tenant_id", event.target.value)}
        />
        <Input
          className="w-48"
          aria-label="Filter by original run ID"
          placeholder="Original run ID"
          value={filters.run_id}
          onChange={(event) => update("run_id", event.target.value)}
        />
        <Input
          className="w-44"
          aria-label="Filter by connector"
          placeholder="Connector"
          value={filters.connector}
          onChange={(event) => update("connector", event.target.value)}
        />
        <Select value={filters.state || "all"} onValueChange={(value) => update("state", value === "all" ? "" : value)}>
          <SelectTrigger className="w-52" aria-label="Filter by evidence state">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All evidence states</SelectItem>
            {CONNECTOR_ACTION_STATES.map((state) => (
              <SelectItem key={state} value={state}>
                {connectorActionState(state).label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {connectorActionScope(filters) && (
          <button type="button" className="text-sm text-primary hover:underline" onClick={() => setParams({})}>
            Clear filters
          </button>
        )}
      </div>
      <ConnectorActionResults key={connectorActionScope(filters)} filters={filters} />
    </div>
  );
}

function ConnectorActionResults({ filters }: { filters: ConnectorActionFilters }) {
  const [cursors, setCursors] = useState<string[]>([]);
  const cursor = cursors.at(-1);
  const path = connectorActionsPath(filters, cursor);
  const history = useConnectorEvidence<AdminConnectorAction[]>(path, !cursor);
  const navigate = useNavigate();
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-xs text-muted-foreground">
          First page refreshes every 8s for up to 2 minutes. Refresh restarts read-only updates.
        </p>
        <EvidenceRefresh refresh={history.reload} loading={history.loading} />
      </div>
      {history.error && <ErrorState message={history.error} />}
      {history.data?.length === 0 && !cursor ? (
        <div className="space-y-4">
          <EmptyState
            icon={Shield}
            title="No recorded connector actions"
            message="No retained records match this view. This does not establish that an agent performed no actions."
          />
          <ConnectorEvidenceSetup />
        </div>
      ) : (
        !(history.error && !history.data) && (
          <>
            <DataTable
              columns={columns}
              data={history.data ?? []}
              getRowId={(action) => action.action_id}
              loading={history.loading}
              onRowClick={(action) => navigate(`/admin${connectorActionPath(action.action_id)}`)}
              initialSorting={[{ id: "created_at", desc: true }]}
            />
            <ListPager
              pageIndex={cursors.length}
              pageCount={history.data?.length ?? 0}
              hasNext={Boolean(history.nextCursor)}
              onPrev={() => setCursors((old) => old.slice(0, -1))}
              onNext={() => {
                if (history.nextCursor) setCursors((old) => [...old, history.nextCursor!]);
              }}
            />
          </>
        )
      )}
      <p className="text-xs text-muted-foreground">
        Retained actions can outlive their original run. Open an action to inspect its timeline and a verified run link
        when available. This page does not execute or retry tools.
      </p>
    </div>
  );
}
