/** Read-only Try-agent hop: agent → plane → connector.tool.
 * Driven by SSE tool_auth plus the existing connectorPending flag.
 * Not a topology editor.
 */

export type HopPhase = "idle" | "running" | "held" | "denied";

export function hopPhase(opts: {
  busy: boolean;
  connectorPending: boolean;
  effect: string | null;
}): HopPhase {
  const effect = (opts.effect || "").toLowerCase();
  if (opts.connectorPending || effect === "pending") return "held";
  if (effect === "deny" || effect === "denied") return "denied";
  if (opts.busy) return "running";
  return "idle";
}

export function hopToolLabel(opts: {
  agentId: string;
  connector: string | null;
  tool: string | null;
}): string {
  const c = (opts.connector || "").trim();
  const t = (opts.tool || "").trim();
  if (c && t) return `${c}.${t}`;
  if (opts.agentId === "pending_agent") return "bank.refund";
  return "tool";
}

export function pendingPageHref(opts: { runId?: string | null; tenantId?: string | null }): string {
  const q = new URLSearchParams();
  const tenant = opts.tenantId?.trim();
  const run = opts.runId?.trim();
  if (tenant) q.set("tenant_id", tenant);
  if (run) q.set("run_id", run);
  const s = q.toString();
  return s ? `/admin/pending?${s}` : "/admin/pending";
}

export function parseToolAuth(data: unknown): {
  effect: string;
  connector: string;
  tool: string;
  actionId: string;
} | null {
  const rec = asRecord(data);
  if (!rec) return null;
  const effect = str(rec.effect);
  const connector = str(rec.connector);
  const tool = str(rec.tool);
  if (!effect && !connector && !tool) return null;
  return {
    effect,
    connector,
    tool,
    actionId: str(rec.action_id),
  };
}

function str(v: unknown): string {
  return typeof v === "string" ? v : v == null ? "" : String(v);
}

function asRecord(data: unknown): Record<string, unknown> | null {
  if (typeof data === "string") {
    try {
      const parsed: unknown = JSON.parse(data);
      return parsed && typeof parsed === "object" ? (parsed as Record<string, unknown>) : null;
    } catch {
      return null;
    }
  }
  if (data && typeof data === "object") return data as Record<string, unknown>;
  return null;
}
