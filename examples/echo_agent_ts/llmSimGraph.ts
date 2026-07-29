/**
 * Simulated-LLM-latency benchmark agent -- TS mirror of
 * examples/llm_sim_agent/graph.py. See that file's doc comment for the
 * full rationale (bench/REPORT.md finding 1d's flagged follow-up: a
 * sustained-load, realistic-latency benchmark for --concurrency,
 * distinct from echo_agent's deliberately near-zero-compute baseline).
 */
import { Annotation, StateGraph, START, END } from "@langchain/langgraph";

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

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// Same default and same rationale as the Python version: a fast,
// typical single-turn LLM response, not a worst case.
const LLM_SIM_DELAY_MS = Number.parseInt(process.env.LLM_SIM_DELAY_MS ?? "800", 10);

async function simulatedLlmCall(state: typeof State.State) {
  await sleep(LLM_SIM_DELAY_MS);
  const lastMessage = state.messages[state.messages.length - 1];
  return {
    messages: [{ role: "ai", content: `(simulated ${LLM_SIM_DELAY_MS}ms LLM response to: ${lastMessage.content})` }],
  };
}

const builder = new StateGraph(State).addNode("llm_call", simulatedLlmCall).addEdge(START, "llm_call").addEdge("llm_call", END);

export const graph = builder.compile();
