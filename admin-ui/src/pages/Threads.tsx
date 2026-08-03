import { useState } from "react";
import { Link, useNavigate } from "react-router";
import { MessagesSquare } from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";
import { useApi } from "../api/useApi";
import type { AdminThread } from "../api/types";
import { EmptyState, ErrorState, formatRelativeTime, formatTimestamp, PageHeader, StatusBadge } from "../components/common";
import { DataTable } from "../components/data-table";
import { ADMIN_PAGE_SIZE, ListPager } from "../components/list-pager";
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
  const [offset, setOffset] = useState(0);
  const path = `/threads?limit=${ADMIN_PAGE_SIZE}&offset=${offset}`;
  const { data, error, loading } = useApi<AdminThread[]>(path, [offset]);
  const navigate = useNavigate();

  return (
    <div>
      <PageHeader title="Threads" subtitle="Across every tenant. Click any column to sort the current page." />
      {error && !data && <ErrorState message={error} />}
      {data && data.length === 0 && offset === 0 && (
        <EmptyState
          icon={MessagesSquare}
          message="No threads yet -- one is created the moment a client starts a conversation."
        />
      )}
      {(data === null || (data && data.length > 0) || offset > 0) && !(data && data.length === 0 && offset === 0) && (
        <>
          <DataTable
            columns={columns}
            data={data ?? []}
            getRowId={(t) => t.thread_id}
            onRowClick={(t) => navigate(`/admin/threads/${t.thread_id}`)}
            loading={loading}
            initialSorting={[{ id: "updated_at", desc: true }]}
          />
          <ListPager offset={offset} limit={ADMIN_PAGE_SIZE} pageCount={data?.length ?? 0} onChange={setOffset} />
        </>
      )}
    </div>
  );
}
