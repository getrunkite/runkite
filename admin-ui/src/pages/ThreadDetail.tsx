import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { Loader2, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { api, ApiError } from "../api/client";
import { useApi } from "../api/useApi";
import type { AdminRun, AdminThread } from "../api/types";
import { EmptyState, ErrorState, formatTimestamp, PageHeader, StatusBadge, TableSkeleton } from "../components/common";
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
import { Skeleton } from "../components/ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../components/ui/table";

export function ThreadDetail() {
  const { threadId } = useParams<{ threadId: string }>();
  const navigate = useNavigate();
  const thread = useApi<AdminThread>(`/threads/${threadId}`, [threadId]);
  const runs = useApi<AdminRun[]>(`/threads/${threadId}/runs`, [threadId]);
  const [deleting, setDeleting] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);

  const handleDelete = async () => {
    if (!threadId) return;
    setDeleting(true);
    try {
      await api.del(`/threads/${threadId}`);
      toast.success("Thread deleted", { description: threadId });
      navigate("/admin/threads");
    } catch (err) {
      toast.error("Failed to delete thread", {
        description: err instanceof ApiError ? err.message : "Request failed.",
      });
      setDeleting(false);
      setConfirmOpen(false);
    }
  };

  if (thread.loading) return <ThreadDetailSkeleton />;
  if (thread.error) return <ErrorState message={thread.error} />;
  if (!thread.data) return null;

  return (
    <div>
      <PageHeader
        title="Thread"
        subtitle={
          <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <Link to="/admin/threads" className="text-primary hover:underline">
              ← Threads
            </Link>
            <span className="text-muted-foreground/40">·</span>
            <code className="font-mono text-xs">{thread.data.thread_id}</code>
          </span>
        }
        actions={
          <Dialog open={confirmOpen} onOpenChange={setConfirmOpen}>
            <DialogTrigger asChild>
              <Button variant="destructive">
                <Trash2 className="size-3.5" />
                Delete thread
              </Button>
            </DialogTrigger>
            <DialogContent>
              <DialogHeader>
                <DialogTitle>Delete this thread?</DialogTitle>
                <DialogDescription>
                  This permanently deletes <code className="font-mono">{thread.data.thread_id}</code> and every
                  checkpoint associated with it. This cannot be undone.
                </DialogDescription>
              </DialogHeader>
              <DialogFooter>
                <Button variant="outline" onClick={() => setConfirmOpen(false)} disabled={deleting}>
                  Cancel
                </Button>
                <Button variant="destructive" onClick={handleDelete} disabled={deleting}>
                  {deleting && <Loader2 className="size-3.5 animate-spin" />}
                  Delete permanently
                </Button>
              </DialogFooter>
            </DialogContent>
          </Dialog>
        }
      />

      <div className="mb-6 grid grid-cols-1 gap-4 md:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Details</CardTitle>
          </CardHeader>
          <CardContent>
            <DetailRow label="Status">
              <StatusBadge status={thread.data.status} />
            </DetailRow>
            <DetailRow label="Tenant">
              <Badge variant="outline">{thread.data.tenant_id}</Badge>
            </DetailRow>
            <DetailRow label="Created">{formatTimestamp(thread.data.created_at)}</DetailRow>
            <DetailRow label="Updated" last>
              {formatTimestamp(thread.data.updated_at)}
            </DetailRow>
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Values</CardTitle>
          </CardHeader>
          <CardContent>
            <pre className="max-h-64 overflow-auto rounded-lg bg-muted/50 p-3 font-mono text-xs">
              {JSON.stringify(thread.data.values ?? {}, null, 2)}
            </pre>
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Runs on this thread</CardTitle>
        </CardHeader>
        <CardContent>
          {runs.error && <ErrorState message={runs.error} />}
          {runs.data && runs.data.length === 0 && (
            <EmptyState
              title="No runs on this thread"
              message="Start a run against this thread_id from your client or SDK — status and stream output will appear here."
            />
          )}
          {(runs.loading || (runs.data && runs.data.length > 0)) && (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Run ID</TableHead>
                  <TableHead>Agent</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Updated</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {runs.loading && !runs.data && <TableSkeleton columns={4} rows={3} />}
                {runs.data?.map((run) => (
                  <TableRow key={run.run_id}>
                    <TableCell className="font-mono text-xs">
                      <Link to={`/admin/runs/${run.run_id}`} className="font-medium text-primary hover:underline">
                        {run.run_id}
                      </Link>
                    </TableCell>
                    <TableCell>{run.agent_id}</TableCell>
                    <TableCell>
                      <StatusBadge status={run.status} />
                    </TableCell>
                    <TableCell className="text-muted-foreground">{formatTimestamp(run.updated_at)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
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

function ThreadDetailSkeleton() {
  return (
    <div>
      <Skeleton className="mb-2 h-8 w-32" />
      <Skeleton className="mb-6 h-4 w-64" />
      <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
        <Skeleton className="h-40" />
        <Skeleton className="h-40" />
      </div>
    </div>
  );
}