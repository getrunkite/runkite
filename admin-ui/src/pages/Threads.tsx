import { Link } from "react-router-dom";
import { useApi } from "../api/useApi";
import type { AdminThread } from "../api/types";
import { EmptyState, ErrorMessage, formatTimestamp, Loading, PageHeader, StatusBadge, Table, Td, Th, Tr } from "../components/ui";

export function Threads() {
  const { data, error, loading } = useApi<AdminThread[]>("/threads");

  if (loading) return <Loading />;
  if (error) return <ErrorMessage message={error} />;
  if (!data || data.length === 0) return <EmptyState message="No threads yet." />;

  return (
    <div>
      <PageHeader title="Threads" subtitle={`${data.length} across every tenant.`} />
      <Table>
        <thead>
          <tr>
            <Th>Thread ID</Th>
            <Th>Status</Th>
            <Th>Tenant</Th>
            <Th>Updated</Th>
          </tr>
        </thead>
        <tbody>
          {data.map((thread) => (
            <Tr key={thread.thread_id}>
              <Td className="font-mono text-xs">
                <Link to={`/admin/threads/${thread.thread_id}`} className="text-indigo-400 hover:underline">
                  {thread.thread_id}
                </Link>
              </Td>
              <Td>
                <StatusBadge status={thread.status} />
              </Td>
              <Td>{thread.tenant_id}</Td>
              <Td className="text-slate-400">{formatTimestamp(thread.updated_at)}</Td>
            </Tr>
          ))}
        </tbody>
      </Table>
    </div>
  );
}
