import { Clock } from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";
import { useApi } from "../api/useApi";
import type { AdminCronSchedule } from "../api/types";
import { EmptyState, ErrorState, formatRelativeTime, formatTimestamp, PageHeader } from "../components/common";
import { DataTable } from "../components/data-table";
import { Badge } from "../components/ui/badge";
import { Tooltip, TooltipContent, TooltipTrigger } from "../components/ui/tooltip";

const columns: ColumnDef<AdminCronSchedule, unknown>[] = [
  { accessorKey: "name", header: "Name", cell: ({ getValue }) => <span className="font-medium">{getValue() as string}</span> },
  { accessorKey: "agent_id", header: "Agent" },
  {
    accessorKey: "expression",
    header: "Expression",
    cell: ({ getValue }) => <span className="font-mono text-xs">{getValue() as string}</span>,
  },
  {
    accessorKey: "timezone",
    header: "Timezone",
    cell: ({ getValue }) => <span className="text-muted-foreground">{(getValue() as string) || "UTC"}</span>,
  },
  {
    id: "tenant_id",
    header: "Tenant",
    accessorFn: (c) => c.tenant_id ?? "default",
    cell: ({ getValue }) => <Badge variant="outline">{getValue() as string}</Badge>,
  },
  {
    accessorKey: "enabled",
    header: "Status",
    cell: ({ getValue }) => {
      const enabled = getValue() as boolean;
      return <Badge variant={enabled ? "success" : "muted"}>{enabled ? "enabled" : "disabled"}</Badge>;
    },
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

export function Cron() {
  const { data, error, loading } = useApi<AdminCronSchedule[]>("/cron");

  if (error && !data) return <ErrorState message={error} />;
  if (data && data.length === 0) {
    return (
      <div>
        <PageHeader title="Cron schedules" subtitle="Across every tenant." />
        <EmptyState icon={Clock} message="No cron schedules configured in langgraph.json." />
      </div>
    );
  }

  return (
    <div>
      <PageHeader title="Cron schedules" subtitle="Across every tenant." />
      <DataTable
        columns={columns}
        data={data ?? []}
        getRowId={(c) => `${c.tenant_id ?? "default"}:${c.name}`}
        loading={loading}
        initialSorting={[{ id: "updated_at", desc: true }]}
      />
    </div>
  );
}
