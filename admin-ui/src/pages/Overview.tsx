import { useApi } from "../api/useApi";
import type { AdminOverview } from "../api/types";
import { Card, ErrorMessage, Loading, PageHeader, StatCard, StatusBadge } from "../components/ui";

export function Overview() {
  const { data, error, loading } = useApi<AdminOverview>("/overview");

  if (loading) return <Loading />;
  if (error) return <ErrorMessage message={error} />;
  if (!data) return null;

  return (
    <div>
      <PageHeader title="Overview" subtitle="Across every tenant in this deployment." />
      <div className="mb-8 grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-5">
        <StatCard label="Agents" value={data.total_agents} />
        <StatCard label="Threads" value={data.total_threads} />
        <StatCard label="Runs" value={data.total_runs} />
        <StatCard label="Connectors" value={data.connector_count} />
        <StatCard label="Cron schedules" value={data.cron_schedule_count} />
      </div>
      <div className="grid grid-cols-1 gap-6 md:grid-cols-2">
        <Card>
          <h3 className="mb-3 text-sm font-medium text-slate-400">Threads by status</h3>
          <StatusBreakdown breakdown={data.threads_by_status} />
        </Card>
        <Card>
          <h3 className="mb-3 text-sm font-medium text-slate-400">Runs by status</h3>
          <StatusBreakdown breakdown={data.runs_by_status} />
        </Card>
      </div>
    </div>
  );
}

function StatusBreakdown({ breakdown }: { breakdown: Record<string, number> }) {
  const entries = Object.entries(breakdown);
  if (entries.length === 0) return <p className="text-sm text-slate-500">None yet.</p>;
  return (
    <ul className="space-y-2">
      {entries.map(([status, count]) => (
        <li key={status} className="flex items-center justify-between">
          <StatusBadge status={status} />
          <span className="text-sm font-medium text-slate-200">{count}</span>
        </li>
      ))}
    </ul>
  );
}
