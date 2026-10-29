/** Optional AI SDK execution. No model or SDK code runs in the Go plane. */
import { readFileSync } from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { splitGraphPath } from "./adapter.js";
import { AiSdkState, stateJson } from "./aiSdkState.js";
import { CompletionDeliveryError } from "./completion.js";
import { buildRunConfig } from "./executeRun.js";
import { runWithBinding } from "./tenantCtx.js";
const SETTINGS = new Set([
    "model",
    "instructions",
    "tools",
    "stopWhen",
    "maxRetries",
    "maxOutputTokens",
    "temperature",
    "providerOptions",
]);
function incomingMessages(input) {
    const value = input;
    if (!value || !Array.isArray(value.messages) || !value.messages.length)
        throw new Error("input.messages must contain new user text");
    return value.messages.map((raw) => {
        const message = raw;
        if (!message || (message.role !== "user" && message.role !== "human") || typeof message.content !== "string") {
            throw new Error("AI SDK input supports new user/human text only; history belongs to the runner");
        }
        return { role: "user", content: message.content };
    });
}
function storedState(value, graphId) {
    if (value === null)
        return null;
    const state = value;
    if (state?.v !== 1 ||
        state.framework !== "ai-sdk" ||
        state.graph_id !== graphId ||
        !Array.isArray(state.messages) ||
        state.completed?.status !== "success" ||
        typeof state.completed.run_id !== "string" ||
        typeof state.completed.input !== "string" ||
        !Array.isArray(state.completed.chunks) ||
        !state.completed.output ||
        typeof state.completed.output !== "object") {
        throw new CompletionDeliveryError("AI SDK checkpoint is invalid or belongs to another agent");
    }
    return state;
}
function genericMessages(messages) {
    return messages.map((message) => ({ ...message, role: message.role === "assistant" ? "ai" : message.role }));
}
function hasUnresolvedTools(messages) {
    const unresolved = new Set();
    for (const message of messages) {
        if (!Array.isArray(message.content))
            continue;
        for (const part of message.content) {
            if (part.type === "tool-call")
                unresolved.add(part.toolCallId);
            if (part.type === "tool-result")
                unresolved.delete(part.toolCallId);
        }
    }
    return unresolved.size !== 0;
}
/** Loads per-run factories. Full native approval/resume and binary state are outside v1. */
export class AiSdkAdapter {
    configPath;
    options;
    factories = new Map();
    state;
    constructor(configPath, options) {
        this.configPath = configPath;
        this.options = options;
        if (!options.httpAddress.trim())
            throw new Error("AI SDK managed runs require the control-plane HTTP address");
        this.state = new AiSdkState(options.httpAddress, options.runnerKind ?? "typescript-ai-sdk", options.runnerToken, options.fetch);
    }
    async load() {
        const config = JSON.parse(readFileSync(this.configPath, "utf8"));
        const kind = this.options.runnerKind ?? "typescript-ai-sdk";
        if (config.runner_kind !== kind)
            throw new Error(`AI SDK config runner_kind must be ${kind}`);
        if (!config.graphs || !Object.keys(config.graphs).length)
            throw new Error("AI SDK config requires graph factories");
        // Optional peer is loaded only for this backend, keeping the default runner unchanged.
        await import("ai");
        for (const [id, reference] of Object.entries(config.graphs)) {
            if (config.llm_cache?.[id])
                throw new Error("whole-run caching is unsupported by the managed AI SDK UI protocol");
            const [file, symbol] = splitGraphPath(reference);
            const module = await import(pathToFileURL(path.resolve(path.dirname(this.configPath), file)).href);
            if (typeof module[symbol] !== "function")
                throw new Error(`AI SDK export ${id} must be a per-run factory`);
            this.factories.set(id, module[symbol]);
        }
    }
    async close() {
        this.factories.clear();
    }
    execute(assignment, emit, isCancelled) {
        return runWithBinding({
            tenantId: assignment.tenant_id,
            runId: assignment.run_id,
            generation: assignment.generation,
            runnerKind: this.options.runnerKind ?? "typescript-ai-sdk",
            runnerToken: this.options.runnerToken,
        }, () => this.executeBound(assignment, emit, isCancelled));
    }
    async executeBound(assignment, emit, isCancelled) {
        let seq = 0;
        const event = (method, data) => emit({ event_id: `${assignment.run_id}_evt_${++seq}`, seq, method, data, namespace: [], ts: Date.now() });
        const generation = assignment.generation ?? 0;
        const messageId = `${assignment.run_id}:g${generation}:assistant`;
        const controller = new AbortController();
        const cancel = () => {
            if (isCancelled() && !controller.signal.aborted)
                controller.abort(new Error("Runkite run cancelled"));
        };
        cancel();
        const timer = setInterval(cancel, 20);
        timer.unref();
        let policyError;
        const chunks = [];
        let chunkBytes = 0;
        let producedOutput = false;
        let activeModelCalls = 0;
        let usage = { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 };
        try {
            if (assignment.checkpoint_ref || assignment.resume_command)
                throw new Error("AI SDK native resume and checkpoint_ref are unsupported");
            const input = incomingMessages(assignment.input);
            const inputKey = stateJson(input);
            if (controller.signal.aborted) {
                await event("end", { status: "interrupted" });
                return "interrupted";
            }
            const snapshot = await this.state.load(assignment.thread_id, controller.signal);
            const prior = storedState(snapshot.value, assignment.graph_id);
            if (controller.signal.aborted) {
                await event("end", { status: "interrupted" });
                return "interrupted";
            }
            await event("lifecycle", { event: "running" });
            if (prior?.completed.run_id === assignment.run_id) {
                if (prior.completed.input !== inputKey)
                    throw new Error("reclaimed run input differs from its committed turn");
                for (const chunk of prior.completed.chunks) {
                    if (chunk.type === "text-delta") {
                        await event("messages", [{ id: messageId, type: "ai", content: chunk.delta }, {}]);
                    }
                    if (chunk.type === "start")
                        await event("custom:ai_sdk", { ...chunk, messageId });
                    else if (chunk.type !== "finish")
                        await event("custom:ai_sdk", chunk);
                }
                const output = {
                    ...prior.completed.output,
                    messages: genericMessages(prior.messages),
                    ai_sdk: { framework: "ai-sdk", generation, message_id: messageId },
                };
                await event("values", output);
                for (const chunk of prior.completed.chunks)
                    if (chunk.type === "finish")
                        await event("custom:ai_sdk", chunk);
                await event("end", { status: "success" });
                return "success";
            }
            const factory = this.factories.get(assignment.graph_id);
            if (!factory)
                throw new Error(`AI SDK factory not found: ${assignment.graph_id}`);
            const definition = await factory(Object.freeze({
                runId: assignment.run_id,
                threadId: assignment.thread_id,
                tenantId: assignment.tenant_id ?? "default",
                generation,
                signal: controller.signal,
                runkiteConfig: buildRunConfig(assignment),
            }));
            if (controller.signal.aborted)
                throw controller.signal.reason;
            if (!definition ||
                typeof definition !== "object" ||
                !definition.model ||
                Object.keys(definition).some((key) => !SETTINGS.has(key))) {
                throw new Error("AI SDK factory must return supported ToolLoopAgent settings");
            }
            const tools = {};
            for (const [name, tool] of Object.entries(definition.tools ?? {})) {
                if (tool.type === "provider" ||
                    !tool.execute ||
                    tool.needsApproval ||
                    /GeneratorFunction/.test(tool.execute.constructor.name))
                    throw new Error(`tool ${name} requires an unsupported execution mode`);
                const execute = tool.execute;
                tools[name] = {
                    ...tool,
                    execute: async (args, options) => {
                        if (controller.signal.aborted)
                            throw controller.signal.reason;
                        if (assignment.allowed_tools && !assignment.allowed_tools.includes(name)) {
                            policyError = new Error(`tool ${name} is outside allowed_tools`);
                            controller.abort(policyError);
                            throw policyError;
                        }
                        const result = await execute(args, options);
                        if (result && typeof result === "object" && Symbol.asyncIterator in result) {
                            policyError = new Error("streaming tool outputs are unsupported by this adapter");
                            controller.abort(policyError);
                            throw policyError;
                        }
                        return result;
                    },
                };
            }
            const { ToolLoopAgent, toUIMessageStream } = await import("ai");
            const agent = new ToolLoopAgent({
                ...definition,
                tools,
                onLanguageModelCallStart: () => {
                    activeModelCalls++;
                },
                onLanguageModelCallEnd: (call) => {
                    activeModelCalls = Math.max(0, activeModelCalls - 1);
                    usage.prompt_tokens += call.usage.inputTokens ?? 0;
                    usage.completion_tokens += call.usage.outputTokens ?? 0;
                    usage.total_tokens = usage.prompt_tokens + usage.completion_tokens;
                    if (call.modelId)
                        usage.model = call.modelId;
                },
            });
            const messages = [...(prior?.messages ?? []), ...input];
            const result = await agent.stream({ messages, abortSignal: controller.signal });
            const stream = toUIMessageStream({
                stream: result.stream,
                tools,
                generateMessageId: () => messageId,
                sendReasoning: false,
                sendSources: false,
                onError: () => "Agent execution failed",
            });
            const reader = stream.getReader();
            const stopReading = () => {
                void reader.cancel(controller.signal.reason).catch(() => { });
            };
            controller.signal.addEventListener("abort", stopReading, { once: true });
            if (controller.signal.aborted)
                stopReading();
            try {
                while (true) {
                    const { done, value: chunk } = await reader.read();
                    if (done)
                        break;
                    if (chunk.type === "error")
                        throw new Error("AI SDK stream failed");
                    chunkBytes += Buffer.byteLength(stateJson(chunk));
                    if (chunkBytes > 8 << 20) {
                        policyError = new Error("AI SDK UI receipt exceeds the 8 MiB limit");
                        controller.abort(policyError);
                        throw policyError;
                    }
                    chunks.push(chunk);
                    if (chunk.type === "text-delta") {
                        producedOutput = true;
                        await event("messages", [{ id: messageId, type: "ai", content: chunk.delta }, {}]);
                    }
                    if (chunk.type !== "finish")
                        await event("custom:ai_sdk", chunk);
                }
            }
            finally {
                controller.signal.removeEventListener("abort", stopReading);
                void reader.cancel().catch(() => { });
            }
            if (policyError)
                throw policyError;
            if (controller.signal.aborted) {
                await event("values", {
                    usage: {
                        ...usage,
                        ...(activeModelCalls || (producedOutput && !usage.total_tokens) ? { unmetered: true } : {}),
                    },
                    ai_sdk: { framework: "ai-sdk", generation, message_id: messageId },
                });
                await event("end", { status: "interrupted" });
                return "interrupted";
            }
            const [responseMessages, nativeUsage, steps, reason] = await Promise.all([
                result.responseMessages,
                result.usage,
                result.steps,
                result.finishReason,
            ]);
            if (reason === "error" || hasUnresolvedTools(responseMessages))
                throw new Error("AI SDK turn did not reach a completed native result");
            const prompt = nativeUsage.inputTokens ?? 0;
            const completion = nativeUsage.outputTokens ?? 0;
            const model = steps.at(-1)?.response.modelId ?? usage.model;
            usage = {
                prompt_tokens: prompt,
                completion_tokens: completion,
                total_tokens: prompt + completion,
                ...(model ? { model } : {}),
                ...(!prompt && !completion ? { unmetered: true } : {}),
            };
            const history = [...messages, ...responseMessages];
            const output = {
                messages: genericMessages(history),
                usage,
                ai_sdk: { framework: "ai-sdk", generation, message_id: messageId },
            };
            if (Buffer.byteLength(stateJson(output)) > 3 << 20)
                throw new Error("AI SDK output exceeds the 3 MiB event limit");
            await this.state.save(assignment.thread_id, {
                v: 1,
                framework: "ai-sdk",
                graph_id: assignment.graph_id,
                messages: history,
                completed: { run_id: assignment.run_id, input: inputKey, status: "success", output: { usage }, chunks },
            }, snapshot.etag);
            await event("values", output);
            for (const chunk of chunks)
                if (chunk.type === "finish")
                    await event("custom:ai_sdk", chunk);
            await event("end", { status: "success" });
            return "success";
        }
        catch (error) {
            if (error instanceof CompletionDeliveryError)
                throw error;
            if (!policyError && controller.signal.aborted) {
                await event("values", {
                    usage: {
                        ...usage,
                        ...(activeModelCalls || (producedOutput && !usage.total_tokens) ? { unmetered: true } : {}),
                    },
                    ai_sdk: { framework: "ai-sdk", generation, message_id: messageId },
                });
                await event("end", { status: "interrupted" });
                return "interrupted";
            }
            if (!controller.signal.aborted)
                controller.abort(error);
            if (usage.total_tokens || activeModelCalls || producedOutput)
                await event("values", {
                    usage: {
                        ...usage,
                        ...(activeModelCalls || (producedOutput && !usage.total_tokens) ? { unmetered: true } : {}),
                    },
                    ai_sdk: { framework: "ai-sdk", generation, message_id: messageId },
                });
            await event("error", {
                type: "ai_sdk_execution_failed",
                message: error instanceof Error ? error.message : String(error),
            });
            return "error";
        }
        finally {
            clearInterval(timer);
        }
    }
}
