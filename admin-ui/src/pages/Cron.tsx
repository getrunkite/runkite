import { Clock } from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";
import { useApi } from "../api/useApi";
import type { AdminCronSchedule } from "../api/types";
import { EmptyState, ErrorState, formatRelativeTime, formatTimestamp, PageHeader, supportPage} from "../components/common";
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

  return (
    <div>
      <PageHeader
        title="Cron schedules"
        subtitle="Across every tenant."
        actions={
          <a
            href={supportPage("admin-guide.html#7-cron")}
            target="_blank"
            rel="noreferrer"
            className="text-sm font-medium text-primary hover:underline"
          >
            Docs: cron →
          </a>
        }
      />
      {error && !data && <ErrorState message={error} />}
      {data && data.length === 0 && (
        <EmptyState
          icon={Clock}
          title="No cron schedules"
          message="Optional. Declare schedules in langgraph.json (or via the cron API) when an agent should wake on a cadence — empty is fine for interactive-only deployments."
        
          learnMore={{ href: supportPage("cron.html"), label: "Docs: cron →" }}
        />
      )}
      {(data === null || (data && data.length > 0)) && (
        <DataTable
          columns={columns}
          data={data ?? []}
          getRowId={(c) => `${c.tenant_id ?? "default"}:${c.name}`}
          loading={loading}
          initialSorting={[{ id: "updated_at", desc: true }]}
        />
      )}
    </div>
  );
}
