/** Long-running agent for cancel testing -- TS mirror of examples/slow_agent/graph.py. */
import { Annotation, StateGraph, START, END } from "@langchain/langgraph";

const State = Annotation.Root({
  messages: Annotation<{ role: string; content: string }[]>({
    reducer: (a, b) => a.concat(b),
    default: () => [],
  }),
  step: Annotation<number>({ reducer: (_a, b) => b, default: () => 0 }),
});

async function sleep(ms: number) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function stepNode(state: typeof State.State) {
  await sleep(2000);
  return { step: state.step + 1 };
}

const builder = new StateGraph(State)
  .addNode("step1", stepNode)
  .addNode("step2", stepNode)
  .addNode("step3", stepNode)
  .addEdge(START, "step1")
  .addEdge("step1", "step2")
  .addEdge("step2", "step3")
  .addEdge("step3", END);

export const graph = builder.compile();
