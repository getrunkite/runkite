/**
 * LangChain.js callback handlers that open OTel child spans for LLM and
 * tool calls under the active runkite.run span.
 *
 * Metadata only (model/tool name, status) -- prompt/completion bodies and
 * tool I/O are intentionally not recorded. Mirrors
 * python/runkite_runner/otel_callbacks.py.
 */
import { context as otelContext, SpanStatusCode, trace } from "@opentelemetry/api";
import { BaseCallbackHandler } from "@langchain/core/callbacks/base";
function modelName(serialized, metadata) {
    if (metadata) {
        for (const key of ["ls_model_name", "model_name", "model"]) {
            const v = metadata[key];
            if (v)
                return String(v);
        }
    }
    if (!serialized)
        return "";
    const anySer = serialized;
    if (anySer.name)
        return String(anySer.name);
    if (Array.isArray(anySer.id) && anySer.id.length > 0)
        return String(anySer.id[anySer.id.length - 1]);
    return "";
}
function toolName(serialized, metadata) {
    if (metadata?.name)
        return String(metadata.name);
    if (!serialized)
        return "";
    const anySer = serialized;
    if (anySer.name)
        return String(anySer.name);
    if (Array.isArray(anySer.id) && anySer.id.length > 0)
        return String(anySer.id[anySer.id.length - 1]);
    return "";
}
/** Opens runkite.llm / runkite.tool spans nested under runkite.run. */
export class OTelCallbackHandler extends BaseCallbackHandler {
    name = "runkite_otel";
    tracer;
    parentContext;
    assignmentRunId;
    spans = new Map();
    constructor(tracer, parentContext, runId = "") {
        super();
        this.tracer = tracer;
        this.parentContext = parentContext;
        this.assignmentRunId = runId;
    }
    /** Spans in this.spans are not clone-safe; LC sometimes copies handlers. */
    copy() {
        return this;
    }
    startChild(name, runId, parentRunId, attributes) {
        // Prefer an open LC-parent span (tool under llm); else the runkite.run
        // context captured at construction (must be built inside withRunSpan).
        let parentCtx = this.parentContext;
        if (parentRunId && this.spans.has(parentRunId)) {
            parentCtx = trace.setSpan(this.parentContext, this.spans.get(parentRunId));
        }
        const span = this.tracer.startSpan(name, {}, parentCtx);
        for (const [k, v] of Object.entries(attributes)) {
            if (v)
                span.setAttribute(k, v);
        }
        if (this.assignmentRunId)
            span.setAttribute("run.id", this.assignmentRunId);
        this.spans.set(runId, span);
    }
    end(runId, error) {
        const span = this.spans.get(runId);
        if (!span)
            return;
        this.spans.delete(runId);
        if (error != null) {
            const err = error instanceof Error ? error : new Error(String(error));
            span.setStatus({ code: SpanStatusCode.ERROR, message: err.message });
            span.recordException(err);
        }
        else {
            span.setStatus({ code: SpanStatusCode.OK });
        }
        span.end();
    }
    handleChatModelStart(llm, _messages, runId, parentRunId, _extraParams, _tags, metadata) {
        this.startChild("runkite.llm", runId, parentRunId, { "llm.name": modelName(llm, metadata) });
    }
    handleLLMStart(llm, _prompts, runId, parentRunId, _extraParams, _tags, metadata) {
        this.startChild("runkite.llm", runId, parentRunId, { "llm.name": modelName(llm, metadata) });
    }
    handleLLMEnd(_output, runId) {
        this.end(runId);
    }
    handleLLMError(err, runId) {
        this.end(runId, err);
    }
    handleToolStart(tool, _input, runId, parentRunId, _tags, metadata) {
        this.startChild("runkite.tool", runId, parentRunId, { "tool.name": toolName(tool, metadata) });
    }
    handleToolEnd(_output, runId) {
        this.end(runId);
    }
    handleToolError(err, runId) {
        this.end(runId, err);
    }
}
/** Build a handler bound to the current OTel context (call inside withRunSpan). */
export function newOTelCallbackHandler(tracer, runId) {
    return new OTelCallbackHandler(tracer, otelContext.active(), runId);
}
