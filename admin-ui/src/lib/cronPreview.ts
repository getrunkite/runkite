import type { AdminCronPreview } from "../api/types.ts";

export const adminCronListPath = "/cron?include_history=true&include_preview=true";

export function cronPreviewUtc(iso: string | undefined): string | undefined {
  if (!iso) return undefined;
  const time = new Date(iso);
  if (!Number.isFinite(time.getTime())) return undefined;
  return time.toISOString().replace("T", " ").replace(/\.000Z$/, " UTC").replace(/Z$/, " UTC");
}

export function cronPreviewDisplay(preview: AdminCronPreview | undefined): {
  label: string;
  tone: "secondary" | "muted" | "warning" | "destructive";
  computedAt?: string;
  timezone?: string;
  fallback: boolean;
} {
  const common = {
    computedAt: cronPreviewUtc(preview?.computed_at),
    timezone: preview?.effective_timezone === "Local" ? "Local (server)" : preview?.effective_timezone || undefined,
    fallback: preview?.timezone_fallback === true,
  };
  switch (preview?.status) {
    case "scheduled": {
      const next = cronPreviewUtc(preview.next_fire_at);
      return { ...common, label: next ?? "Preview unavailable", tone: next ? "secondary" : "warning" };
    }
    case "disabled":
      return { ...common, label: "Disabled", tone: "muted" };
    case "invalid_expression":
      return { ...common, label: "Invalid expression", tone: "destructive" };
    case "no_future_fire":
      return { ...common, label: "No next boundary found", tone: "warning" };
    case "relative_interval_unavailable":
      return { ...common, label: "Relative interval preview unavailable", tone: "muted" };
    default:
      return { ...common, label: "Preview unavailable", tone: "warning" };
  }
}
