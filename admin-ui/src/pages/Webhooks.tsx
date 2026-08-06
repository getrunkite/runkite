import { useState } from "react";
import { Link } from "react-router";
import { Loader2, Send, Webhook as WebhookIcon } from "lucide-react";
import { toast } from "sonner";
import type { ColumnDef } from "@tanstack/react-table";
import { api, ApiError } from "../api/client";
import { useApi } from "../api/useApi";
import type { AdminWebhookDeadLetter } from "../api/types";
import { EmptyState, ErrorState, formatRelativeTime, formatTimestamp, PageHeader } from "../components/common";
import { DataTable } from "../components/data-table";
import { Button } from "../components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "../components/ui/tooltip";

interface RedeliverResult {
  delivered: boolean;
  status_code?: number;
  error?: string;
}

export function Webhooks() {
  const { data, error, loading } = useApi<AdminWebhookDeadLetter[]>("/webhooks/dead-letters");
  const [redelivering, setRedelivering] = useState<string | null>(null);

  const handleRedeliver = async (id: string) => {
    setRedelivering(id);
    try {
      const result = await api.post<RedeliverResult>(`/webhooks/dead-letters/${id}/redeliver`);
      if (result.delivered) {
        toast.success("Webhook redelivered", { description: `Receiver responded with ${result.status_code}.` });
      } else {
        toast.error("Redelivery failed", { description: result.error ?? "The receiver rejected the retry." });
      }
    } catch (err) {
      toast.error("Redelivery failed", {
        description: err instanceof ApiError ? err.message : "Request failed.",
      });
    } finally {
      setRedelivering(null);
    }
  };

  const columns: ColumnDef<AdminWebhookDeadLetter, unknown>[] = [
    {
      accessorKey: "tenant_id",
      header: "Tenant",
      cell: ({ getValue }) => <span className="font-mono text-xs">{getValue() as string}</span>,
    },
    {
      accessorKey: "url",
      header: "URL",
      cell: ({ getValue }) => <span className="max-w-48 truncate font-mono text-xs">{getValue() as string}</span>,
    },
    { accessorKey: "event_type", header: "Event" },
    {
      accessorKey: "run_id",
      header: "Run",
      cell: ({ getValue }) => (
        <Link
          to={`/admin/runs/${getValue() as string}`}
          className="font-mono text-xs text-primary hover:underline"
        >
          {getValue() as string}
        </Link>
      ),
    },
    { accessorKey: "attempts", header: "Attempts" },
    {
      accessorKey: "error",
      header: "Error",
      enableSorting: false,
      cell: ({ getValue }) => <span className="max-w-56 truncate text-destructive">{getValue() as string}</span>,
    },
    {
      accessorKey: "failed_at",
      header: "Failed",
      cell: ({ getValue }) => {
        const iso = getValue() as string;
        return (
          <Tooltip>
            <TooltipTrigger className="text-muted-foreground">{formatRelativeTime(iso)}</TooltipTrigger>
            <TooltipContent>{formatTimestamp(iso)}</TooltipContent>
          </Tooltip>
        );
      },
    },
    {
      id: "redeliver",
      header: "Redeliver",
      enableSorting: false,
      cell: ({ row }) => (
        <Button
          variant="outline"
          size="sm"
          onClick={() => handleRedeliver(row.original.id)}
          disabled={redelivering === row.original.id}
        >
          {redelivering === row.original.id ? (
            <Loader2 className="size-3.5 animate-spin" />
          ) : (
            <Send className="size-3.5" />
          )}
          Redeliver
        </Button>
      ),
    },
  ];

  return (
    <div>
      <PageHeader
        title="Webhook dead-letters"
        subtitle="Deliveries that exhausted every retry. Redelivery re-POSTs the stored payload unsigned and doesn't remove the entry below -- it's a manual retry, not a resolution."
      />
      {error && !data && <ErrorState message={error} />}
      {data && data.length === 0 && (
        <EmptyState
          icon={WebhookIcon}
          title="No dead-letters"
          message="Good — nothing exhausted retries. Failed deliveries land here after the webhook pipeline gives up; empty means healthy or no webhooks configured."
        />
      )}
      {(data === null || (data && data.length > 0)) && (
        <DataTable
          columns={columns}
          data={data ?? []}
          getRowId={(dl) => dl.id}
          loading={loading}
          initialSorting={[{ id: "failed_at", desc: true }]}
        />
      )}
    </div>
  );
}
