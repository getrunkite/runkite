import { useState } from "react";
import { Link, useNavigate } from "react-router";
import { Package, Plus } from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";
import { useApi } from "../api/useApi";
import type { AdminRegistryEntry } from "../api/types";
import { DocsLink, EmptyState, ErrorState, PageHeader, supportPage} from "../components/common";
import { DataTable } from "../components/data-table";
import { ListPager, adminListPath } from "../components/list-pager";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";

const columns: ColumnDef<AdminRegistryEntry, unknown>[] = [
  {
    accessorKey: "name",
    header: "Name",
    cell: ({ getValue }) => <span className="font-mono text-xs font-medium">{getValue() as string}</span>,
  },
  {
    accessorKey: "display_name",
    header: "Display name",
    cell: ({ getValue }) => (getValue() as string) || "—",
  },
  {
    accessorKey: "tenant_id",
    header: "Tenant",
    cell: ({ getValue }) => <Badge variant="outline">{getValue() as string}</Badge>,
  },
  {
    accessorKey: "author",
    header: "Author",
    cell: ({ getValue }) => <span className="text-muted-foreground">{(getValue() as string) || "—"}</span>,
  },
  {
    id: "tags",
    header: "Tags",
    accessorFn: (entry) => entry.tags?.join(", ") ?? "",
    enableSorting: false,
    cell: ({ row }) =>
      row.original.tags && row.original.tags.length > 0 ? (
        <div className="flex flex-wrap gap-1">
          {row.original.tags.map((tag) => (
            <Badge key={tag} variant="muted">
              {tag}
            </Badge>
          ))}
        </div>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    id: "source",
    header: "Source",
    accessorFn: (entry) => `${entry.source_type}: ${entry.source_ref}`,
    cell: ({ getValue }) => (
      <span className="max-w-xs truncate font-mono text-xs text-muted-foreground">{getValue() as string}</span>
    ),
  },
  { accessorKey: "version", header: "Version", cell: ({ getValue }) => `v${getValue() as number}` },
];

export function Registry() {
  const navigate = useNavigate();
  const [cursorStack, setCursorStack] = useState<string[]>([]);
  const cursor = cursorStack.length ? cursorStack[cursorStack.length - 1] : undefined;
  const path = adminListPath("/registry", cursor);
  const { data, error, loading, nextCursor } = useApi<AdminRegistryEntry[]>(path, [cursor]);
  const onFirstPage = cursorStack.length === 0;

  return (
    <div>
      <PageHeader
        title="Registry"
        subtitle="A metadata catalog for publishing and discovering agent definitions across every tenant."
        actions={
          <>
            <DocsLink href={supportPage("admin-guide.html#4-registry")}>Docs: registry →</DocsLink>
            <Button size="sm" asChild>
              <Link to="/admin/registry/new">
                <Plus className="size-3.5" />
                Publish entry
              </Link>
            </Button>
          </>
        }
      />
      {error && !data && <ErrorState message={error} />}
      {data && data.length === 0 && onFirstPage && (
        <EmptyState
          icon={Package}
          title="Registry is empty"
          message="Optional catalog. Publish with PUT /registry/entries/{name} when you want discoverable agent definitions across tenants — see docs/registry.md."
        
          learnMore={{ href: supportPage("registry.html"), label: "Docs: registry →" }}
        />
      )}
      {(data === null || (data && data.length > 0) || !onFirstPage) && !(data && data.length === 0 && onFirstPage) && (
        <>
          <DataTable
            columns={columns}
            data={data ?? []}
            getRowId={(e) => `${e.tenant_id}:${e.name}`}
            loading={loading}
            onRowClick={(e) =>
              navigate(`/admin/registry/${encodeURIComponent(e.name)}?tenant_id=${encodeURIComponent(e.tenant_id)}`)
            }
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
    </div>
  );
}
