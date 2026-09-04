/**
 * Accumulate LLM token usage from LangGraph.js stream chunks into Output.usage.
 *
 * Mirrors python/runkite_runner/usage.py so FinOps metering is language-agnostic:
 * the control plane reads a top-level `usage` object on the final `values` blob.
 *
 * cost_usd is normally left unset here — the control plane pricebook fills
 * USD from tokens. The one exception: an LLM gateway sitting in front of
 * the provider (OpenRouter, and OpenAI-compatible gateways that follow the
 * same convention) can return an authoritative per-call cost inline in the
 * same usage object as the token counts (OpenRouter: `usage.cost`, in
 * USD). LangChain.js's OpenAI-compatible client generally passes an
 * unrecognized key like this straight through, so we opportunistically
 * look for it and — when present and non-zero — sum it into `cost_usd`,
 * which the control plane's `Pricebook.EstimateUSD` already prefers over
 * any tokens x pricebook estimate. Gateways that report cost out-of-band
 * instead of inline (Portkey headers, Helicone's async dashboard) are not
 * covered — there is nothing in the message object to read.
 */
export type UsageTotals = {
    prompt_tokens?: number;
    completion_tokens?: number;
    model?: string;
    cost_usd?: number;
    _sawUnmeteredAiMessage?: boolean;
};
export type UsagePayload = {
    prompt_tokens: number;
    completion_tokens: number;
    total_tokens: number;
    model?: string;
    cost_usd?: number;
    unmetered?: boolean;
};
/**
 * Add any token usage found in data into totals (mutates). Call once on
 * the final values snapshot.
 *
 * `skipIds` / `skipPrefix` come from executeRun's pre-run getState
 * snapshot. Multi-turn "values" chunks contain the entire message
 * history; without a filter turn N re-bills turns 1..N-1 (and a HITL
 * resume re-bills the pre-interrupt turn). Prefer message ids when
 * present; `skipPrefix` covers graphs that append messages without
 * stable ids (bare concat instead of MessagesAnnotation).
 */
export declare function accumulateUsage(totals: UsageTotals, data: unknown, skipIds?: Set<string>, skipPrefix?: number): void;
export declare function usagePayload(totals: UsageTotals): UsagePayload | null;
/**
 * For adapters that call a framework-specific usage extractor instead of
 * accumulateUsage: if that extraction found nothing but the framework
 * clearly produced real model output, surface the same explicit
 * zero-plus-marker payload instead of silently omitting usage and looking
 * identical to an agent that made no LLM call at all. Mirrors Python's
 * usage_or_unmetered.
 */
export declare function usageOrUnmetered(usage: UsagePayload | null, producedOutput: boolean): UsagePayload | null;
