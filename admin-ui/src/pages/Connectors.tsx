import { Plug } from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";
import { useApi } from "../api/useApi";
import type { AdminConnector } from "../api/types";
import { EmptyState, ErrorState, PageHeader, StatusBadge } from "../components/common";
import { DataTable } from "../components/data-table";

const columns: ColumnDef<AdminConnector, unknown>[] = [
  { accessorKey: "name", header: "Name", cell: ({ getValue }) => <span className="font-medium">{getValue() as string}</span> },
  {
    accessorKey: "type",
    header: "Type",
    cell: ({ getValue }) => <span className="font-mono text-xs text-muted-foreground">{getValue() as string}</span>,
  },
  {
    accessorKey: "mcp",
    header: "MCP endpoint",
    cell: ({ getValue }) => (
      <span className="max-w-xs truncate text-muted-foreground">{(getValue() as string) || "—"}</span>
    ),
  },
  {
    accessorKey: "circuit_breaker_state",
    header: "Circuit breaker",
    cell: ({ getValue }) => {
      const state = getValue() as string | undefined;
      return state ? <StatusBadge status={state} /> : "—";
    },
  },
];

export function Connectors() {
  const { data, error, loading } = useApi<AdminConnector[]>("/connectors");

  return (
    <div>
      <PageHeader title="Connectors" subtitle="OAuth/MCP sessions runners pre-warm for their declared agents." />
      {error && !data && <ErrorState message={error} />}
      {data && data.length === 0 && (
        <EmptyState
          icon={Plug}
          title="No connectors"
          message="Optional. Add a connectors block to langgraph.json when agents need pre-warmed OAuth or MCP sessions — see docs/connectors.md."
        />
      )}
      {(data === null || (data && data.length > 0)) && (
        <DataTable columns={columns} data={data ?? []} getRowId={(c) => c.name} loading={loading} />
      )}
    </div>
  );
}
