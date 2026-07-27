import { useApi } from "../api/useApi";
import type { AdminConnector } from "../api/types";
import { EmptyState, ErrorMessage, Loading, PageHeader, StatusBadge, Table, Td, Th, Tr } from "../components/ui";

export function Connectors() {
  const { data, error, loading } = useApi<AdminConnector[]>("/connectors");

  if (loading) return <Loading />;
  if (error) return <ErrorMessage message={error} />;
  if (!data || data.length === 0) return <EmptyState message="No connectors configured." />;

  return (
    <div>
      <PageHeader title="Connectors" subtitle="OAuth/MCP sessions runners pre-warm for their declared agents." />
      <Table>
        <thead>
          <tr>
            <Th>Name</Th>
            <Th>Type</Th>
            <Th>MCP endpoint</Th>
            <Th>Circuit breaker</Th>
          </tr>
        </thead>
        <tbody>
          {data.map((c) => (
            <Tr key={c.name}>
              <Td>{c.name}</Td>
              <Td className="font-mono text-xs">{c.type}</Td>
              <Td className="text-slate-400">{c.mcp || "—"}</Td>
              <Td>{c.circuit_breaker_state ? <StatusBadge status={c.circuit_breaker_state} /> : "—"}</Td>
            </Tr>
          ))}
        </tbody>
      </Table>
    </div>
  );
}
