import { useMemo, useState, type ReactNode } from "react";
import { BookOpen, DollarSign, ExternalLink } from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";
import { useApi } from "../api/useApi";
import type { AdminAuditEvent, AdminUsageHoldsSummary, AdminUsageSummaryRow } from "../api/types";
import { EmptyState, ErrorState, PageHeader, formatRelativeTime, supportPage } from "../components/common";
import { DataTable } from "../components/data-table";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";
import { Input } from "../components/ui/input";
import { Tooltip, TooltipContent, TooltipTrigger } from "../components/ui/tooltip";

/** Public docs — GitHub Pages support map + repo configuration reference. */
const DOCS = {
  finops: supportPage("finops.html"),
  production: supportPage("production.html"),
  configuration: "https://github.com/getrunkite/runkite/blob/main/docs/configuration.md",
} as const;

const USD_TIP =
  "Estimate only — not a provider invoice. Dollars = tokens × finops.pricebook, or gateway cost_usd when the runner reports one.";

const OPEN_HOLDS_TIP =
  "A hold is a provisional reservation placed the instant a run is created (finops.reservation), before the real token count is known — it counts toward today's USD/token caps so many runs starting at once can't all sneak in under the limit before any of them reports real usage. Released the moment the run finishes; a nonzero count here means that many runs are still in flight for this filter right now.";

function UsdHeader() {
  return (
    <Tooltip>
      <TooltipTrigger className="cursor-help underline decoration-dotted underline-offset-4">Est. USD</TooltipTrigger>
      <TooltipContent className="max-w-xs">{USD_TIP}</TooltipContent>
    </Tooltip>
  );
}

function DocsLink({ href, children }: { href: string; children: ReactNode }) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer"
      className="inline-flex items-center gap-1 font-medium text-primary hover:underline"
    >
      {children}
      <ExternalLink className="size-3 opacity-70" />
    </a>
  );
}

function reasonLabel(code: string | undefined): string {
  switch (code) {
    case "usage_unpriced":
      return "Unpriced model";
    case "usage_unmetered":
      return "Unmetered reply";
    case "budget_soft":
      return "Soft budget";
    case "budget_exceeded":
      return "Hard budget";
    case "budget_alert":
      return "Approaching cap";
    case "budget_kill":
      return "Kill on breach";
    case "budget_route":
      return "Cheaper alias";
    default:
      return code || "—";
  }
}

function alertDetail(e: AdminAuditEvent): string {
  const attrs = (e.attrs ?? {}) as Record<string, unknown>;
  if (e.reason_code === "usage_unpriced" && attrs.model) {
    return `Add pricebook row for ${String(attrs.model)}`;
  }
  if (e.reason_code === "usage_unmetered") {
    return attrs.model
      ? `${String(attrs.model)} replied but reported no usage — check that framework/provider's usage_metadata shape`
      : "A run replied but reported no usage at all — check that adapter's usage extraction";
  }
  if (attrs.model) return `model ${String(attrs.model)}`;
  if (attrs.cap_kind) return `cap ${String(attrs.cap_kind)}`;
  return "—";
}

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
    header: () => <UsdHeader />,
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
    header: "What happened",
    cell: ({ row }) => (
      <div className="flex flex-col gap-0.5">
        <span className="text-sm">{reasonLabel(row.original.reason_code)}</span>
        <span className="font-mono text-[10px] text-muted-foreground">{row.original.reason_code || "—"}</span>
      </div>
    ),
  },
  {
    id: "detail",
    header: "What to do",
    cell: ({ row }) => <span className="text-sm text-muted-foreground">{alertDetail(row.original)}</span>,
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

  const unpriced = useMemo(() => (alerts ?? []).filter((a) => a.reason_code === "usage_unpriced"), [alerts]);
  const unpricedModels = useMemo(() => {
    const set = new Set<string>();
    for (const a of unpriced) {
      const m = (a.attrs as Record<string, unknown> | undefined)?.model;
      if (m) set.add(String(m));
    }
    return [...set].sort();
  }, [unpriced]);

  const tokensButNoUsd = totals.tin + totals.tout > 0 && totals.usd === 0;
  const routingActive = (alerts ?? []).some((a) => a.reason_code === "budget_route");

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

  return (
    <div>
      <PageHeader
        title="Spend"
        subtitle="Estimated spend from metered runs. Tokens come from the model; dollars come from your pricebook (or a gateway cost)."
        actions={
          <div className="flex flex-wrap items-center gap-2">
            <Button type="button" variant="outline" size="sm" asChild>
              <a href={DOCS.finops} target="_blank" rel="noreferrer">
                <BookOpen className="size-3.5" />
                FinOps guide
              </a>
            </Button>
            <Button type="button" variant="outline" size="sm" asChild>
              <a href={DOCS.configuration} target="_blank" rel="noreferrer">
                Configure finops
              </a>
            </Button>
          </div>
        }
      />

      <div className="mb-4 rounded-sm border border-border bg-card px-4 py-3 text-sm">
        <p className="font-medium">How USD gets here</p>
        <ol className="mt-2 list-decimal space-y-1.5 pl-5 text-muted-foreground">
          <li>
            Runner finishes a run (or HITL pause) with token usage on the output.
          </li>
          <li>
            Control plane multiplies tokens by <code className="text-foreground">finops.pricebook</code> for that{" "}
            <em>exact model id</em> — or uses gateway <code className="text-foreground">cost_usd</code> when present.
          </li>
          <li>
            Missing pricebook row → tokens still show, Est. USD stays <code className="text-foreground">$0</code>, and an
            Unpriced model alert fires. Add the model under <code className="text-foreground">finops.pricebook</code> in{" "}
            <code className="text-foreground">langgraph.json</code>, then restart the control plane.
          </li>
          <li>
            A reply came back but the runner found <em>no</em> token/cost data in any shape it recognizes (a brand-new
            provider, or a framework integration that has not adopted LangChain's standard{" "}
            <code className="text-foreground">usage_metadata</code>) → an Unmetered reply alert fires instead of quietly
            showing $0, so a genuinely unmetered agent is never mistaken for a free one.
          </li>
        </ol>
        <p className="mt-3 text-muted-foreground">
          Needs a SQL state backend (Postgres / MySQL / SQLite).{" "}
          <DocsLink href={DOCS.production}>Production day-0</DocsLink>
          {" · "}
          <DocsLink href={DOCS.finops}>Spend docs</DocsLink>
          {" · "}
          <DocsLink href={DOCS.configuration}>configuration.md</DocsLink>
        </p>
      </div>

      {(unpriced.length > 0 || tokensButNoUsd) && (
        <div className="mb-4 rounded-sm border border-amber-500/40 bg-amber-500/10 px-4 py-3 text-sm">
          <p className="font-medium text-amber-100">Est. USD is $0 because pricing did not match</p>
          <p className="mt-1 text-muted-foreground">
            Tokens were recorded. This is not a rounding issue — the model id was missing from your pricebook (or no
            gateway cost was reported).
          </p>
          {unpricedModels.length > 0 && (
            <p className="mt-2">
              Models to add:{" "}
              {unpricedModels.map((m) => (
                <code key={m} className="mr-2 rounded bg-background/40 px-1.5 py-0.5 font-mono text-xs">
                  {m}
                </code>
              ))}
            </p>
          )}
          <p className="mt-2 text-muted-foreground">
            Example:{" "}
            <code className="text-xs text-foreground">
              {`"finops": { "pricebook": { "${unpricedModels[0] || "gemini-2.0-flash"}": { "input_per_1k": 0.0001, "output_per_1k": 0.0004 } } }`}
            </code>
          </p>
          <p className="mt-2">
            <DocsLink href={DOCS.configuration}>Open configuration reference</DocsLink>
          </p>
        </div>
      )}

      <div className="mb-4 grid gap-3 sm:grid-cols-5">
        <div className="rounded-sm border border-border bg-card px-3 py-2">
          <Tooltip>
            <TooltipTrigger className="cursor-help text-[11px] uppercase tracking-wide text-muted-foreground underline decoration-dotted underline-offset-4">
              Est. USD
            </TooltipTrigger>
            <TooltipContent className="max-w-xs">{USD_TIP}</TooltipContent>
          </Tooltip>
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
          <Tooltip>
            <TooltipTrigger className="cursor-help text-[11px] uppercase tracking-wide text-muted-foreground underline decoration-dotted underline-offset-4">
              Open holds (UTC day)
            </TooltipTrigger>
            <TooltipContent className="max-w-xs">{OPEN_HOLDS_TIP}</TooltipContent>
          </Tooltip>
          <p className="font-mono text-lg font-semibold tabular-nums">
            {holds ? `${holds.count} · $${Number(holds.usd_hold || 0).toFixed(4)}` : "—"}
          </p>
        </div>
      </div>

      {routingActive && (
        <div className="mb-4 rounded-sm border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-sm text-muted-foreground">
          Cheaper-alias routing fired recently for this filter — see Alerts → Cheaper alias.
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
              ? "Nothing matches these filters."
              : "After a run completes (or pauses for HITL), tokens show here. Add finops.pricebook so Est. USD is non-zero. SQL backends only."
          }
          learnMore={{ href: DOCS.finops, label: "FinOps guide →" }}
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
        <h2 className="mb-1 text-lg font-semibold">Alerts</h2>
        <p className="mb-3 text-sm text-muted-foreground">
          Budget and pricing signals for this filter.{" "}
          <DocsLink href={DOCS.finops}>What each alert means</DocsLink>
        </p>
        {(!alerts || alerts.length === 0) && (
          <p className="text-sm text-muted-foreground">No alerts yet.</p>
        )}
        {alerts && alerts.length > 0 && (
          <DataTable columns={alertColumns} data={alerts} getRowId={(r) => r.id} initialSorting={[{ id: "ts", desc: true }]} />
        )}
      </div>
    </div>
  );
}
