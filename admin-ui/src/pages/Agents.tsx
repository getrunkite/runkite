import { useApi } from "../api/useApi";
import type { AdminAgent } from "../api/types";
import { EmptyState, ErrorMessage, Loading, PageHeader, Table, Td, Th, Tr } from "../components/ui";

export function Agents() {
  const { data, error, loading } = useApi<AdminAgent[]>("/agents");

  if (loading) return <Loading />;
  if (error) return <ErrorMessage message={error} />;
  if (!data || data.length === 0) return <EmptyState message="No agents registered." />;

  return (
    <div>
      <PageHeader title="Agents" subtitle={`${data.length} registered across every tenant.`} />
      <Table>
        <thead>
          <tr>
            <Th>Agent ID</Th>
            <Th>Name</Th>
            <Th>Tenant</Th>
            <Th>Version</Th>
            <Th>Description</Th>
          </tr>
        </thead>
        <tbody>
          {data.map((agent) => (
            <Tr key={`${agent.tenant_id}:${agent.agent_id}`}>
              <Td className="font-mono text-xs">{agent.agent_id}</Td>
              <Td>{agent.name}</Td>
              <Td>{agent.tenant_id}</Td>
              <Td>{agent.version}</Td>
              <Td className="text-slate-400">{agent.description || "—"}</Td>
            </Tr>
          ))}
        </tbody>
      </Table>
    </div>
  );
}
