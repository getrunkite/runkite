/**
 * Accumulate LLM token usage from LangGraph.js stream chunks into Output.usage.
 *
 * Mirrors python/runkite_runner/usage.py so FinOps metering is language-agnostic:
 * the control plane reads a top-level `usage` object on the final `values` blob.
 */
export type UsageTotals = {
    prompt_tokens?: number;
    completion_tokens?: number;
    model?: string;
};
export type UsagePayload = {
    prompt_tokens: number;
    completion_tokens: number;
    total_tokens: number;
    model?: string;
};
/** Add any token usage found in data into totals (mutates). Call once on the final values snapshot. */
export declare function accumulateUsage(totals: UsageTotals, data: unknown): void;
export declare function usagePayload(totals: UsageTotals): UsagePayload | null;
