/**
 * LangGraph.js agent powered by Gemini (real LLM).
 * Requires GOOGLE_API_KEY in the environment (load from repo-root .env.llm).
 */
import { ChatGoogleGenerativeAI } from "@langchain/google-genai";
import { Annotation, END, START, StateGraph } from "@langchain/langgraph";

interface Message {
  role: string;
  content: string;
}

const State = Annotation.Root({
  messages: Annotation<Message[]>({
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
  const resp = await llm.invoke([
    { role: "system", content: "You are a terse, helpful assistant. Reply in one short sentence." },
    { role: "user", content: last.content },
  ]);
  const content = typeof resp.content === "string" ? resp.content : JSON.stringify(resp.content);
  return { messages: [{ role: "ai", content }] };
}

const builder = new StateGraph(State)
  .addNode("agent", agentNode)
  .addEdge(START, "agent")
  .addEdge("agent", END);

export const graph = builder.compile();
