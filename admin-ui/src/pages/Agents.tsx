import { useMemo, useState } from "react";
import { Bot, Search } from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";
import { useApi } from "../api/useApi";
import type { AdminAgent } from "../api/types";
import { EmptyState, ErrorState, PageHeader } from "../components/common";
import { DataTable } from "../components/data-table";
import { Badge } from "../components/ui/badge";
import { Input } from "../components/ui/input";

const columns: ColumnDef<AdminAgent, unknown>[] = [
  {
    accessorKey: "agent_id",
    header: "Agent ID",
    cell: ({ getValue }) => <span className="font-mono text-xs">{getValue() as string}</span>,
  },
  { accessorKey: "name", header: "Name", cell: ({ getValue }) => <span className="font-medium">{getValue() as string}</span> },
  {
    accessorKey: "tenant_id",
    header: "Tenant",
    cell: ({ getValue }) => <Badge variant="outline">{getValue() as string}</Badge>,
  },
  { accessorKey: "version", header: "Version", cell: ({ getValue }) => `v${getValue() as number}` },
  {
    accessorKey: "description",
    header: "Description",
    enableSorting: false,
    cell: ({ getValue }) => (
      <span className="max-w-xs truncate text-muted-foreground">{(getValue() as string) || "—"}</span>
    ),
  },
];

export function Agents() {
  const { data, error, loading } = useApi<AdminAgent[]>("/agents");
  const [query, setQuery] = useState("");

  const filtered = useMemo(() => {
    if (!data) return null;
    const q = query.trim().toLowerCase();
    if (!q) return data;
    return data.filter((a) => a.agent_id.toLowerCase().includes(q) || a.name.toLowerCase().includes(q));
  }, [data, query]);

  return (
    <div>
      <PageHeader title="Agents" subtitle={data ? `${data.length} registered across every tenant.` : undefined} />

      {error && !data && <ErrorState message={error} />}

      {(!error || data) && (
        <>
          <div className="relative mb-4 max-w-xs">
            <Search className="absolute top-1/2 left-3 size-3.5 -translate-y-1/2 text-muted-foreground" />
            <Input
              placeholder="Filter by ID or name..."
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              className="pl-9"
            />
          </div>

          {data && data.length === 0 ? (
            <EmptyState icon={Bot} title="No agents registered" message="Agents appear here once a langgraph.json is bootstrapped." />
          ) : filtered && filtered.length === 0 ? (
            <EmptyState
              title="No matches"
              message={`Nothing matches "${query}".`}
              action={
                <button type="button" className="text-sm font-medium text-primary hover:underline" onClick={() => setQuery("")}>
                  Clear filter
                </button>
              }
            />
          ) : (
            <DataTable
              columns={columns}
              data={filtered ?? []}
              getRowId={(a) => `${a.tenant_id}:${a.agent_id}`}
              loading={loading}
              initialSorting={[{ id: "agent_id", desc: false }]}
            />
          )}
        </>
      )}
    </div>
  );
}
