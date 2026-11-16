import { createOpenAI } from "@ai-sdk/openai";
import type { LanguageModel } from "ai";

export const SCRIPTED_NOTE = Object.freeze({
  title: "Release plan",
  body: "Review the changes before publishing.",
});

export type ApplicationModel = {
  model: LanguageModel;
  mode: "provider" | "scripted";
};

/** The scripted model proposes a fixed local note; the plane still decides. */
export async function createApplicationModel(
  environment: NodeJS.ProcessEnv = process.env,
  providerFactory: typeof createOpenAI = createOpenAI,
): Promise<ApplicationModel> {
  const mode = environment.RUNKITE_DEMO_MODEL ?? "provider";
  if (mode === "scripted") {
    const { MockLanguageModelV4 } = await import("ai/test");
    return {
      mode,
      model: new MockLanguageModelV4({
        provider: "runkite-local-script",
        modelId: "scripted-note-proposal",
        doGenerate: {
          content: [
            {
              type: "text",
              text: "Scripted local demo: proposing the fixed note. Check the job for its governance outcome.",
            },
            {
              type: "tool-call",
              toolCallId: "scripted-note-one",
              toolName: "createNote",
              input: JSON.stringify(SCRIPTED_NOTE),
            },
          ],
          finishReason: { unified: "tool-calls", raw: undefined },
          usage: {
            inputTokens: { total: 0, noCache: 0, cacheRead: 0, cacheWrite: 0 },
            outputTokens: { total: 0, text: 0, reasoning: 0 },
          },
          warnings: [],
        },
      }),
    };
  }
  if (mode !== "provider") {
    throw new Error("RUNKITE_DEMO_MODEL must be provider or scripted");
  }
  const required = (name: string): string => {
    const value = environment[name];
    if (!value) throw new Error(`${name} is required`);
    return value;
  };
  const apiKey = required("OPENAI_API_KEY");
  const modelId = required("MODEL");
  return { mode, model: providerFactory({ apiKey })(modelId) };
}
