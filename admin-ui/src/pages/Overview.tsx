import { useEffect, useRef, useState } from "react";
import { Bot, Clock, MessagesSquare, Plug, Workflow } from "lucide-react";
import { Area, AreaChart, CartesianGrid, Cell, Pie, PieChart, XAxis, YAxis } from "recharts";
import { useApi } from "../api/useApi";
import type { AdminOverview } from "../api/types";
import { ErrorState, PageHeader, StatusBadge } from "../components/common";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "../components/ui/card";
import { Skeleton } from "../components/ui/skeleton";
import { ChartContainer, ChartTooltip, ChartTooltipContent, type ChartConfig } from "../components/ui/chart";
import { statusMeta } from "../lib/status";

const POLL_MS = 5000;
const HISTORY_LIMIT = 60; // 5 minutes at a 5s poll

interface HistoryPoint {
  t: number;
  total_runs: number;
  total_threads: number;
}

const STAT_CARDS: { key: keyof AdminOverview; label: string; icon: typeof Bot }[] = [
  { key: "total_agents", label: "Agents", icon: Bot },
  { key: "total_threads", label: "Threads", icon: MessagesSquare },
  { key: "total_runs", label: "Runs", icon: Workflow },
  { key: "connector_count", label: "Connectors", icon: Plug },
  { key: "cron_schedule_count", label: "Cron schedules", icon: Clock },
];

// Tailwind's `--color-chart-N` tokens from index.css, wired through the
// chart config so the SVG fills track the active theme automatically.
const runsChartConfig = {
  total_runs: { label: "Total runs", color: "var(--color-chart-1)" },
} satisfies ChartConfig;

export function Overview() {
  const { data, error, loading } = useApi<AdminOverview>("/overview", [], POLL_MS);
  const [history, setHistory] = useState<HistoryPoint[]>([]);
  const [runsDelta, setRunsDelta] = useState<number | null>(null);
  const lastSeenRuns = useRef<number | null>(null);

  // Client-side accumulation, not a backend time-series endpoint: the
  // admin API only ever returns the current snapshot (see
  // AdminOverview), so a genuinely live-updating trend line has to be
  // built from this page's own polling history -- starts empty on load
  // and fills in over the session, same trade-off any "live" dashboard
  // without a metrics backend makes.
  //
  // The delta-vs-last-poll comparison also lives here rather than being
  // computed during render: mutating lastSeenRuns.current inline in the
  // render body would re-run on every render pass (including ones React
  // throws away, e.g. under Strict Mode's double-invoke), desyncing the
  // ref from what actually got committed. An effect only fires once per
  // real data change.
  useEffect(() => {
    if (!data) return;
    setRunsDelta(lastSeenRuns.current !== null ? data.total_runs - lastSeenRuns.current : null);
    lastSeenRuns.current = data.total_runs;
    setHistory((prev) => {
      const next = [...prev, { t: Date.now(), total_runs: data.total_runs, total_threads: data.total_threads }];
      return next.length > HISTORY_LIMIT ? next.slice(next.length - HISTORY_LIMIT) : next;
    });
  }, [data]);

  if (error && !data) return <ErrorState message={error} />;

  return (
    <div>
      <PageHeader
        title="Overview"
        subtitle="Across every tenant in this deployment."
        actions={<LiveIndicator loading={loading} />}
      />

      <div className="mb-6 grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-5">
        {STAT_CARDS.map((stat) => (
          <StatCard
            key={stat.key}
            label={stat.label}
            value={data?.[stat.key] as number | undefined}
            icon={stat.icon}
            delta={stat.key === "total_runs" ? runsDelta : null}
          />
        ))}
      </div>

      <div className="mb-6 grid grid-cols-1 gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader>
            <CardTitle>Runs over this session</CardTitle>
            <CardDescription>
              Live, polled every {POLL_MS / 1000}s -- fills in as you watch, not a historical backfill.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {history.length < 2 ? (
              <div className="flex h-56 items-center justify-center text-sm text-muted-foreground">
                Collecting data points...
              </div>
            ) : (
              <ChartContainer config={runsChartConfig} className="h-56 w-full">
                <AreaChart data={history} margin={{ left: 0, right: 12, top: 8, bottom: 0 }}>
                  <defs>
                    <linearGradient id="fillRuns" x1="0" y1="0" x2="0" y2="1">
                      <stop offset="0%" stopColor="var(--color-total_runs)" stopOpacity={0.55} />
                      <stop offset="100%" stopColor="var(--color-total_runs)" stopOpacity={0} />
                    </linearGradient>
                  </defs>
                  <CartesianGrid vertical={false} strokeDasharray="3 3" />
                  <XAxis
                    dataKey="t"
                    tickFormatter={(t: number) => new Date(t).toLocaleTimeString([], { hour12: false })}
                    tickLine={false}
                    axisLine={false}
                    tickMargin={8}
                    minTickGap={40}
                  />
                  <YAxis domain={["dataMin - 1", "dataMax + 2"]} hide />
                  <ChartTooltip
                    content={
                      <ChartTooltipContent
                        labelFormatter={(t) => new Date(t as number).toLocaleTimeString()}
                      />
                    }
                  />
                  <Area
                    dataKey="total_runs"
                    type="monotone"
                    fill="url(#fillRuns)"
                    stroke="var(--color-total_runs)"
                    strokeWidth={2}
                  />
                </AreaChart>
              </ChartContainer>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Runs by status</CardTitle>
            <CardDescription>Current snapshot across every tenant.</CardDescription>
          </CardHeader>
          <CardContent>
            <StatusDonut breakdown={data?.runs_by_status} loading={loading && !data} />
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Threads by status</CardTitle>
          <CardDescription>How many threads are idle, busy, or interrupted right now.</CardDescription>
        </CardHeader>
        <CardContent>
          <StatusBreakdownBars breakdown={data?.threads_by_status} loading={loading && !data} />
        </CardContent>
      </Card>
    </div>
  );
}

function LiveIndicator({ loading }: { loading: boolean }) {
  return (
    <div className="flex items-center gap-1.5 rounded-full border border-border/60 bg-card px-2.5 py-1 text-xs text-muted-foreground">
      <span className="relative flex size-1.5">
        <span
          className={`absolute inline-flex h-full w-full rounded-full bg-success opacity-75 ${loading ? "animate-ping" : ""}`}
        />
        <span className="relative inline-flex size-1.5 rounded-full bg-success" />
      </span>
      Live -- updates every {POLL_MS / 1000}s
    </div>
  );
}

function StatCard({
  label,
  value,
  icon: Icon,
  delta,
}: {
  label: string;
  value?: number;
  icon: typeof Bot;
  delta?: number | null;
}) {
  return (
    <Card>
      <CardContent className="flex items-start justify-between">
        <div>
          <p className="text-sm text-muted-foreground">{label}</p>
          {value === undefined ? (
            <Skeleton className="mt-2 h-8 w-16" />
          ) : (
            <p className="mt-1 text-3xl font-semibold tabular-nums tracking-tight">{value.toLocaleString()}</p>
          )}
          {delta != null && delta !== 0 && (
            <p className={`mt-1 text-xs font-medium ${delta > 0 ? "text-success" : "text-muted-foreground"}`}>
              {delta > 0 ? "+" : ""}
              {delta} since last check
            </p>
          )}
        </div>
        <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
          <Icon className="size-4" />
        </div>
      </CardContent>
    </Card>
  );
}

function StatusDonut({ breakdown, loading }: { breakdown?: Record<string, number>; loading: boolean }) {
  if (loading) return <Skeleton className="mx-auto h-48 w-48 rounded-full" />;
  const entries = Object.entries(breakdown ?? {});
  if (entries.length === 0) {
    return <p className="flex h-48 items-center justify-center text-sm text-muted-foreground">No runs yet.</p>;
  }
  const chartData = entries.map(([status, count]) => ({ status, count, fill: `var(--color-${status})` }));
  const config: ChartConfig = Object.fromEntries(
    entries.map(([status]) => [status, { label: status, color: statusColorVar(status) }]),
  );

  return (
    <div>
      <ChartContainer config={config} className="mx-auto h-48 w-full">
        <PieChart>
          <ChartTooltip content={<ChartTooltipContent hideLabel />} />
          <Pie data={chartData} dataKey="count" nameKey="status" innerRadius={45} outerRadius={70} strokeWidth={2}>
            {chartData.map((entry) => (
              <Cell key={entry.status} fill={entry.fill} />
            ))}
          </Pie>
        </PieChart>
      </ChartContainer>
      <ul className="mt-2 space-y-1.5">
        {entries.map(([status, count]) => (
          <li key={status} className="flex items-center justify-between text-sm">
            <StatusBadge status={status} />
            <span className="font-medium tabular-nums">{count}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

function StatusBreakdownBars({ breakdown, loading }: { breakdown?: Record<string, number>; loading: boolean }) {
  if (loading) {
    return (
      <div className="space-y-3">
        {[0, 1, 2].map((i) => (
          <Skeleton key={i} className="h-6 w-full" />
        ))}
      </div>
    );
  }
  const entries = Object.entries(breakdown ?? {});
  if (entries.length === 0) {
    return <p className="text-sm text-muted-foreground">No threads yet.</p>;
  }
  const max = Math.max(...entries.map(([, count]) => count), 1);

  return (
    <div className="space-y-3">
      {entries.map(([status, count]) => (
        <div key={status} className="flex items-center gap-3">
          <div className="w-28 shrink-0">
            <StatusBadge status={status} />
          </div>
          <div className="h-2 flex-1 overflow-hidden rounded-full bg-muted">
            <div
              className="h-full rounded-full transition-all"
              style={{ width: `${(count / max) * 100}%`, backgroundColor: statusColorVar(status) }}
            />
          </div>
          <span className="w-10 shrink-0 text-right text-sm font-medium tabular-nums">{count}</span>
        </div>
      ))}
    </div>
  );
}

// Maps each status to one of the theme's 5 chart color tokens
// (index.css's --chart-1..5), grouped by tone so e.g. every
// success-like status (success/idle/closed) reads the same color --
// consistent with StatusBadge's own tone mapping in lib/status.ts.
function statusColorVar(status: string): string {
  const { tone } = statusMeta(status);
  switch (tone) {
    case "success":
      return "var(--color-chart-2)";
    case "warning":
      return "var(--color-chart-3)";
    case "destructive":
      return "var(--color-chart-5)";
    case "secondary":
      return "var(--color-chart-1)";
    default:
      return "var(--color-chart-4)";
  }
}
