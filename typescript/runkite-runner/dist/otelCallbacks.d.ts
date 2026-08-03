/**
 * LangChain.js callback handlers that open OTel child spans for LLM and
 * tool calls under the active runkite.run span.
 *
 * Metadata only (model/tool name, status) -- prompt/completion bodies and
 * tool I/O are intentionally not recorded. Mirrors
 * python/runkite_runner/otel_callbacks.py.
 */
import { Tracer, Context } from "@opentelemetry/api";
import { BaseCallbackHandler } from "@langchain/core/callbacks/base";
import type { Serialized } from "@langchain/core/load/serializable";
import type { LLMResult } from "@langchain/core/outputs";
/** Opens runkite.llm / runkite.tool spans nested under runkite.run. */
export declare class OTelCallbackHandler extends BaseCallbackHandler {
    name: string;
    private readonly tracer;
    private readonly parentContext;
    private readonly assignmentRunId;
    private readonly spans;
    constructor(tracer: Tracer, parentContext: Context, runId?: string);
    /** Spans in this.spans are not clone-safe; LC sometimes copies handlers. */
    copy(): BaseCallbackHandler;
    private startChild;
    private end;
    handleChatModelStart(llm: Serialized, _messages: unknown[][], runId: string, parentRunId?: string, _extraParams?: Record<string, unknown>, _tags?: string[], metadata?: Record<string, unknown>): void;
    handleLLMStart(llm: Serialized, _prompts: string[], runId: string, parentRunId?: string, _extraParams?: Record<string, unknown>, _tags?: string[], metadata?: Record<string, unknown>): void;
    handleLLMEnd(_output: LLMResult, runId: string): void;
    handleLLMError(err: Error, runId: string): void;
    handleToolStart(tool: Serialized, _input: string, runId: string, parentRunId?: string, _tags?: string[], metadata?: Record<string, unknown>): void;
    handleToolEnd(_output: unknown, runId: string): void;
    handleToolError(err: Error, runId: string): void;
}
/** Build a handler bound to the current OTel context (call inside withRunSpan). */
export declare function newOTelCallbackHandler(tracer: Tracer, runId: string): OTelCallbackHandler;
