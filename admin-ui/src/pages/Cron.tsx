import { useApi } from "../api/useApi";
import type { AdminCronSchedule } from "../api/types";
import { EmptyState, ErrorMessage, formatTimestamp, Loading, PageHeader, StatusBadge, Table, Td, Th, Tr } from "../components/ui";

export function Cron() {
  const { data, error, loading } = useApi<AdminCronSchedule[]>("/cron");

  if (loading) return <Loading />;
  if (error) return <ErrorMessage message={error} />;
  if (!data || data.length === 0) return <EmptyState message="No cron schedules configured." />;

  return (
    <div>
      <PageHeader title="Cron schedules" subtitle="Across every tenant." />
      <Table>
        <thead>
          <tr>
            <Th>Name</Th>
            <Th>Agent</Th>
            <Th>Expression</Th>
            <Th>Timezone</Th>
            <Th>Tenant</Th>
            <Th>Status</Th>
            <Th>Updated</Th>
          </tr>
        </thead>
        <tbody>
          {data.map((c) => (
            <Tr key={`${c.tenant_id ?? "default"}:${c.name}`}>
              <Td>{c.name}</Td>
              <Td>{c.agent_id}</Td>
              <Td className="font-mono text-xs">{c.expression}</Td>
              <Td className="text-slate-400">{c.timezone || "UTC"}</Td>
              <Td>{c.tenant_id ?? "default"}</Td>
              <Td>
                <StatusBadge status={c.enabled ? "idle" : "error"} />
                <span className="ml-1 text-xs text-slate-400">{c.enabled ? "enabled" : "disabled"}</span>
              </Td>
              <Td className="text-slate-400">{formatTimestamp(c.updated_at)}</Td>
            </Tr>
          ))}
        </tbody>
      </Table>
    </div>
  );
}
