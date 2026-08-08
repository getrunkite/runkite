import { useState } from "react";
import { Link } from "react-router";
import { Shield } from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";
import { useApi } from "../api/useApi";
import type { AdminAuditEvent } from "../api/types";
import { EmptyState, ErrorState, formatRelativeTime, formatTimestamp, PageHeader, StatusBadge } from "../components/common";
import { DataTable } from "../components/data-table";
import { ListPager, adminListPath } from "../components/list-pager";
import { Badge } from "../components/ui/badge";
import { Input } from "../components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../components/ui/select";
import { Tooltip, TooltipContent, TooltipTrigger } from "../components/ui/tooltip";

const DECISION_OPTIONS = ["all", "allow", "deny", "pending"];

const columns: ColumnDef<AdminAuditEvent, unknown>[] = [
  {
    accessorKey: "ts",
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
    accessorKey: "decision",
    header: "Decision",
    cell: ({ getValue }) => <StatusBadge status={getValue() as string} />,
  },
  {
    accessorKey: "tenant_id",
    header: "Tenant",
    cell: ({ getValue }) => <Badge variant="outline">{getValue() as string}</Badge>,
  },
  { accessorKey: "action", header: "Action" },
  {
    accessorKey: "agent_id",
    header: "Agent",
    cell: ({ getValue }) => {
      const v = getValue() as string | undefined;
      return v ? <span className="font-mono text-xs">{v}</span> : <span className="text-muted-foreground">—</span>;
    },
  },
  {
    accessorKey: "connector",
    header: "Connector",
    cell: ({ getValue }) => {
      const v = getValue() as string | undefined;
      return v ? <span className="font-mono text-xs">{v}</span> : <span className="text-muted-foreground">—</span>;
    },
  },
  {
    accessorKey: "tool",
    header: "Tool",
    cell: ({ getValue }) => {
      const v = getValue() as string | undefined;
      return v ? <span className="font-mono text-xs">{v}</span> : <span className="text-muted-foreground">—</span>;
    },
  },
  {
    accessorKey: "reason_code",
    header: "Reason",
    cell: ({ getValue }) => {
      const v = getValue() as string | undefined;
      return v ? (
        <span className="max-w-[12rem] truncate font-mono text-xs text-muted-foreground" title={v}>
          {v}
        </span>
      ) : (
        <span className="text-muted-foreground">—</span>
      );
    },
  },
  {
    accessorKey: "run_id",
    header: "Run",
    cell: ({ row }) =>
      row.original.run_id ? (
        <Link
          to={`/admin/runs/${row.original.run_id}`}
          onClick={(e) => e.stopPropagation()}
          className="font-mono text-xs font-medium text-primary hover:underline"
        >
          {row.original.run_id}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
];

export function Audit() {
  const [tenantId, setTenantId] = useState("");
  const [decision, setDecision] = useState("all");
  const [connector, setConnector] = useState("");
  const [cursorStack, setCursorStack] = useState<string[]>([]);
  const cursor = cursorStack.length ? cursorStack[cursorStack.length - 1] : undefined;

  const extra: Record<string, string> = {};
  const tenant = tenantId.trim();
  const conn = connector.trim();
  if (tenant) extra.tenant_id = tenant;
  if (decision !== "all") extra.decision = decision;
  if (conn) extra.connector = conn;

  const path = adminListPath("/audit-events", cursor, Object.keys(extra).length ? extra : undefined);
  const { data, error, loading, nextCursor } = useApi<AdminAuditEvent[]>(path, [tenant, decision, conn, cursor]);
  const onFirstPage = cursorStack.length === 0;
  const hasFilter = tenant !== "" || decision !== "all" || conn !== "";

  const clearFilters = () => {
    setTenantId("");
    setDecision("all");
    setConnector("");
    setCursorStack([]);
  };

  return (
    <div>
      <PageHeader
        title="Audit"
        subtitle="Policy decisions across every tenant (SQL backends). Click any column to sort the current page."
      />

      <div className="mb-4 flex flex-wrap items-center gap-3">
        <Input
          className="w-44"
          placeholder="Tenant ID"
          value={tenantId}
          onChange={(e) => {
            setTenantId(e.target.value);
            setCursorStack([]);
          }}
        />
        <Select
          value={decision}
          onValueChange={(v) => {
            setDecision(v);
            setCursorStack([]);
          }}
        >
          <SelectTrigger className="w-44">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {DECISION_OPTIONS.map((d) => (
              <SelectItem key={d} value={d}>
                {d === "all" ? "All decisions" : d}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Input
          className="w-44"
          placeholder="Connector"
          value={connector}
          onChange={(e) => {
            setConnector(e.target.value);
            setCursorStack([]);
          }}
        />
      </div>

      {error && !data && <ErrorState message={error} />}
      {data && data.length === 0 && onFirstPage && (
        <EmptyState
          icon={Shield}
          title={hasFilter ? "No matching decisions" : "No audit events yet"}
          message={
            hasFilter
              ? "No policy decisions match these filters. Try clearing them, or confirm policy is enabled on a SQL state backend."
              : "Decisions appear here when policy is configured and audit writes are enabled (Postgres, MySQL, or SQLite)."
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
            getRowId={(e) => e.id}
            loading={loading}
            initialSorting={[{ id: "ts", desc: true }]}
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
