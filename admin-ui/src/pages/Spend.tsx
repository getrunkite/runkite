import { useMemo, useState } from "react";
import { DollarSign } from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";
import { useApi } from "../api/useApi";
import type { AdminUsageSummaryRow } from "../api/types";
import { EmptyState, ErrorState, PageHeader, supportPage } from "../components/common";
import { DataTable } from "../components/data-table";
import { Badge } from "../components/ui/badge";
import { Input } from "../components/ui/input";

const columns: ColumnDef<AdminUsageSummaryRow, unknown>[] = [
  {
    accessorKey: "day",
    header: "Day (UTC)",
    cell: ({ getValue }) => <span className="font-mono text-xs">{getValue() as string}</span>,
  },
  {
    accessorKey: "tenant_id",
    header: "Tenant",
    cell: ({ getValue }) => <Badge variant="outline">{getValue() as string}</Badge>,
  },
  {
    accessorKey: "agent_id",
    header: "Agent",
    cell: ({ getValue }) => {
      const v = (getValue() as string) || "";
      return v ? <span className="font-mono text-xs">{v}</span> : <span className="text-muted-foreground">—</span>;
    },
  },
  {
    accessorKey: "usd_estimate",
    header: "USD",
    cell: ({ getValue }) => {
      const n = getValue() as number;
      return <span className="font-mono text-sm font-medium tabular-nums">${Number(n || 0).toFixed(4)}</span>;
    },
  },
  {
    accessorKey: "tokens_in",
    header: "Tokens in",
    cell: ({ getValue }) => <span className="font-mono text-xs tabular-nums">{Number(getValue() || 0).toLocaleString()}</span>,
  },
  {
    accessorKey: "tokens_out",
    header: "Tokens out",
    cell: ({ getValue }) => <span className="font-mono text-xs tabular-nums">{Number(getValue() || 0).toLocaleString()}</span>,
  },
  {
    accessorKey: "run_count",
    header: "Runs",
    cell: ({ getValue }) => <span className="font-mono text-xs tabular-nums">{Number(getValue() || 0)}</span>,
  },
];

function buildUsagePath(tenantId: string, agentId: string): string {
  const q = new URLSearchParams();
  if (tenantId.trim()) q.set("tenant_id", tenantId.trim());
  if (agentId.trim()) q.set("agent_id", agentId.trim());
  const qs = q.toString();
  return qs ? `/usage/summary?${qs}` : "/usage/summary";
}

export function Spend() {
  const [tenantId, setTenantId] = useState("");
  const [agentId, setAgentId] = useState("");
  const path = buildUsagePath(tenantId, agentId);
  const { data, error, loading } = useApi<AdminUsageSummaryRow[]>(path, [tenantId, agentId]);

  const totals = useMemo(() => {
    const rows = data ?? [];
    let usd = 0;
    let tin = 0;
    let tout = 0;
    let runs = 0;
    for (const r of rows) {
      usd += r.usd_estimate || 0;
      tin += r.tokens_in || 0;
      tout += r.tokens_out || 0;
      runs += r.run_count || 0;
    }
    return { usd, tin, tout, runs };
  }, [data]);

  const hasFilter = tenantId.trim() !== "" || agentId.trim() !== "";
  const clearFilters = () => {
    setTenantId("");
    setAgentId("");
  };

  return (
    <div>
      <PageHeader
        title="Spend"
        subtitle="Token and USD estimates by tenant / agent / UTC day (from usage_events). Caps are configured in langgraph.json finops.budgets — this page is read-only."
      />

      <div className="mb-4 grid gap-3 sm:grid-cols-4">
        <div className="rounded-sm border border-border bg-card px-3 py-2">
          <p className="text-[11px] uppercase tracking-wide text-muted-foreground">USD (filter)</p>
          <p className="font-mono text-lg font-semibold tabular-nums text-primary">${totals.usd.toFixed(4)}</p>
        </div>
        <div className="rounded-sm border border-border bg-card px-3 py-2">
          <p className="text-[11px] uppercase tracking-wide text-muted-foreground">Tokens in</p>
          <p className="font-mono text-lg font-semibold tabular-nums">{totals.tin.toLocaleString()}</p>
        </div>
        <div className="rounded-sm border border-border bg-card px-3 py-2">
          <p className="text-[11px] uppercase tracking-wide text-muted-foreground">Tokens out</p>
          <p className="font-mono text-lg font-semibold tabular-nums">{totals.tout.toLocaleString()}</p>
        </div>
        <div className="rounded-sm border border-border bg-card px-3 py-2">
          <p className="text-[11px] uppercase tracking-wide text-muted-foreground">Metered runs</p>
          <p className="font-mono text-lg font-semibold tabular-nums">{totals.runs.toLocaleString()}</p>
        </div>
      </div>

      <div className="mb-4 flex flex-wrap items-center gap-3">
        <Input
          className="w-44"
          placeholder="Tenant ID"
          value={tenantId}
          onChange={(e) => setTenantId(e.target.value)}
        />
        <Input
          className="w-44"
          placeholder="Agent ID"
          value={agentId}
          onChange={(e) => setAgentId(e.target.value)}
        />
        {hasFilter && (
          <button type="button" className="text-sm font-medium text-primary hover:underline" onClick={clearFilters}>
            Clear filters
          </button>
        )}
      </div>

      {error && !data && <ErrorState message={error} />}
      {data && data.length === 0 && (
        <EmptyState
          icon={DollarSign}
          title={hasFilter ? "No matching spend" : "No usage events yet"}
          message={
            hasFilter
              ? "No usage_events match these filters. Clear them, or confirm runners emit Output.usage on terminal runs."
              : "Spend appears after a terminal run with Output.usage (Python LangGraph sums AIMessage.usage_metadata). Configure finops.pricebook for USD estimates. SQL backends only."
          }
          action={
            hasFilter ? (
              <button type="button" className="text-sm font-medium text-primary hover:underline" onClick={clearFilters}>
                Clear filters
              </button>
            ) : undefined
          }
          learnMore={{ href: supportPage("finops.html"), label: "Docs: Spend (FinOps) →" }}
        />
      )}
      {data && data.length > 0 && (
        <DataTable
          columns={columns}
          data={data}
          getRowId={(r) => `${r.day}|${r.tenant_id}|${r.agent_id}`}
          loading={loading}
          initialSorting={[{ id: "day", desc: true }]}
        />
      )}
    </div>
  );
}
