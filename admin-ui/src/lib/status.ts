import type { LucideIcon } from "lucide-react";
import { AlertCircle, CheckCircle2, CircleDashed, CircleSlash, Clock, Loader2, PauseCircle } from "lucide-react";

export type StatusTone = "success" | "warning" | "destructive" | "secondary" | "muted";

interface StatusMeta {
  tone: StatusTone;
  icon: LucideIcon;
  /** Whether the icon should visually spin -- reserved for genuinely
   * in-progress states (running/busy), never for a terminal one. */
  spin?: boolean;
}

// One central status→visual mapping for every status string this
// project's models emit across runs/threads/connectors (success, idle,
// running, busy, pending, interrupted, error, timeout, closed, open,
// half-open) -- replaces the old ui.tsx's STATUS_COLORS map with the
// same tone but also a matching icon, since a color alone is a weaker
// signal than color+shape together (colorblind-friendlier too).
const STATUS_META: Record<string, StatusMeta> = {
  success: { tone: "success", icon: CheckCircle2 },
  idle: { tone: "success", icon: CheckCircle2 },
  closed: { tone: "success", icon: CheckCircle2 },
  allow: { tone: "success", icon: CheckCircle2 },
  approved: { tone: "success", icon: CheckCircle2 },
  consumed: { tone: "success", icon: CheckCircle2 },
  running: { tone: "secondary", icon: Loader2, spin: true },
  busy: { tone: "secondary", icon: Loader2, spin: true },
  pending: { tone: "warning", icon: Clock },
  interrupted: { tone: "warning", icon: PauseCircle },
  "half-open": { tone: "warning", icon: AlertCircle },
  error: { tone: "destructive", icon: CircleSlash },
  timeout: { tone: "destructive", icon: AlertCircle },
  open: { tone: "destructive", icon: AlertCircle },
  deny: { tone: "destructive", icon: CircleSlash },
  denied: { tone: "destructive", icon: CircleSlash },
};

export function statusMeta(status: string): StatusMeta {
  return STATUS_META[status] ?? { tone: "muted", icon: CircleDashed };
}
