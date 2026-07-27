import { useState } from "react";
import { Link } from "react-router-dom";
import { useApi } from "../api/useApi";
import type { AdminRun } from "../api/types";
import { EmptyState, ErrorMessage, formatTimestamp, Loading, PageHeader, StatusBadge, Table, Td, Th, Tr } from "../components/ui";

const STATUS_OPTIONS = ["", "pending", "running", "success", "error", "timeout", "interrupted"];

export function Runs() {
  const [status, setStatus] = useState("");
  const path = status ? `/runs?status=${encodeURIComponent(status)}` : "/runs";
  const { data, error, loading } = useApi<AdminRun[]>(path, [status]);

  return (
    <div>
      <PageHeader title="Runs" subtitle="Across every tenant." />
      <div className="mb-4">
        <select
          value={status}
          onChange={(e) => setStatus(e.target.value)}
          className="rounded-md border border-slate-700 bg-slate-800 px-3 py-1.5 text-sm text-slate-200"
        >
          {STATUS_OPTIONS.map((s) => (
            <option key={s} value={s}>
              {s === "" ? "All statuses" : s}
            </option>
          ))}
        </select>
      </div>

      {loading && <Loading />}
      {error && <ErrorMessage message={error} />}
      {data && data.length === 0 && <EmptyState message="No runs match this filter." />}
      {data && data.length > 0 && (
        <Table>
          <thead>
            <tr>
              <Th>Run ID</Th>
              <Th>Agent</Th>
              <Th>Thread</Th>
              <Th>Status</Th>
              <Th>Tenant</Th>
              <Th>Updated</Th>
            </tr>
          </thead>
          <tbody>
            {data.map((run) => (
              <Tr key={run.run_id}>
                <Td className="font-mono text-xs">
                  <Link to={`/admin/runs/${run.run_id}`} className="text-indigo-400 hover:underline">
                    {run.run_id}
                  </Link>
                </Td>
                <Td>{run.agent_id}</Td>
                <Td className="font-mono text-xs">
                  {run.thread_id && (
                    <Link to={`/admin/threads/${run.thread_id}`} className="text-indigo-400 hover:underline">
                      {run.thread_id}
                    </Link>
                  )}
                </Td>
                <Td>
                  <StatusBadge status={run.status} />
                </Td>
                <Td>{run.tenant_id}</Td>
                <Td className="text-slate-400">{formatTimestamp(run.updated_at)}</Td>
              </Tr>
            ))}
          </tbody>
        </Table>
      )}
    </div>
  );
}
