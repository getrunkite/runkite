import { Link, useNavigate } from "react-router-dom";
import { MessagesSquare } from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";
import { useApi } from "../api/useApi";
import type { AdminThread } from "../api/types";
import { EmptyState, ErrorState, formatRelativeTime, formatTimestamp, PageHeader, StatusBadge } from "../components/common";
import { DataTable } from "../components/data-table";
import { Badge } from "../components/ui/badge";
import { Tooltip, TooltipContent, TooltipTrigger } from "../components/ui/tooltip";

const columns: ColumnDef<AdminThread, unknown>[] = [
  {
    accessorKey: "thread_id",
    header: "Thread ID",
    cell: ({ row }) => (
      <Link
        to={`/admin/threads/${row.original.thread_id}`}
        onClick={(e) => e.stopPropagation()}
        className="font-mono text-xs font-medium text-primary hover:underline"
      >
        {row.original.thread_id}
      </Link>
    ),
  },
  {
    accessorKey: "status",
    header: "Status",
    cell: ({ getValue }) => <StatusBadge status={getValue() as string} />,
  },
  {
    accessorKey: "tenant_id",
    header: "Tenant",
    cell: ({ getValue }) => <Badge variant="outline">{getValue() as string}</Badge>,
  },
  {
    accessorKey: "updated_at",
    header: "Updated",
    cell: ({ getValue }) => {
      const iso = getValue() as string;
      return (
        <Tooltip>
          <TooltipTrigger className="text-muted-foreground">{formatRelativeTime(iso)}</TooltipTrigger>
          <TooltipContent>{formatTimestamp(iso)}</TooltipContent>
        </Tooltip>
      );
    },
  },
];

export function Threads() {
  const { data, error, loading } = useApi<AdminThread[]>("/threads");
  const navigate = useNavigate();

  if (error && !data) return <ErrorState message={error} />;
  if (data && data.length === 0) {
    return (
      <div>
        <PageHeader title="Threads" subtitle="Across every tenant." />
        <EmptyState icon={MessagesSquare} message="No threads yet -- one is created the moment a client starts a conversation." />
      </div>
    );
  }

  return (
    <div>
      <PageHeader
        title="Threads"
        subtitle={data ? `${data.length} across every tenant. Click any column to sort.` : undefined}
      />
      <DataTable
        columns={columns}
        data={data ?? []}
        getRowId={(t) => t.thread_id}
        onRowClick={(t) => navigate(`/admin/threads/${t.thread_id}`)}
        loading={loading}
        initialSorting={[{ id: "updated_at", desc: true }]}
      />
    </div>
  );
}
