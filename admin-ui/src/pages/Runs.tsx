import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { Workflow } from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";
import { useApi } from "../api/useApi";
import type { AdminRun } from "../api/types";
import { EmptyState, ErrorState, formatRelativeTime, formatTimestamp, PageHeader, StatusBadge } from "../components/common";
import { DataTable } from "../components/data-table";
import { Badge } from "../components/ui/badge";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../components/ui/select";
import { Tooltip, TooltipContent, TooltipTrigger } from "../components/ui/tooltip";

const STATUS_OPTIONS = ["all", "pending", "running", "success", "error", "timeout", "interrupted"];

const columns: ColumnDef<AdminRun, unknown>[] = [
  {
    accessorKey: "run_id",
    header: "Run ID",
    cell: ({ row }) => (
      <Link
        to={`/admin/runs/${row.original.run_id}`}
        onClick={(e) => e.stopPropagation()}
        className="font-mono text-xs font-medium text-primary hover:underline"
      >
        {row.original.run_id}
      </Link>
    ),
  },
  { accessorKey: "agent_id", header: "Agent" },
  {
    accessorKey: "thread_id",
    header: "Thread",
    cell: ({ row }) =>
      row.original.thread_id ? (
        <Link
          to={`/admin/threads/${row.original.thread_id}`}
          onClick={(e) => e.stopPropagation()}
          className="font-mono text-xs text-primary hover:underline"
        >
          {row.original.thread_id}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
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

export function Runs() {
  const [status, setStatus] = useState("all");
  const path = status === "all" ? "/runs" : `/runs?status=${encodeURIComponent(status)}`;
  const { data, error, loading } = useApi<AdminRun[]>(path, [status], 8000);
  const navigate = useNavigate();

  return (
    <div>
      <PageHeader title="Runs" subtitle="Across every tenant. Auto-refreshes every 8s. Click any column to sort." />

      <div className="mb-4 flex items-center gap-3">
        <Select value={status} onValueChange={setStatus}>
          <SelectTrigger className="w-44">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {STATUS_OPTIONS.map((s) => (
              <SelectItem key={s} value={s}>
                {s === "all" ? "All statuses" : s}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      {error && !data && <ErrorState message={error} />}
      {data && data.length === 0 && (
        <EmptyState
          icon={Workflow}
          title={status === "all" ? "No runs yet" : "No matching runs"}
          message={
            status === "all"
              ? "Runs appear here as clients create them across every tenant."
              : `No runs with status “${status}”. Try another filter.`
          }
          action={
            status !== "all" ? (
              <button type="button" className="text-sm font-medium text-primary hover:underline" onClick={() => setStatus("all")}>
                Clear status filter
              </button>
            ) : undefined
          }
        />
      )}
      {(data === null || (data && data.length > 0)) && (
        <DataTable
          columns={columns}
          data={data ?? []}
          getRowId={(r) => r.run_id}
          onRowClick={(r) => navigate(`/admin/runs/${r.run_id}`)}
          loading={loading}
          initialSorting={[{ id: "updated_at", desc: true }]}
        />
      )}
    </div>
  );
}
