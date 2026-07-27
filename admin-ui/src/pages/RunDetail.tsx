import { useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api, ApiError } from "../api/client";
import { useApi } from "../api/useApi";
import { streamSSE, type SseEvent } from "../api/sse";
import type { AdminRun } from "../api/types";
import { Button, Card, ErrorMessage, formatTimestamp, Loading, PageHeader, StatusBadge } from "../components/ui";

interface LogEntry extends SseEvent {
  receivedAt: number;
}

// Mirrors internal/api/runs.go's isTerminalStatus -- a run in one of
// these statuses has already finished; canceling it again is a no-op the
// UI shouldn't offer.
const TERMINAL_STATUSES = new Set(["success", "error", "interrupted", "timeout"]);

export function RunDetail() {
  const { runId } = useParams<{ runId: string }>();
  const run = useApi<AdminRun>(`/runs/${runId}`, [runId]);
  const [events, setEvents] = useState<LogEntry[]>([]);
  const [streaming, setStreaming] = useState(true);
  const [cancelling, setCancelling] = useState(false);
  const [cancelError, setCancelError] = useState<string | null>(null);
  const logEndRef = useRef<HTMLDivElement>(null);

  const handleCancel = async () => {
    if (!runId) return;
    setCancelling(true);
    setCancelError(null);
    try {
      await api.post(`/runs/${runId}/cancel`);
      run.reload();
    } catch (err) {
      setCancelError(err instanceof ApiError ? err.message : "Failed to cancel run.");
    } finally {
      setCancelling(false);
    }
  };

  useEffect(() => {
    if (!runId) return;
    setEvents([]);
    setStreaming(true);
    const abort = streamSSE(
      `/runs/${runId}/stream`,
      (event) => setEvents((prev) => [...prev, { ...event, receivedAt: Date.now() }]),
      () => setStreaming(false),
    );
    return abort;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runId]);

  useEffect(() => {
    logEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [events.length]);

  if (run.loading) return <Loading />;
  if (run.error) return <ErrorMessage message={run.error} />;
  if (!run.data) return null;

  const isTerminal = TERMINAL_STATUSES.has(run.data.status);

  return (
    <div>
      <div className="mb-6 flex items-start justify-between">
        <PageHeader title="Run" subtitle={run.data.run_id} />
        {!isTerminal && (
          <Button variant="danger" onClick={handleCancel} disabled={cancelling}>
            {cancelling ? "Cancelling..." : "Cancel run"}
          </Button>
        )}
      </div>
      {cancelError && (
        <div className="mb-4">
          <ErrorMessage message={cancelError} />
        </div>
      )}
      <div className="mb-6 grid grid-cols-1 gap-6 md:grid-cols-2">
        <Card>
          <h3 className="mb-3 text-sm font-medium text-slate-400">Details</h3>
          <dl className="space-y-1 text-sm">
            <Row label="Status"><StatusBadge status={run.data.status} /></Row>
            <Row label="Agent">{run.data.agent_id}</Row>
            <Row label="Tenant">{run.data.tenant_id}</Row>
            <Row label="Thread">
              {run.data.thread_id && (
                <Link to={`/admin/threads/${run.data.thread_id}`} className="text-indigo-400 hover:underline">
                  {run.data.thread_id}
                </Link>
              )}
            </Row>
            <Row label="Created">{formatTimestamp(run.data.created_at)}</Row>
            <Row label="Updated">{formatTimestamp(run.data.updated_at)}</Row>
          </dl>
          {run.data.error && (
            <div className="mt-3">
              <ErrorMessage message={run.data.error} />
            </div>
          )}
        </Card>
        <Card>
          <h3 className="mb-3 text-sm font-medium text-slate-400">Input</h3>
          <pre className="max-h-48 overflow-auto rounded bg-slate-950 p-3 text-xs text-slate-300">
            {JSON.stringify(run.data.input ?? {}, null, 2)}
          </pre>
        </Card>
      </div>

      <Card>
        <div className="mb-3 flex items-center justify-between">
          <h3 className="text-sm font-medium text-slate-400">Event stream (replayed + live)</h3>
          <span className={`text-xs ${streaming ? "text-emerald-400" : "text-slate-500"}`}>
            {streaming ? "● live" : "closed"}
          </span>
        </div>
        <div className="max-h-96 space-y-1 overflow-y-auto rounded bg-slate-950 p-3 font-mono text-xs">
          {events.length === 0 && <p className="text-slate-500">Waiting for events...</p>}
          {events.map((entry, i) => (
            <div key={i} className="flex gap-2">
              <span className="shrink-0 text-slate-500">{new Date(entry.receivedAt).toLocaleTimeString()}</span>
              <span className="shrink-0 font-semibold text-indigo-400">{entry.event}</span>
              <span className="whitespace-pre-wrap break-all text-slate-300">{entry.data}</span>
            </div>
          ))}
          <div ref={logEndRef} />
        </div>
      </Card>
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
