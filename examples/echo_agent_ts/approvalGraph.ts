/**
 * HITL interrupt/resume agent -- TS mirror of examples/approval_agent/graph.py.
 * Proves VG-003 (interrupt -> human approval -> resume -> completion) for
 * the TypeScript runner, the same validation gate already proven for Python.
 */
import { Annotation, StateGraph, START, END, interrupt } from "@langchain/langgraph";

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

function proposeAction(state: typeof State.State) {
  const approved = interrupt({ action: "send_email", to: "alice@example.com" });
  return { messages: [{ role: "ai", content: `Action approved: ${approved}` }] };
}

function executeAction(state: typeof State.State) {
  return { messages: [{ role: "ai", content: "Email sent to alice@example.com successfully!" }] };
}

const builder = new StateGraph(State)
  .addNode("propose_action", proposeAction)
  .addNode("execute_action", executeAction)
  .addEdge(START, "propose_action")
  .addEdge("propose_action", "execute_action")
  .addEdge("execute_action", END);

export const graph = builder.compile();
