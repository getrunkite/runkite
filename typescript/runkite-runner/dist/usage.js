/**
 * Accumulate LLM token usage from LangGraph.js stream chunks into Output.usage.
 *
 * Mirrors python/runkite_runner/usage.py so FinOps metering is language-agnostic:
 * the control plane reads a top-level `usage` object on the final `values` blob.
 */
function asInt(v) {
    const n = Number(v ?? 0);
    return Number.isFinite(n) ? Math.trunc(n) : 0;
}
function usageFromMessage(msg) {
    let prompt = 0;
    let completion = 0;
    let model = "";
    if (msg == null || typeof msg !== "object")
        return { prompt, completion, model };
    const m = msg;
    let um = (m.usage_metadata ?? m.usage);
    const rm = (m.response_metadata ?? {});
    if (rm && typeof rm === "object") {
        model = String(rm.model_name ?? rm.model ?? "") || model;
        const tu = (rm.token_usage ?? rm.usage);
        if (tu && typeof tu === "object" && !um)
            um = tu;
    }
    if (um && typeof um === "object") {
        prompt = asInt(um.input_tokens ?? um.prompt_tokens ?? um.promptTokenCount);
        completion = asInt(um.output_tokens ?? um.completion_tokens ?? um.candidatesTokenCount);
        if (!prompt && !completion) {
            const total = asInt(um.total_tokens ?? um.totalTokenCount);
            if (total)
                prompt = total;
        }
        if (!model)
            model = String(um.model ?? "");
    }
    return { prompt, completion, model };
}
function walkMessages(obj, out = []) {
    if (obj == null)
        return out;
    if (Array.isArray(obj)) {
        for (const item of obj)
            walkMessages(item, out);
        return out;
    }
    if (typeof obj === "object") {
        const o = obj;
        if ("messages" in o)
            walkMessages(o.messages, out);
        if (o.type === "ai" || o.type === "AIMessage" || "usage_metadata" in o)
            out.push(obj);
        return out;
    }
    return out;
}
/** Add any token usage found in data into totals (mutates). Call once on the final values snapshot. */
export function accumulateUsage(totals, data) {
    for (const msg of walkMessages(data)) {
        const { prompt, completion, model } = usageFromMessage(msg);
        if (prompt || completion) {
            totals.prompt_tokens = (totals.prompt_tokens ?? 0) + prompt;
            totals.completion_tokens = (totals.completion_tokens ?? 0) + completion;
        }
        if (model && !totals.model)
            totals.model = model;
    }
}
export function usagePayload(totals) {
    const p = totals.prompt_tokens ?? 0;
    const c = totals.completion_tokens ?? 0;
    if (p === 0 && c === 0)
        return null;
    const out = { prompt_tokens: p, completion_tokens: c, total_tokens: p + c };
    if (totals.model)
        out.model = totals.model;
    return out;
}
