import { useState } from "react";
import { Link } from "react-router-dom";
import { api, ApiError } from "../api/client";
import { useApi } from "../api/useApi";
import type { AdminWebhookDeadLetter } from "../api/types";
import { Button, EmptyState, ErrorMessage, formatTimestamp, Loading, PageHeader, Table, Td, Th, Tr } from "../components/ui";

interface RedeliverResult {
  delivered: boolean;
  status_code?: number;
  error?: string;
}

export function Webhooks() {
  const { data, error, loading } = useApi<AdminWebhookDeadLetter[]>("/webhooks/dead-letters");
  const [redelivering, setRedelivering] = useState<string | null>(null);
  // Per-row feedback (not persisted -- see handleRedeliverWebhook's doc
  // comment: a successful redelivery doesn't remove the dead letter from
  // the list, so this is the only signal the UI has to show).
  const [results, setResults] = useState<Record<string, RedeliverResult | string>>({});

  const handleRedeliver = async (id: string) => {
    setRedelivering(id);
    setResults((prev) => ({ ...prev, [id]: undefined as unknown as RedeliverResult }));
    try {
      const result = await api.post<RedeliverResult>(`/webhooks/dead-letters/${id}/redeliver`);
      setResults((prev) => ({ ...prev, [id]: result }));
    } catch (err) {
      setResults((prev) => ({ ...prev, [id]: err instanceof ApiError ? err.message : "Redelivery failed." }));
    } finally {
      setRedelivering(null);
    }
  };

  if (loading) return <Loading />;
  if (error) return <ErrorMessage message={error} />;
  if (!data || data.length === 0) return <EmptyState message="No failed webhook deliveries -- nothing to see here." />;

  return (
    <div>
      <PageHeader
        title="Webhook dead-letters"
        subtitle="Deliveries that exhausted every retry. Redelivery re-POSTs the stored payload unsigned (see the API's doc comment) and doesn't remove the entry below -- it's a manual retry, not a resolution."
      />
      <Table>
        <thead>
          <tr>
            <Th>URL</Th>
            <Th>Event</Th>
            <Th>Run</Th>
            <Th>Attempts</Th>
            <Th>Error</Th>
            <Th>Failed at</Th>
            <Th>Redeliver</Th>
          </tr>
        </thead>
        <tbody>
          {data.map((dl) => {
            const result = results[dl.id];
            return (
              <Tr key={dl.id}>
                <Td className="max-w-xs truncate">{dl.url}</Td>
                <Td>{dl.event_type}</Td>
                <Td className="font-mono text-xs">
                  <Link to={`/admin/runs/${dl.run_id}`} className="text-indigo-400 hover:underline">
                    {dl.run_id}
                  </Link>
                </Td>
                <Td>{dl.attempts}</Td>
                <Td className="max-w-xs truncate text-red-400">{dl.error}</Td>
                <Td className="text-slate-400">{formatTimestamp(dl.failed_at)}</Td>
                <Td>
                  <div className="flex items-center gap-2">
                    <Button onClick={() => handleRedeliver(dl.id)} disabled={redelivering === dl.id}>
                      {redelivering === dl.id ? "Sending..." : "Redeliver"}
                    </Button>
                    {typeof result === "string" && <span className="text-xs text-red-400">{result}</span>}
                    {typeof result === "object" && result?.delivered && (
                      <span className="text-xs text-emerald-400">Delivered ({result.status_code})</span>
                    )}
                    {typeof result === "object" && result && !result.delivered && (
                      <span className="text-xs text-red-400">{result.error ?? "Failed"}</span>
                    )}
                  </div>
                </Td>
              </Tr>
            );
          })}
        </tbody>
      </Table>
    </div>
  );
}
