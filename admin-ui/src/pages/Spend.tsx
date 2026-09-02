import { useMemo, useState } from "react";
import { DollarSign } from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";
import { useApi } from "../api/useApi";
import type { AdminAuditEvent, AdminUsageHoldsSummary, AdminUsageSummaryRow } from "../api/types";
import { EmptyState, ErrorState, PageHeader, formatRelativeTime, supportPage } from "../components/common";
import { DataTable } from "../components/data-table";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";
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

const alertColumns: ColumnDef<AdminAuditEvent, unknown>[] = [
  {
    accessorKey: "ts",
    header: "When",
    cell: ({ getValue }) => <span className="text-muted-foreground">{formatRelativeTime(getValue() as string)}</span>,
  },
  {
    accessorKey: "reason_code",
    header: "Reason",
    cell: ({ getValue }) => <span className="font-mono text-xs">{(getValue() as string) || "—"}</span>,
  },
  {
    accessorKey: "decision",
    header: "Decision",
    cell: ({ getValue }) => <Badge variant="outline">{getValue() as string}</Badge>,
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
      const v = getValue() as string | undefined;
      return v ? <span className="font-mono text-xs">{v}</span> : <span className="text-muted-foreground">—</span>;
    },
  },
];

function buildUsagePath(tenantId: string, agentId: string, from: string, to: string): string {
  const q = new URLSearchParams();
  if (tenantId.trim()) q.set("tenant_id", tenantId.trim());
  if (agentId.trim()) q.set("agent_id", agentId.trim());
  if (from.trim()) q.set("from", new Date(from.trim()).toISOString());
  if (to.trim()) q.set("to", new Date(to.trim()).toISOString());
  const qs = q.toString();
  return qs ? `/usage/summary?${qs}` : "/usage/summary";
}

function buildAlertsPath(tenantId: string, agentId: string): string {
  const q = new URLSearchParams({ limit: "25" });
  if (tenantId.trim()) q.set("tenant_id", tenantId.trim());
  if (agentId.trim()) q.set("agent_id", agentId.trim());
  return `/usage/alerts?${q}`;
}

function buildHoldsPath(tenantId: string, agentId: string): string {
  const q = new URLSearchParams();
  if (tenantId.trim()) q.set("tenant_id", tenantId.trim());
  if (agentId.trim()) q.set("agent_id", agentId.trim());
  const qs = q.toString();
  return qs ? `/usage/holds?${qs}` : "/usage/holds";
}

function buildExportURL(tenantId: string, agentId: string, from: string, to: string, format: "csv" | "json"): string {
  const q = new URLSearchParams({ format });
  if (tenantId.trim()) q.set("tenant_id", tenantId.trim());
  if (agentId.trim()) q.set("agent_id", agentId.trim());
  if (from.trim()) q.set("from", new Date(from.trim()).toISOString());
  if (to.trim()) q.set("to", new Date(to.trim()).toISOString());
  return `/admin-api/usage/export?${q}`;
}

export function Spend() {
  const [tenantId, setTenantId] = useState("");
  const [agentId, setAgentId] = useState("");
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const path = buildUsagePath(tenantId, agentId, from, to);
  const alertsPath = buildAlertsPath(tenantId, agentId);
  const holdsPath = buildHoldsPath(tenantId, agentId);
  const { data, error, loading } = useApi<AdminUsageSummaryRow[]>(path, [tenantId, agentId, from, to]);
  const { data: alerts } = useApi<AdminAuditEvent[]>(alertsPath, [tenantId, agentId]);
  const { data: holds } = useApi<AdminUsageHoldsSummary>(holdsPath, [tenantId, agentId]);

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

  const hasFilter = tenantId.trim() !== "" || agentId.trim() !== "" || from.trim() !== "" || to.trim() !== "";
  const clearFilters = () => {
    setTenantId("");
    setAgentId("");
    setFrom("");
    setTo("");
  };

  const download = (format: "csv" | "json") => {
    window.location.href = buildExportURL(tenantId, agentId, from, to, format);
  };

  const routingActive = (alerts ?? []).some((a) => a.reason_code === "budget_route");

  return (
    <div>
      <PageHeader
        title="Spend"
        subtitle="Token and USD estimates by tenant / agent / UTC day (from usage_events). Caps, alerts, reservation, and cheaper-alias routing live in langgraph.json finops — this page is read-only."
      />

      <div className="mb-4 grid gap-3 sm:grid-cols-5">
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
        <div className="rounded-sm border border-border bg-card px-3 py-2">
          <p className="text-[11px] uppercase tracking-wide text-muted-foreground">Open holds (UTC day)</p>
          <p className="font-mono text-lg font-semibold tabular-nums">
            {holds ? `${holds.count} · $${Number(holds.usd_hold || 0).toFixed(4)}` : "—"}
          </p>
        </div>
      </div>

      {routingActive && (
        <div className="mb-4 rounded-sm border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-sm">
          Cheaper-alias routing recently fired for this filter (see Alerts / reason <code>budget_route</code>).
        </div>
      )}

      <div className="mb-4 flex flex-wrap items-center gap-3">
        <Input className="w-40" placeholder="Tenant ID" value={tenantId} onChange={(e) => setTenantId(e.target.value)} />
        <Input className="w-40" placeholder="Agent ID" value={agentId} onChange={(e) => setAgentId(e.target.value)} />
        <Input className="w-40" type="date" value={from} onChange={(e) => setFrom(e.target.value)} title="From (UTC day)" />
        <Input className="w-40" type="date" value={to} onChange={(e) => setTo(e.target.value)} title="To (UTC day)" />
        <Button type="button" variant="outline" size="sm" onClick={() => download("csv")}>
          Export CSV
        </Button>
        <Button type="button" variant="outline" size="sm" onClick={() => download("json")}>
          Export JSON
        </Button>
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
              ? "No usage_events match these filters."
              : "Spend appears after a terminal run with Output.usage (LangGraph, CrewAI/LlamaIndex adapters when metrics exist, and TypeScript runners that emit usage). Configure finops.pricebook for USD. SQL backends only."
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

      <div className="mt-10">
        <h2 className="mb-2 text-lg font-semibold">Alerts</h2>
        <p className="mb-3 text-sm text-muted-foreground">
          Recent budget_soft / budget_exceeded / budget_alert / budget_route audit rows. Subscribe webhooks to{" "}
          <code>budget_alert</code> for outbound delivery. Approach (`budget_alert`) audits are emitted at most once per tenant/agent/scope/kind/UTC day per control-plane process.
        </p>
        {(!alerts || alerts.length === 0) && (
          <p className="text-sm text-muted-foreground">No budget alerts for this filter yet.</p>
        )}
        {alerts && alerts.length > 0 && (
          <DataTable columns={alertColumns} data={alerts} getRowId={(r) => r.id} initialSorting={[{ id: "ts", desc: true }]} />
        )}
      </div>
    </div>
  );
}
