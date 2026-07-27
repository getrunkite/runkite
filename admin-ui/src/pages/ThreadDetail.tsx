import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api, ApiError } from "../api/client";
import { useApi } from "../api/useApi";
import type { AdminRun, AdminThread } from "../api/types";
import { Button, Card, ErrorMessage, formatTimestamp, Loading, PageHeader, StatusBadge, Table, Td, Th, Tr } from "../components/ui";

export function ThreadDetail() {
  const { threadId } = useParams<{ threadId: string }>();
  const navigate = useNavigate();
  const thread = useApi<AdminThread>(`/threads/${threadId}`, [threadId]);
  const runs = useApi<AdminRun[]>(`/threads/${threadId}/runs`, [threadId]);
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);

  const handleDelete = async () => {
    if (!threadId) return;
    if (!window.confirm(`Delete thread ${threadId}? This also deletes its runs and checkpoints.`)) return;
    setDeleting(true);
    setDeleteError(null);
    try {
      await api.del(`/threads/${threadId}`);
      navigate("/admin/threads");
    } catch (err) {
      setDeleteError(err instanceof ApiError ? err.message : "Failed to delete thread.");
      setDeleting(false);
    }
  };

  if (thread.loading) return <Loading />;
  if (thread.error) return <ErrorMessage message={thread.error} />;
  if (!thread.data) return null;

  return (
    <div>
      <div className="mb-6 flex items-start justify-between">
        <PageHeader title="Thread" subtitle={thread.data.thread_id} />
        <Button variant="danger" onClick={handleDelete} disabled={deleting}>
          {deleting ? "Deleting..." : "Delete thread"}
        </Button>
      </div>
      {deleteError && (
        <div className="mb-4">
          <ErrorMessage message={deleteError} />
        </div>
      )}
      <div className="mb-6 grid grid-cols-1 gap-6 md:grid-cols-2">
        <Card>
          <h3 className="mb-3 text-sm font-medium text-slate-400">Details</h3>
          <dl className="space-y-1 text-sm">
            <Row label="Status"><StatusBadge status={thread.data.status} /></Row>
            <Row label="Tenant">{thread.data.tenant_id}</Row>
            <Row label="Created">{formatTimestamp(thread.data.created_at)}</Row>
            <Row label="Updated">{formatTimestamp(thread.data.updated_at)}</Row>
          </dl>
        </Card>
        <Card>
          <h3 className="mb-3 text-sm font-medium text-slate-400">Values</h3>
          <pre className="max-h-64 overflow-auto rounded bg-slate-950 p-3 text-xs text-slate-300">
            {JSON.stringify(thread.data.values ?? {}, null, 2)}
          </pre>
        </Card>
      </div>

      <h3 className="mb-3 text-sm font-medium text-slate-400">Runs on this thread</h3>
      {runs.loading && <Loading />}
      {runs.error && <ErrorMessage message={runs.error} />}
      {runs.data && runs.data.length > 0 && (
        <Table>
          <thead>
            <tr>
              <Th>Run ID</Th>
              <Th>Agent</Th>
              <Th>Status</Th>
              <Th>Updated</Th>
            </tr>
          </thead>
          <tbody>
            {runs.data.map((run) => (
              <Tr key={run.run_id}>
                <Td className="font-mono text-xs">
                  <Link to={`/admin/runs/${run.run_id}`} className="text-indigo-400 hover:underline">
                    {run.run_id}
                  </Link>
                </Td>
                <Td>{run.agent_id}</Td>
                <Td>
                  <StatusBadge status={run.status} />
                </Td>
                <Td className="text-slate-400">{formatTimestamp(run.updated_at)}</Td>
              </Tr>
            ))}
          </tbody>
        </Table>
      )}
      {runs.data && runs.data.length === 0 && <p className="text-sm text-slate-500">No runs yet.</p>}
    </div>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex justify-between border-b border-slate-800/60 py-1.5 last:border-0">
      <dt className="text-slate-400">{label}</dt>
      <dd className="text-slate-200">{children}</dd>
    </div>
  );
}
