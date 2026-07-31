import { Package } from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";
import { useApi } from "../api/useApi";
import type { AdminRegistryEntry } from "../api/types";
import { EmptyState, ErrorState, PageHeader } from "../components/common";
import { DataTable } from "../components/data-table";
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
  const { data, error, loading } = useApi<AdminRegistryEntry[]>("/registry");

  if (error && !data) return <ErrorState message={error} />;
  if (data && data.length === 0) {
    return (
      <div>
        <PageHeader title="Registry" subtitle="A metadata catalog for publishing and discovering agent definitions." />
        <EmptyState
          icon={Package}
          title="No registry entries published yet"
          message="Publish an entry via PUT /registry/entries/{name} to see it here."
        />
      </div>
    );
  }

  return (
    <div>
      <PageHeader
        title="Registry"
        subtitle={
          data
            ? `${data.length} published across every tenant. source_ref is where to deploy an entry, not something this control plane executes.`
            : undefined
        }
      />
      <DataTable columns={columns} data={data ?? []} getRowId={(e) => `${e.tenant_id}:${e.name}`} loading={loading} />
    </div>
  );
}
