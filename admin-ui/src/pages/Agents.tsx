import { useMemo, useState } from "react";
import { useNavigate } from "react-router";
import { Bot, Search } from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";
import { useApi } from "../api/useApi";
import type { AdminAgent } from "../api/types";
import { DocsLink, EmptyState, ErrorState, PageHeader, supportPage} from "../components/common";
import { DataTable } from "../components/data-table";
import { ListPager, adminListPath } from "../components/list-pager";
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
  const navigate = useNavigate();
  const [cursorStack, setCursorStack] = useState<string[]>([]);
  const cursor = cursorStack.length ? cursorStack[cursorStack.length - 1] : undefined;
  const path = adminListPath("/agents", cursor);
  const { data, error, loading, nextCursor } = useApi<AdminAgent[]>(path, [cursor]);
  const [query, setQuery] = useState("");
  const onFirstPage = cursorStack.length === 0;

  const filtered = useMemo(() => {
    if (!data) return null;
    const q = query.trim().toLowerCase();
    if (!q) return data;
    return data.filter((a) => a.agent_id.toLowerCase().includes(q) || a.name.toLowerCase().includes(q));
  }, [data, query]);

  return (
    <div>
      <PageHeader
        title="Agents"
        subtitle="Registered across every tenant. Filter applies to the current page."
        actions={<DocsLink href={supportPage("admin-guide.html#2-agents")}>Docs: agents →</DocsLink>}
      />

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

          {data && data.length === 0 && onFirstPage ? (
            <EmptyState
              icon={Bot}
              title="No agents registered"
              message="Start the control plane with a graph config, then attach a runner. Try: runkite dev --config examples/echo_agent/langgraph.json"
            
          learnMore={{ href: supportPage("agents.html"), label: "Docs: agents →" }}
        />
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
            <>
              <DataTable
                columns={columns}
                data={filtered ?? []}
                getRowId={(a) => `${a.tenant_id}:${a.agent_id}`}
                loading={loading}
                initialSorting={[{ id: "agent_id", desc: false }]}
                onRowClick={(a) => navigate(`/admin/agents/${encodeURIComponent(a.agent_id)}?tenant_id=${encodeURIComponent(a.tenant_id)}`)}
              />
              <ListPager
                pageIndex={cursorStack.length}
                pageCount={data?.length ?? 0}
                hasNext={Boolean(nextCursor)}
                onPrev={() => setCursorStack((s) => s.slice(0, -1))}
                onNext={() => {
                  if (nextCursor) setCursorStack((s) => [...s, nextCursor]);
                }}
              />
            </>
          )}
        </>
      )}
    </div>
  );
}
