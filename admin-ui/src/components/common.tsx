import type { ReactNode } from "react";
import { AlertTriangle, Inbox } from "lucide-react";
import { cn } from "../lib/utils";
import { statusMeta } from "../lib/status";
import { Badge } from "./ui/badge";
import { Skeleton } from "./ui/skeleton";
import { TableCell, TableRow } from "./ui/table";

export function PageHeader({
  title,
  subtitle,
  actions,
}: {
  title: string;
  subtitle?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <div className="mb-6 flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
        {subtitle && <p className="mt-1.5 max-w-3xl text-sm text-muted-foreground">{subtitle}</p>}
      </div>
      {actions && <div className="flex shrink-0 items-center gap-2">{actions}</div>}
    </div>
  );
}

export function StatusBadge({ status }: { status: string }) {
  const { tone, icon: Icon, spin } = statusMeta(status);
  return (
    <Badge variant={tone}>
      <Icon className={cn("size-3", spin && "animate-spin")} />
      {status}
    </Badge>
  );
}

export function EmptyState({
  title = "Nothing here yet",
  message,
  icon: Icon = Inbox,
  action,
  learnMore,
}: {
  title?: string;
  message: string;
  icon?: typeof Inbox;
  action?: ReactNode;
  /** Optional deep link into the public support map (GitHub Pages). */
  learnMore?: { href: string; label: string };
}) {
  return (
    <div className="flex flex-col items-center justify-center gap-3 rounded-sm border border-dashed border-border py-16 text-center">
      <div className="flex size-12 items-center justify-center rounded-sm bg-muted">
        <Icon className="size-6 text-muted-foreground" />
      </div>
      <div>
        <p className="text-sm font-medium">{title}</p>
        <p className="mt-1 max-w-sm text-sm text-muted-foreground">{message}</p>
      </div>
      {(action || learnMore) && (
        <div className="mt-1 flex flex-wrap items-center justify-center gap-3">
          {action}
          {learnMore && (
            <a
              href={learnMore.href}
              target="_blank"
              rel="noreferrer"
              className="text-sm font-medium text-primary hover:underline"
            >
              {learnMore.label}
            </a>
          )}
        </div>
      )}
    </div>
  );
}

/** Public support-map base (GitHub Pages). Used by Admin empty states. */
export const SUPPORT_BASE = "https://getrunkite.github.io/runkite/support";

export function supportPage(path: string): string {
  const cleaned = path.replace(/^\//, "");
  return `${SUPPORT_BASE}/${cleaned}`;
}

export function ErrorState({ message }: { message: string }) {
  return (
    <div className="flex items-start gap-3 rounded-sm border border-destructive/30 bg-destructive/10 px-4 py-3 text-sm text-destructive">
      <AlertTriangle className="mt-0.5 size-4 shrink-0" />
      <span>{message}</span>
    </div>
  );
}

/** Skeleton rows matching the real table's column count, shown while a
 * list page's first fetch is in flight -- a layout-stable loading state
 * (the table shell renders immediately) reads as far more polished than
 * a plain "Loading..." string that pops the page height around once
 * data arrives. */
export function TableSkeleton({ columns, rows = 6 }: { columns: number; rows?: number }) {
  return (
    <>
      {Array.from({ length: rows }).map((_, r) => (
        <TableRow key={r} className="hover:bg-transparent">
          {Array.from({ length: columns }).map((_, c) => (
            <TableCell key={c}>
              <Skeleton className="h-4 w-full max-w-40" />
            </TableCell>
          ))}
        </TableRow>
      ))}
    </>
  );
}

export function formatTimestamp(iso: string): string {
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

/** Short, human "3m ago"-style relative time -- used alongside (not
 * instead of) the full timestamp via a tooltip, matching how Linear/
 * Vercel/GitHub surface recency at a glance without losing precision. */
export function formatRelativeTime(iso: string): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return iso;
  const diffSec = Math.round((Date.now() - then) / 1000);
  if (diffSec < 5) return "just now";
  if (diffSec < 60) return `${diffSec}s ago`;
  const diffMin = Math.round(diffSec / 60);
  if (diffMin < 60) return `${diffMin}m ago`;
  const diffHour = Math.round(diffMin / 60);
  if (diffHour < 24) return `${diffHour}h ago`;
  const diffDay = Math.round(diffHour / 24);
  return `${diffDay}d ago`;
}
