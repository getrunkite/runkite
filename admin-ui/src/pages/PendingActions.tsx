import { useState } from "react";
import { Link } from "react-router";
import { Check, Loader2, ShieldAlert, X } from "lucide-react";
import { toast } from "sonner";
import type { ColumnDef } from "@tanstack/react-table";
import { api, ApiError } from "../api/client";
import { useApi } from "../api/useApi";
import type { AdminPendingAction } from "../api/types";
import { EmptyState, ErrorState, formatRelativeTime, formatTimestamp, PageHeader, StatusBadge, supportPage } from "../components/common";
import { DataTable } from "../components/data-table";
import { ListPager, adminListPath } from "../components/list-pager";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";
import { Input } from "../components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../components/ui/select";
import { Tooltip, TooltipContent, TooltipTrigger } from "../components/ui/tooltip";

const STATUS_OPTIONS = ["pending", "approved", "denied", "consumed", "all"];

export function PendingActions() {
  const [tenantId, setTenantId] = useState("");
  const [status, setStatus] = useState("pending");
  const [connector, setConnector] = useState("");
  const [runId, setRunId] = useState("");
  const [cursorStack, setCursorStack] = useState<string[]>([]);
  const cursor = cursorStack.length ? cursorStack[cursorStack.length - 1] : undefined;
  const [busyId, setBusyId] = useState<string | null>(null);

  const extra: Record<string, string> = {};
  const tenant = tenantId.trim();
  const conn = connector.trim();
  const run = runId.trim();
  if (tenant) extra.tenant_id = tenant;
  if (status !== "all") extra.status = status;
  if (conn) extra.connector = conn;
  if (run) extra.run_id = run;

  const path = adminListPath("/pending-actions", cursor, Object.keys(extra).length ? extra : undefined);
  const { data, error, loading, nextCursor, reload } = useApi<AdminPendingAction[]>(path, [
    tenant,
    status,
    conn,
    run,
    cursor,
  ]);
  const onFirstPage = cursorStack.length === 0;
  const hasFilter = tenant !== "" || status !== "pending" || conn !== "" || run !== "";

  const resolve = async (a: AdminPendingAction, verb: "approve" | "deny") => {
    setBusyId(a.id);
    try {
      await api.post(`/pending-actions/${encodeURIComponent(a.id)}/${verb}`);
      toast.success(verb === "approve" ? "Approved" : "Denied", {
        description: `${a.connector}.${a.tool} on run ${a.run_id}`,
      });
      reload();
    } catch (err) {
      toast.error(verb === "approve" ? "Approve failed" : "Deny failed", {
        description: err instanceof ApiError ? err.message : "Request failed.",
      });
    } finally {
      setBusyId(null);
    }
  };

  const columns: ColumnDef<AdminPendingAction, unknown>[] = [
    {
      accessorKey: "created_at",
      header: "When",
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
      accessorKey: "status",
      header: "Status",
      cell: ({ getValue }) => <StatusBadge status={getValue() as string} />,
    },
    {
      accessorKey: "tenant_id",
      header: "Tenant",
      cell: ({ getValue }) => <Badge variant="outline">{getValue() as string}</Badge>,
    },
    {
      accessorKey: "agent_id",
      header: "Agent",
      cell: ({ getValue }) => <span className="font-mono text-xs">{getValue() as string}</span>,
    },
    {
      accessorKey: "connector",
      header: "Connector",
      cell: ({ getValue }) => <span className="font-mono text-xs">{getValue() as string}</span>,
    },
    {
      accessorKey: "tool",
      header: "Tool",
      cell: ({ getValue }) => <span className="font-mono text-xs">{getValue() as string}</span>,
    },
    {
      accessorKey: "run_id",
      header: "Run",
      cell: ({ getValue }) => {
        const id = getValue() as string;
        return (
          <Link to={`/admin/runs/${encodeURIComponent(id)}`} className="font-mono text-xs text-primary hover:underline">
            {id}
          </Link>
        );
      },
    },
    {
      accessorKey: "reason_code",
      header: "Reason",
      cell: ({ row }) => {
        const code = row.original.reason_code;
        const reason = row.original.reason;
        const label = code || reason;
        if (!label) return <span className="text-muted-foreground">—</span>;
        return (
          <span className="max-w-[12rem] truncate font-mono text-xs text-muted-foreground" title={reason || code}>
            {label}
          </span>
        );
      },
    },
    {
      id: "actions",
      header: "",
      enableSorting: false,
      cell: ({ row }) => {
        const a = row.original;
        if (a.status !== "pending") return null;
        const busy = busyId === a.id;
        return (
          <div className="flex justify-end gap-1">
            <Button size="sm" variant="outline" disabled={busy} onClick={() => resolve(a, "approve")}>
              {busy ? <Loader2 className="size-3.5 animate-spin" /> : <Check className="size-3.5" />}
              Approve
            </Button>
            <Button size="sm" variant="ghost" disabled={busy} onClick={() => resolve(a, "deny")}>
              <X className="size-3.5" />
              Deny
            </Button>
          </div>
        );
      },
    },
  ];

  const clearFilters = () => {
    setTenantId("");
    setStatus("pending");
    setConnector("");
    setRunId("");
    setCursorStack([]);
  };

  return (
    <div>
      <PageHeader
        title="Pending actions"
        subtitle="Connector HITL queue (SQL backends). Approve mints a one-shot capability for the next matching tools/call."
      />

      <div className="mb-4 flex flex-wrap items-center gap-3">
        <Select
          value={status}
          onValueChange={(v) => {
            setStatus(v);
            setCursorStack([]);
          }}
        >
          <SelectTrigger className="w-40">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {STATUS_OPTIONS.map((s) => (
              <SelectItem key={s} value={s}>
                {s === "all" ? "All statuses" : s}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Input
          className="w-40"
          placeholder="Tenant ID"
          value={tenantId}
          onChange={(e) => {
            setTenantId(e.target.value);
            setCursorStack([]);
          }}
        />
        <Input
          className="w-40"
          placeholder="Connector"
          value={connector}
          onChange={(e) => {
            setConnector(e.target.value);
            setCursorStack([]);
          }}
        />
        <Input
          className="w-44"
          placeholder="Run ID"
          value={runId}
          onChange={(e) => {
            setRunId(e.target.value);
            setCursorStack([]);
          }}
        />
      </div>

      {error && !data && <ErrorState message={error} />}
      {data && data.length === 0 && onFirstPage && (
        <EmptyState
          icon={ShieldAlert}
          title={hasFilter ? "No matching actions" : "No pending actions"}
          message={
            hasFilter
              ? "Nothing matches these filters. Clear them, or confirm policy webhook returns effect pending on a SQL backend."
              : "Rows appear when a connector webhook returns effect pending. Requires a SQL state backend and an enabled policy webhook."
          }
          learnMore={
            hasFilter
              ? undefined
              : { href: supportPage("hitl-ops.html"), label: "Docs: HITL ops →" }
          }
          action={
            hasFilter ? (
              <button type="button" className="text-sm font-medium text-primary hover:underline" onClick={clearFilters}>
                Clear filters
              </button>
            ) : undefined
          }
        />
      )}
      {(data === null || (data && data.length > 0) || !onFirstPage) && !(data && data.length === 0 && onFirstPage) && (
        <>
          <DataTable
            columns={columns}
            data={data ?? []}
            getRowId={(a) => a.id}
            loading={loading}
            initialSorting={[{ id: "created_at", desc: true }]}
          />
          <ListPager
            pageIndex={cursorStack.length}
            pageCount={data?.length ?? 0}
            hasNext={Boolean(nextCursor)}
            onPrev={() => setCursorStack((s) => s.slice(0, -1))}
            onNext={() => {
              if (nextCursor) setCursorStack((s) => [...s, nextCursor]);
            }}
          />
        </>
      )}
    </div>
  );
}
