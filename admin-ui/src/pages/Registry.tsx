import { useApi } from "../api/useApi";
import type { AdminRegistryEntry } from "../api/types";
import { EmptyState, ErrorMessage, Loading, PageHeader, Table, Td, Th, Tr } from "../components/ui";

export function Registry() {
  const { data, error, loading } = useApi<AdminRegistryEntry[]>("/registry");

  if (loading) return <Loading />;
  if (error) return <ErrorMessage message={error} />;
  if (!data || data.length === 0) return <EmptyState message="No registry entries published yet." />;

  return (
    <div>
      <PageHeader
        title="Registry"
        subtitle={`${data.length} published across every tenant. A metadata catalog -- source_ref is where to actually deploy an entry, not something this control plane executes.`}
      />
      <Table>
        <thead>
          <tr>
            <Th>Name</Th>
            <Th>Display Name</Th>
            <Th>Tenant</Th>
            <Th>Author</Th>
            <Th>Tags</Th>
            <Th>Source</Th>
            <Th>Version</Th>
          </tr>
        </thead>
        <tbody>
          {data.map((entry) => (
            <Tr key={`${entry.tenant_id}:${entry.name}`}>
              <Td className="font-mono text-xs">{entry.name}</Td>
              <Td>{entry.display_name || "—"}</Td>
              <Td>{entry.tenant_id}</Td>
              <Td>{entry.author || "—"}</Td>
              <Td className="text-slate-400">{entry.tags?.join(", ") || "—"}</Td>
              <Td className="max-w-xs truncate text-xs">
                {entry.source_type}: {entry.source_ref}
              </Td>
              <Td>{entry.version}</Td>
            </Tr>
          ))}
        </tbody>
      </Table>
    </div>
  );
}
