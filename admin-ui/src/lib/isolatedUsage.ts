export interface IsolatedUsageView {
  status: "unknown" | "partial" | "complete";
  tokens: string;
  dollars: string;
  reporter?: string;
  sequence: number;
}

/** Read only the canonical isolated settlement envelope, not arbitrary output. */
export function isolatedUsageView(output: unknown): IsolatedUsageView | undefined {
  if (!output || typeof output !== "object" || Array.isArray(output)) return;
  const usage = (output as Record<string, unknown>).usage;
  if (!usage || typeof usage !== "object" || Array.isArray(usage)) return;
  const value = usage as Record<string, unknown>;
  if (value.reporting_source !== "isolated_workload" || !["unknown", "reported"].includes(String(value.reporting_status)) || typeof value.complete !== "boolean" || !Number.isSafeInteger(value.sequence) || Number(value.sequence) < 1) return;
  const reported = value.reporting_status === "reported";
  const counts = [value.prompt_tokens, value.completion_tokens];
  if (reported && counts.some((n) => typeof n !== "number" || !Number.isSafeInteger(n) || n < 0)) return;
  const status = !reported ? "unknown" : value.complete ? "complete" : "partial";
  const cost = value.cost_usd;
  const knownCost = value.cost_reported === true && typeof cost === "number" && Number.isFinite(cost) && cost >= 0;
  const worker = value.reported_workload;
  const reporter = worker && typeof worker === "object" && !Array.isArray(worker) && typeof (worker as Record<string, unknown>).uri === "string" ? String((worker as Record<string, unknown>).uri) : undefined;
  return { status, tokens: reported ? `${counts[0]} input / ${counts[1]} output` : "Not reported", dollars: knownCost ? `$${cost.toFixed(4)} reported` : "USD not reported", reporter, sequence: Number(value.sequence) };
}
