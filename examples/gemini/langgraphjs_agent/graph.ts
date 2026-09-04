/**
 * LangGraph.js agent powered by Gemini (real LLM).
 * Requires GOOGLE_API_KEY in the environment (load from repo-root .env.llm).
 *
 * Returns the model AIMessage (with usage_metadata) into state — not a
 * stripped {role,content} dict — so the runner's accumulateUsage can meter
 * FinOps the same way as the Python LangGraph example.
 */
import { ChatGoogleGenerativeAI } from "@langchain/google-genai";
import { Annotation, END, START, StateGraph } from "@langchain/langgraph";
import type { BaseMessage } from "@langchain/core/messages";

const State = Annotation.Root({
  messages: Annotation<BaseMessage[]>({
    reducer: (a, b) => a.concat(b),
    default: () => [],
  }),
});

const modelName = process.env.GEMINI_MODEL || "gemini-2.0-flash";
const temperature = Number(process.env.GEMINI_TEMPERATURE || "0");

const llm = new ChatGoogleGenerativeAI({
  model: modelName,
  apiKey: process.env.GOOGLE_API_KEY,
  temperature,
});

async function agentNode(state: typeof State.State) {
  const last = state.messages[state.messages.length - 1];
  const lastContent = typeof last.content === "string" ? last.content : JSON.stringify(last.content);
  const resp = await llm.invoke([
    { role: "system", content: "You are a terse, helpful assistant. Reply in one short sentence." },
    { role: "user", content: lastContent },
  ]);
  // ChatGoogleGenerativeAI returns usage_metadata tokens but omits model
  // from response_metadata — stamp it so pricebook soft-match can price USD.
  resp.response_metadata = {
    ...(resp.response_metadata ?? {}),
    model_name: modelName,
  };
  // Keep the AIMessage intact (usage_metadata / response_metadata) for FinOps.
  return { messages: [resp] };
}

const builder = new StateGraph(State)
  .addNode("agent", agentNode)
  .addEdge(START, "agent")
  .addEdge("agent", END);

export const graph = builder.compile();
