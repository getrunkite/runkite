import * as React from "react";
import * as RechartsPrimitive from "recharts";
import { cn } from "../../lib/utils";

// Minimal, focused port of shadcn/ui's chart.tsx: a ChartContainer that
// wires each series' color to a CSS variable (so Recharts' SVG fills
// reference the SAME design tokens as the rest of the app, including
// dark/light theme switching, instead of hard-coded hex colors baked
// into chart config objects) plus a themed tooltip/legend replacing
// Recharts' own unstyled defaults.

export type ChartConfig = Record<
  string,
  {
    label?: React.ReactNode;
    icon?: React.ComponentType;
    color?: string;
  }
>;

type ChartContextProps = { config: ChartConfig };
const ChartContext = React.createContext<ChartContextProps | null>(null);

function useChart() {
  const ctx = React.useContext(ChartContext);
  if (!ctx) throw new Error("useChart must be used within a ChartContainer");
  return ctx;
}

function ChartContainer({
  id,
  className,
  children,
  config,
  ...props
}: React.ComponentProps<"div"> & {
  config: ChartConfig;
  children: React.ComponentProps<typeof RechartsPrimitive.ResponsiveContainer>["children"];
}) {
  const uniqueId = React.useId();
  const chartId = `chart-${id ?? uniqueId.replace(/:/g, "")}`;

  return (
    <ChartContext.Provider value={{ config }}>
      <div
        data-slot="chart"
        data-chart={chartId}
        className={cn(
          "[&_.recharts-cartesian-axis-tick_text]:fill-muted-foreground [&_.recharts-cartesian-grid_line]:stroke-border/50 [&_.recharts-curve.recharts-tooltip-cursor]:stroke-border [&_.recharts-dot[stroke='#fff']]:stroke-transparent [&_.recharts-layer]:outline-hidden [&_.recharts-sector]:outline-hidden [&_.recharts-sector[stroke='#fff']]:stroke-transparent [&_.recharts-surface]:outline-hidden flex aspect-video justify-center text-xs",
          className,
        )}
        {...props}
      >
        <ChartStyle id={chartId} config={config} />
        <RechartsPrimitive.ResponsiveContainer>{children}</RechartsPrimitive.ResponsiveContainer>
      </div>
    </ChartContext.Provider>
  );
}

function ChartStyle({ id, config }: { id: string; config: ChartConfig }) {
  const colored = Object.entries(config).filter(([, cfg]) => cfg.color);
  if (!colored.length) return null;

  return (
    <style
      dangerouslySetInnerHTML={{
        __html: `[data-chart=${id}] {\n${colored.map(([key, cfg]) => `  --color-${key}: ${cfg.color};`).join("\n")}\n}`,
      }}
    />
  );
}

const ChartTooltip = RechartsPrimitive.Tooltip;

interface ChartTooltipPayloadItem {
  dataKey?: string | number;
  name?: string | number;
  value?: number | string;
  color?: string;
}

function ChartTooltipContent({
  active,
  payload,
  label,
  className,
  indicator = "dot",
  hideLabel = false,
  labelFormatter,
  formatter,
}: {
  active?: boolean;
  payload?: readonly ChartTooltipPayloadItem[];
  label?: unknown;
  className?: string;
  indicator?: "dot" | "line";
  hideLabel?: boolean;
  labelFormatter?: (value: unknown) => React.ReactNode;
  formatter?: (value: number, name: string) => React.ReactNode;
}) {
  const { config } = useChart();

  if (!active || !payload?.length) return null;

  return (
    <div
      className={cn(
        "border-border/60 bg-popover text-popover-foreground grid min-w-[9rem] gap-1.5 rounded-lg border px-2.5 py-1.5 text-xs shadow-lg",
        className,
      )}
    >
      {!hideLabel && label != null && (
        <div className="font-medium">{labelFormatter ? labelFormatter(label) : String(label)}</div>
      )}
      <div className="grid gap-1">
        {payload.map((item: ChartTooltipPayloadItem, i: number) => {
          const key = String(item.dataKey ?? item.name ?? i);
          const itemConfig = config[key];
          const color = item.color ?? itemConfig?.color;
          return (
            <div key={key} className="flex w-full items-center gap-2">
              <span
                className={cn(
                  "shrink-0 rounded-[2px]",
                  indicator === "dot" ? "size-2" : "h-full w-1",
                )}
                style={{ backgroundColor: color }}
              />
              <div className="flex flex-1 justify-between gap-2 leading-none">
                <span className="text-muted-foreground">{itemConfig?.label ?? item.name}</span>
                <span className="text-foreground font-mono font-medium tabular-nums">
                  {formatter && typeof item.value === "number" ? formatter(item.value, key) : item.value}
                </span>
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

function ChartLegendContent({ payload }: { payload?: readonly { value?: string; color?: string }[] }) {
  const { config } = useChart();
  if (!payload?.length) return null;

  return (
    <div className="flex flex-wrap items-center justify-center gap-4 pt-2">
      {payload.map((item, i) => {
        const key = item.value ?? String(i);
        const itemConfig = config[key];
        return (
          <div key={key} className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <span className="size-2 shrink-0 rounded-[2px]" style={{ backgroundColor: item.color }} />
            {itemConfig?.label ?? key}
          </div>
        );
      })}
    </div>
  );
}

export { ChartContainer, ChartTooltip, ChartTooltipContent, ChartLegendContent };
