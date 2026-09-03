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
};

export type UsagePayload = {
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  model?: string;
  cost_usd?: number;
};

function asInt(v: unknown): number {
  const n = Number(v ?? 0);
  return Number.isFinite(n) ? Math.trunc(n) : 0;
}

function asFloat(v: unknown): number {
  const n = Number(v ?? 0);
  return Number.isFinite(n) ? n : 0;
}

function usageFromMessage(msg: unknown): { prompt: number; completion: number; model: string; cost: number } {
  let prompt = 0;
  let completion = 0;
  let model = "";
  let cost = 0;
  if (msg == null || typeof msg !== "object") return { prompt, completion, model, cost };
  const m = msg as Record<string, unknown>;
  let um = (m.usage_metadata ?? m.usage) as Record<string, unknown> | undefined;
  const rm = (m.response_metadata ?? {}) as Record<string, unknown>;
  if (rm && typeof rm === "object") {
    model = String(rm.model_name ?? rm.model ?? "") || model;
    const tu = (rm.token_usage ?? rm.usage) as Record<string, unknown> | undefined;
    if (tu && typeof tu === "object" && !um) um = tu;
  }
  if (um && typeof um === "object") {
    prompt = asInt(um.input_tokens ?? um.prompt_tokens ?? um.promptTokenCount);
    completion = asInt(um.output_tokens ?? um.completion_tokens ?? um.candidatesTokenCount);
    if (!prompt && !completion) {
      const total = asInt(um.total_tokens ?? um.totalTokenCount);
      if (total) prompt = total;
    }
    if (!model) model = String(um.model ?? "");
    cost = asFloat(um.cost ?? um.total_cost);
  }
  return { prompt, completion, model, cost };
}

function walkMessages(obj: unknown, out: unknown[] = []): unknown[] {
  if (obj == null) return out;
  if (Array.isArray(obj)) {
    for (const item of obj) walkMessages(item, out);
    return out;
  }
  if (typeof obj === "object") {
    const o = obj as Record<string, unknown>;
    // Serialized AIMessage-shaped object — check first, same rationale as
    // the Python runner: do not skip it just because it happens to also
    // carry an unrelated "messages" field.
    if (o.type === "ai" || o.type === "AIMessage" || "usage_metadata" in o) {
      out.push(obj);
      return out;
    }
    // "values" mode: a state object with a top-level "messages" key.
    if ("messages" in o) {
      walkMessages(o.messages, out);
      return out;
    }
    // "updates" mode: LangGraph.js wraps a superstep's changes as
    // {nodeName: {...state changes...}}, one key per node that ran. There
    // is no "messages" key at this level -- it is one level down, inside
    // each node's own update -- so without this fallback, every run using
    // stream_modes=["updates"] alone reported zero usage regardless of
    // outcome: the messages were real, accumulateUsage just never saw
    // them under the wrong key.
    for (const v of Object.values(o)) walkMessages(v, out);
    return out;
  }
  return out;
}

/** Add any token usage found in data into totals (mutates). Call once on the final values snapshot. */
export function accumulateUsage(totals: UsageTotals, data: unknown): void {
  for (const msg of walkMessages(data)) {
    const { prompt, completion, model, cost } = usageFromMessage(msg);
    if (prompt || completion) {
      totals.prompt_tokens = (totals.prompt_tokens ?? 0) + prompt;
      totals.completion_tokens = (totals.completion_tokens ?? 0) + completion;
    }
    if (cost) {
      totals.cost_usd = (totals.cost_usd ?? 0) + cost;
    }
    if (model && !totals.model) totals.model = model;
  }
}

export function usagePayload(totals: UsageTotals): UsagePayload | null {
  const p = totals.prompt_tokens ?? 0;
  const c = totals.completion_tokens ?? 0;
  if (p === 0 && c === 0) return null;
  const out: UsagePayload = { prompt_tokens: p, completion_tokens: c, total_tokens: p + c };
  if (totals.model) out.model = totals.model;
  if (totals.cost_usd) out.cost_usd = totals.cost_usd;
  return out;
}
