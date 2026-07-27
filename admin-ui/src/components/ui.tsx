import type { ReactNode } from "react";

export function PageHeader({ title, subtitle }: { title: string; subtitle?: string }) {
  return (
    <div className="mb-6">
      <h2 className="text-2xl font-semibold text-slate-100">{title}</h2>
      {subtitle && <p className="mt-1 text-sm text-slate-400">{subtitle}</p>}
    </div>
  );
}

export function Card({ children, className = "" }: { children: ReactNode; className?: string }) {
  return <div className={`rounded-lg border border-slate-800 bg-slate-900 p-5 ${className}`}>{children}</div>;
}

export function StatCard({ label, value }: { label: string; value: number | string }) {
  return (
    <Card>
      <p className="text-sm text-slate-400">{label}</p>
      <p className="mt-1 text-3xl font-semibold text-slate-100">{value}</p>
    </Card>
  );
}

const STATUS_COLORS: Record<string, string> = {
  success: "bg-emerald-500/15 text-emerald-400",
  idle: "bg-emerald-500/15 text-emerald-400",
  running: "bg-blue-500/15 text-blue-400",
  busy: "bg-blue-500/15 text-blue-400",
  pending: "bg-amber-500/15 text-amber-400",
  interrupted: "bg-amber-500/15 text-amber-400",
  error: "bg-red-500/15 text-red-400",
  timeout: "bg-red-500/15 text-red-400",
  closed: "bg-emerald-500/15 text-emerald-400",
  open: "bg-red-500/15 text-red-400",
  "half-open": "bg-amber-500/15 text-amber-400",
};

export function StatusBadge({ status }: { status: string }) {
  const cls = STATUS_COLORS[status] ?? "bg-slate-500/15 text-slate-400";
  return <span className={`inline-block rounded-full px-2 py-0.5 text-xs font-medium ${cls}`}>{status}</span>;
}

export function Loading() {
  return <p className="text-sm text-slate-400">Loading...</p>;
}

export function ErrorMessage({ message }: { message: string }) {
  return <p className="rounded-md border border-red-900 bg-red-950/50 px-3 py-2 text-sm text-red-400">{message}</p>;
}

const BUTTON_VARIANTS: Record<"default" | "danger", string> = {
  default: "border-slate-700 bg-slate-800 text-slate-200 hover:bg-slate-700",
  danger: "border-red-900 bg-red-950/60 text-red-400 hover:bg-red-950",
};

/** Every admin write action (cancel run, delete thread, redeliver webhook)
 * uses this instead of a bare <button> -- consistent disabled/pending
 * styling so a slow request can't be double-submitted by an impatient
 * click. */
export function Button({
  children,
  onClick,
  disabled,
  variant = "default",
}: {
  children: ReactNode;
  onClick: () => void;
  disabled?: boolean;
  variant?: "default" | "danger";
}) {
  return (
    <button
      onClick={onClick}
      disabled={disabled}
      className={`rounded-md border px-3 py-1.5 text-sm font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-50 ${BUTTON_VARIANTS[variant]}`}
    >
      {children}
    </button>
  );
}

export function EmptyState({ message }: { message: string }) {
  return <p className="text-sm text-slate-500">{message}</p>;
}

export function Table({ children }: { children: ReactNode }) {
  return (
    <div className="overflow-hidden rounded-lg border border-slate-800">
      <table className="w-full text-left text-sm">{children}</table>
    </div>
  );
}

export function Th({ children }: { children: ReactNode }) {
  return <th className="border-b border-slate-800 bg-slate-900 px-4 py-2 font-medium text-slate-400">{children}</th>;
}

export function Td({ children, className = "" }: { children: ReactNode; className?: string }) {
  return <td className={`border-b border-slate-800/60 px-4 py-2 text-slate-200 ${className}`}>{children}</td>;
}

export function Tr({ children, onClick }: { children: ReactNode; onClick?: () => void }) {
  return (
    <tr onClick={onClick} className={onClick ? "cursor-pointer hover:bg-slate-800/40" : undefined}>
      {children}
    </tr>
  );
}

/** A generic fetch-and-render helper: loading/error/empty states are
 * identical across every list page, so each page only needs to describe
 * WHAT to fetch and HOW to render one item, not the loading/error
 * boilerplate around it. */
export function formatTimestamp(iso: string): string {
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}
