import { useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router";
import { Loader2, Radio, XCircle } from "lucide-react";
import { toast } from "sonner";
import { api, ApiError } from "../api/client";
import { useApi } from "../api/useApi";
import { streamSSE, type SseEvent } from "../api/sse";
import type { AdminRun } from "../api/types";
import { ErrorState, formatTimestamp, PageHeader, StatusBadge } from "../components/common";
import { adminListPath } from "../components/list-pager";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "../components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "../components/ui/dialog";
import { ScrollArea } from "../components/ui/scroll-area";
import { Skeleton } from "../components/ui/skeleton";

interface LogEntry extends SseEvent {
  receivedAt: number;
}

// Mirrors internal/api/runs.go's isTerminalStatus -- a run in one of
// these statuses has already finished; canceling it again is a no-op the
// UI shouldn't offer.
const TERMINAL_STATUSES = new Set(["success", "error", "interrupted", "timeout"]);

const EVENT_COLORS: Record<string, string> = {
  end: "text-success",
  error: "text-destructive",
  interrupted: "text-warning",
};

export function RunDetail() {
  const { runId } = useParams<{ runId: string }>();
  const run = useApi<AdminRun>(`/runs/${runId}`, [runId]);
  const childrenPath = runId ? adminListPath("/runs", undefined, { parent_run_id: runId }) : "";
  const children = useApi<AdminRun[]>(childrenPath, [runId]);
  const [events, setEvents] = useState<LogEntry[]>([]);
  const [streaming, setStreaming] = useState(true);
  const [cancelling, setCancelling] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const logEndRef = useRef<HTMLDivElement>(null);

  const handleCancel = async () => {
    if (!runId) return;
    setCancelling(true);
    try {
      await api.post(`/runs/${runId}/cancel`);
      toast.success("Cancel requested", { description: runId });
      setConfirmOpen(false);
      run.reload();
    } catch (err) {
      toast.error("Failed to cancel run", {
        description: err instanceof ApiError ? err.message : "Request failed.",
      });
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

  if (run.loading) return <RunDetailSkeleton />;
  if (run.error) return <ErrorState message={run.error} />;
  if (!run.data) return null;

  const isTerminal = TERMINAL_STATUSES.has(run.data.status);
  const childRuns = children.data ?? [];
  // Show A2A panel for delegated children immediately; for parents wait until
  // the child-run query finishes so ordinary runs don't flash an empty card.
  const showA2A =
    Boolean(run.data.parent_run_id) ||
    (run.data.depth ?? 0) > 0 ||
    (!children.loading && childRuns.length > 0);

  return (
    <div>
      <PageHeader
        title="Run"
        subtitle={
          <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <Link to="/admin/runs" className="text-primary hover:underline">
              ← Runs
            </Link>
            <span className="text-muted-foreground/40">·</span>
            <code className="font-mono text-xs">{run.data.run_id}</code>
          </span>
        }
        actions={
          !isTerminal && (
            <Dialog open={confirmOpen} onOpenChange={setConfirmOpen}>
              <DialogTrigger asChild>
                <Button variant="destructive">
                  <XCircle className="size-3.5" />
                  Cancel run
                </Button>
              </DialogTrigger>
              <DialogContent>
                <DialogHeader>
                  <DialogTitle>Cancel this run?</DialogTitle>
                  <DialogDescription>
                    Requests a graceful stop of <code className="font-mono">{run.data!.run_id}</code>. The runner
                    finishes its current step before the run transitions to <code>interrupted</code>.
                  </DialogDescription>
                </DialogHeader>
                <DialogFooter>
                  <Button variant="outline" onClick={() => setConfirmOpen(false)} disabled={cancelling}>
                    Keep running
                  </Button>
                  <Button variant="destructive" onClick={handleCancel} disabled={cancelling}>
                    {cancelling && <Loader2 className="size-3.5 animate-spin" />}
                    Cancel run
                  </Button>
                </DialogFooter>
              </DialogContent>
            </Dialog>
          )
        }
      />

      <div className="mb-4 grid grid-cols-1 gap-4 md:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Details</CardTitle>
          </CardHeader>
          <CardContent>
            <DetailRow label="Status">
              <StatusBadge status={run.data.status} />
            </DetailRow>
            <DetailRow label="Agent">{run.data.agent_id}</DetailRow>
            <DetailRow label="Tenant">
              <Badge variant="outline">{run.data.tenant_id}</Badge>
            </DetailRow>
            <DetailRow label="Thread">
              {run.data.thread_id ? (
                <Link to={`/admin/threads/${run.data.thread_id}`} className="font-mono text-xs text-primary hover:underline">
                  {run.data.thread_id}
                </Link>
              ) : (
                <span className="text-muted-foreground">—</span>
              )}
            </DetailRow>
            {run.data.parent_run_id && (
              <DetailRow label="Parent run">
                <Link
                  to={`/admin/runs/${run.data.parent_run_id}`}
                  className="font-mono text-xs text-primary hover:underline"
                >
                  {run.data.parent_run_id}
                </Link>
              </DetailRow>
            )}
            {(run.data.depth ?? 0) > 0 && (
              <DetailRow label="A2A depth">{run.data.depth}</DetailRow>
            )}
            <DetailRow label="Created">{formatTimestamp(run.data.created_at)}</DetailRow>
            <DetailRow label="Updated" last>
              {formatTimestamp(run.data.updated_at)}
            </DetailRow>
            {run.data.error && (
              <div className="mt-3">
                <ErrorState message={run.data.error} />
              </div>
            )}
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Input</CardTitle>
          </CardHeader>
          <CardContent>
            <pre className="max-h-48 overflow-auto rounded-lg bg-muted/50 p-3 font-mono text-xs">
              {JSON.stringify(run.data.input ?? {}, null, 2)}
            </pre>
          </CardContent>
        </Card>
      </div>

      {showA2A && (
        <Card className="mb-4">
          <CardHeader>
            <CardTitle>Agent-to-agent</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3 text-sm">
            {run.data.parent_run_id ? (
              <p>
                This run was delegated from{" "}
                <Link to={`/admin/runs/${run.data.parent_run_id}`} className="font-mono text-xs text-primary hover:underline">
                  {run.data.parent_run_id}
                </Link>
                {run.data.agent_id ? (
                  <>
                    {" "}
                    (<span className="font-medium">{run.data.agent_id}</span> as child).
                  </>
                ) : (
                  "."
                )}
              </p>
            ) : (
              <p className="text-muted-foreground">
                Top-level run. Child runs created via A2A show up below when this agent delegated mid-flight.
              </p>
            )}
            {children.loading && <p className="text-muted-foreground">Loading child runs…</p>}
            {!children.loading && childRuns.length === 0 && !run.data.parent_run_id && (
              <p className="text-muted-foreground">No child runs for this run.</p>
            )}
            {childRuns.length > 0 && (
              <div className="overflow-hidden rounded-lg border border-border/60">
                <table className="w-full text-left text-sm">
                  <thead className="bg-muted/40 text-muted-foreground">
                    <tr>
                      <th className="px-3 py-2 font-medium">Child run</th>
                      <th className="px-3 py-2 font-medium">Agent</th>
                      <th className="px-3 py-2 font-medium">Status</th>
                    </tr>
                  </thead>
                  <tbody>
                    {childRuns.map((c) => (
                      <tr key={c.run_id} className="border-t border-border/60">
                        <td className="px-3 py-2">
                          <Link
                            to={`/admin/runs/${c.run_id}`}
                            className="font-mono text-xs text-primary hover:underline"
                          >
                            {c.run_id}
                          </Link>
                        </td>
                        <td className="px-3 py-2">{c.agent_id || "—"}</td>
                        <td className="px-3 py-2">
                          <StatusBadge status={c.status} />
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader className="flex-row items-center justify-between">
          <CardTitle>Event stream</CardTitle>
          <Badge variant={streaming ? "success" : "muted"}>
            <Radio className={`size-3 ${streaming ? "animate-pulse" : ""}`} />
            {streaming ? "live" : "closed"}
          </Badge>
        </CardHeader>
        <CardContent>
          <ScrollArea className="h-96 rounded-lg border border-border/60 bg-muted/30">
            <div className="space-y-1 p-3 font-mono text-xs">
              {events.length === 0 && <p className="text-muted-foreground">Waiting for events...</p>}
              {events.map((entry, i) => (
                <div key={i} className="flex gap-2">
                  <span className="shrink-0 text-muted-foreground/70">
                    {new Date(entry.receivedAt).toLocaleTimeString()}
                  </span>
                  <span className={`shrink-0 font-semibold ${EVENT_COLORS[entry.event] ?? "text-primary"}`}>
                    {entry.event}
                  </span>
                  <span className="whitespace-pre-wrap break-all text-foreground/90">{entry.data}</span>
                </div>
              ))}
              <div ref={logEndRef} />
            </div>
          </ScrollArea>
        </CardContent>
      </Card>
    </div>
  );
}

function DetailRow({ label, children, last = false }: { label: string; children: React.ReactNode; last?: boolean }) {
  return (
    <div className={`flex items-center justify-between py-2 text-sm ${last ? "" : "border-b border-border/60"}`}>
      <dt className="text-muted-foreground">{label}</dt>
      <dd>{children}</dd>
    </div>
  );
}

function RunDetailSkeleton() {
  return (
    <div>
      <Skeleton className="mb-2 h-8 w-24" />
      <Skeleton className="mb-6 h-4 w-64" />
      <div className="mb-4 grid grid-cols-1 gap-4 md:grid-cols-2">
        <Skeleton className="h-48" />
        <Skeleton className="h-48" />
      </div>
      <Skeleton className="h-96" />
    </div>
  );
}
