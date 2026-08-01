import { useState } from "react";
import { Package } from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";
import { useApi } from "../api/useApi";
import type { AdminRegistryEntry } from "../api/types";
import { EmptyState, ErrorState, PageHeader } from "../components/common";
import { DataTable } from "../components/data-table";
import { ADMIN_PAGE_SIZE, ListPager } from "../components/list-pager";
import { Badge } from "../components/ui/badge";

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
  const [offset, setOffset] = useState(0);
  const path = `/registry?limit=${ADMIN_PAGE_SIZE}&offset=${offset}`;
  const { data, error, loading } = useApi<AdminRegistryEntry[]>(path, [offset]);

  return (
    <div>
      <PageHeader
        title="Registry"
        subtitle="A metadata catalog for publishing and discovering agent definitions across every tenant."
      />
      {error && !data && <ErrorState message={error} />}
      {data && data.length === 0 && offset === 0 && (
        <EmptyState
          icon={Package}
          title="No registry entries published yet"
          message="Publish an entry via PUT /registry/entries/{name} to see it here."
        />
      )}
      {(data === null || (data && data.length > 0) || offset > 0) && !(data && data.length === 0 && offset === 0) && (
        <>
          <DataTable columns={columns} data={data ?? []} getRowId={(e) => `${e.tenant_id}:${e.name}`} loading={loading} />
          <ListPager offset={offset} limit={ADMIN_PAGE_SIZE} pageCount={data?.length ?? 0} onChange={setOffset} />
        </>
      )}
    </div>
  );
}
