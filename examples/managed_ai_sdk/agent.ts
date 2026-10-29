/** Offline native AI SDK agent. Replace the mock model with your provider model. */
import { jsonSchema, simulateReadableStream, tool } from "ai";
import { MockLanguageModelV4 } from "ai/test";
import type { AiSdkAgentDefinition, AiSdkRunContext } from "runkite-runner/ai-sdk";

export function createAgent(_context: AiSdkRunContext): AiSdkAgentDefinition {
  const model = new MockLanguageModelV4({
    doStream: async (options) => {
      const userMessages = options.prompt.filter((message) => message.role === "user");
      const newest = userMessages.at(-1);
      const text = newest?.content.map((part) => part.type === "text" ? part.text : "").join("") ?? "";
      if (text.includes("slow")) {
        await new Promise<void>((resolve, reject) => {
          const timer = setTimeout(resolve, 10_000);
          const abort = () => { clearTimeout(timer); reject(options.abortSignal?.reason); };
          options.abortSignal?.addEventListener("abort", abort, { once: true });
          if (options.abortSignal?.aborted) abort();
        });
      }
      const callTool = options.prompt.at(-1)?.role === "user" && text.includes("echo");
      const chunks = callTool ? [
        { type: "tool-call" as const, toolCallId: "echo-call", toolName: "echo", input: JSON.stringify({ text }) },
      ] : [
        { type: "text-start" as const, id: "reply" },
        { type: "text-delta" as const, id: "reply", delta: `Remembered user turns: ${userMessages.length}. ` },
        { type: "text-delta" as const, id: "reply", delta: text },
        { type: "text-end" as const, id: "reply" },
      ];
      return { stream: simulateReadableStream({ initialDelayInMs: null, chunkDelayInMs: null, chunks: [
        { type: "stream-start", warnings: [] }, ...chunks,
        { type: "finish", finishReason: { unified: callTool ? "tool-calls" : "stop", raw: undefined },
          usage: { inputTokens: { total: 10, noCache: 10, cacheRead: 0, cacheWrite: 0 },
            outputTokens: { total: 2, text: 2, reasoning: 0 } } },
      ] }) };
    },
  });
  return {
    model,
    instructions: "Answer the user's request. Use echo only when asked.",
    maxRetries: 0,
    tools: {
      echo: tool({ description: "Return the supplied text without external effects.",
        inputSchema: jsonSchema<{ text: string }>({ type: "object", properties: { text: { type: "string" } },
          required: ["text"], additionalProperties: false }),
        execute: async ({ text }) => ({ text }),
      }),
    },
  };
}
