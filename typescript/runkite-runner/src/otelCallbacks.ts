/**
 * LangChain.js callback handlers that open OTel child spans for LLM and
 * tool calls under the active runkite.run span.
 *
 * Metadata only (model/tool name, status) -- prompt/completion bodies and
 * tool I/O are intentionally not recorded. Mirrors
 * python/runkite_runner/otel_callbacks.py.
 */
import { context as otelContext, Span, SpanStatusCode, Tracer, trace, Context } from "@opentelemetry/api";
import { BaseCallbackHandler } from "@langchain/core/callbacks/base";
import type { Serialized } from "@langchain/core/load/serializable";
import type { LLMResult } from "@langchain/core/outputs";

function modelName(serialized: Serialized | undefined, metadata?: Record<string, unknown>): string {
  if (metadata) {
    for (const key of ["ls_model_name", "model_name", "model"] as const) {
      const v = metadata[key];
      if (v) return String(v);
    }
  }
  if (!serialized) return "";
  const anySer = serialized as { name?: string; id?: string[] };
  if (anySer.name) return String(anySer.name);
  if (Array.isArray(anySer.id) && anySer.id.length > 0) return String(anySer.id[anySer.id.length - 1]);
  return "";
}

function toolName(serialized: Serialized | undefined, metadata?: Record<string, unknown>): string {
  if (metadata?.name) return String(metadata.name);
  if (!serialized) return "";
  const anySer = serialized as { name?: string; id?: string[] };
  if (anySer.name) return String(anySer.name);
  if (Array.isArray(anySer.id) && anySer.id.length > 0) return String(anySer.id[anySer.id.length - 1]);
  return "";
}

/** Opens runkite.llm / runkite.tool spans nested under runkite.run. */
export class OTelCallbackHandler extends BaseCallbackHandler {
  name = "runkite_otel";

  private readonly tracer: Tracer;
  private readonly parentContext: Context;
  private readonly assignmentRunId: string;
  private readonly spans = new Map<string, Span>();

  constructor(tracer: Tracer, parentContext: Context, runId = "") {
    super();
    this.tracer = tracer;
    this.parentContext = parentContext;
    this.assignmentRunId = runId;
  }

  /** Spans in this.spans are not clone-safe; LC sometimes copies handlers. */
  copy(): BaseCallbackHandler {
    return this;
  }

  private startChild(
    name: string,
    runId: string,
    parentRunId: string | undefined,
    attributes: Record<string, string>,
  ): void {
    // Prefer an open LC-parent span (tool under llm); else the runkite.run
    // context captured at construction (must be built inside withRunSpan).
    let parentCtx = this.parentContext;
    if (parentRunId && this.spans.has(parentRunId)) {
      parentCtx = trace.setSpan(this.parentContext, this.spans.get(parentRunId)!);
    }
    const span = this.tracer.startSpan(name, {}, parentCtx);
    for (const [k, v] of Object.entries(attributes)) {
      if (v) span.setAttribute(k, v);
    }
    if (this.assignmentRunId) span.setAttribute("run.id", this.assignmentRunId);
    this.spans.set(runId, span);
  }

  private end(runId: string, error?: unknown): void {
    const span = this.spans.get(runId);
    if (!span) return;
    this.spans.delete(runId);
    if (error != null) {
      const err = error instanceof Error ? error : new Error(String(error));
      span.setStatus({ code: SpanStatusCode.ERROR, message: err.message });
      span.recordException(err);
    } else {
      span.setStatus({ code: SpanStatusCode.OK });
    }
    span.end();
  }

  handleChatModelStart(
    llm: Serialized,
    _messages: unknown[][],
    runId: string,
    parentRunId?: string,
    _extraParams?: Record<string, unknown>,
    _tags?: string[],
    metadata?: Record<string, unknown>,
  ): void {
    this.startChild("runkite.llm", runId, parentRunId, { "llm.name": modelName(llm, metadata) });
  }

  handleLLMStart(
    llm: Serialized,
    _prompts: string[],
    runId: string,
    parentRunId?: string,
    _extraParams?: Record<string, unknown>,
    _tags?: string[],
    metadata?: Record<string, unknown>,
  ): void {
    this.startChild("runkite.llm", runId, parentRunId, { "llm.name": modelName(llm, metadata) });
  }

  handleLLMEnd(_output: LLMResult, runId: string): void {
    this.end(runId);
  }

  handleLLMError(err: Error, runId: string): void {
    this.end(runId, err);
  }

  handleToolStart(
    tool: Serialized,
    _input: string,
    runId: string,
    parentRunId?: string,
    _tags?: string[],
    metadata?: Record<string, unknown>,
  ): void {
    this.startChild("runkite.tool", runId, parentRunId, { "tool.name": toolName(tool, metadata) });
  }

  handleToolEnd(_output: unknown, runId: string): void {
    this.end(runId);
  }

  handleToolError(err: Error, runId: string): void {
    this.end(runId, err);
  }
}

/** Build a handler bound to the current OTel context (call inside withRunSpan). */
export function newOTelCallbackHandler(tracer: Tracer, runId: string): OTelCallbackHandler {
  return new OTelCallbackHandler(tracer, otelContext.active(), runId);
}
