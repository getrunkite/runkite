import { Clock } from "lucide-react";
import { Link } from "react-router";
import type { ColumnDef } from "@tanstack/react-table";
import { useApi } from "../api/useApi";
import type { AdminCronSchedule } from "../api/types";
import { DocsLink, EmptyState, ErrorState, formatRelativeTime, formatTimestamp, PageHeader, StatusBadge, supportPage } from "../components/common";
import { adminCronRowId, adminRetainedRunHref, adminThreadPath } from "../lib/adminPaths";
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
    id: "history",
    header: "Thread",
    cell: ({ row }) => {
      const schedule = row.original;
      return schedule.thread_available ? (
        <Link
          to={`/admin${adminThreadPath(schedule.thread_id, schedule.tenant_id ?? "default")}`}
          className="font-mono text-xs text-primary hover:underline"
        >
          {schedule.thread_id}
        </Link>
      ) : (
        <span className="text-muted-foreground">
          {schedule.history_status === "unavailable" ? "Thread unavailable" : "No retained thread"}
        </span>
      );
    },
  },
  {
    id: "latest_run",
    header: "Latest retained run",
    cell: ({ row }) => {
      const schedule = row.original;
      if (schedule.history_status === "unavailable") {
        return <span className="text-destructive">History unavailable</span>;
      }
      const run = schedule.latest_run;
      if (!run) return <span className="text-muted-foreground">No retained runs</span>;
      const runHref = adminRetainedRunHref({ ...run, tenant_id: schedule.tenant_id ?? "default" });
      return (
        <div className="flex flex-col items-start gap-1">
          {runHref ? <Link to={runHref} className="text-primary hover:underline">
            <StatusBadge status={run.status} />
            <span className="sr-only"> Open run {run.run_id}</span>
          </Link> : <StatusBadge status={run.status} />}
          <Tooltip>
            <TooltipTrigger className="text-xs text-muted-foreground">{formatRelativeTime(run.updated_at)}</TooltipTrigger>
            <TooltipContent>Updated {formatTimestamp(run.updated_at)}</TooltipContent>
          </Tooltip>
        </div>
      );
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
  const { data, error, loading } = useApi<AdminCronSchedule[]>("/cron?include_history=true", [], 15000);

  return (
    <div>
      <PageHeader
        title="Cron schedules"
        subtitle="Across every tenant. Refreshes every 15s."
        actions={<DocsLink href={supportPage("admin-guide.html#7-cron")}>Docs: cron →</DocsLink>}
      />
      <p className="mb-4 text-sm text-muted-foreground">
        Latest retained run on each schedule thread. A linked status opens its exact stored result.
        Skipped fires and failures before a run was created are not recorded here.
        History may have been deleted, and other callers can create runs on the same thread.
      </p>
      {error && !data && <ErrorState message={error} />}
      {error && data && <ErrorState message={`Could not refresh schedules. Displayed history may be stale. ${error}`} />}
      {data && data.length === 0 && (
        <EmptyState
          icon={Clock}
          title="No cron schedules"
          message="Optional. Declare schedules in langgraph.json when an agent should wake on a cadence. Interactive deployments can leave this empty."
          learnMore={{ href: supportPage("cron.html"), label: "Docs: cron →" }}
        />
      )}
      {(data === null || (data && data.length > 0)) && (
        <DataTable
          columns={columns}
          data={data ?? []}
          getRowId={adminCronRowId}
          loading={loading}
          initialSorting={[{ id: "updated_at", desc: true }]}
        />
      )}
    </div>
  );
}
