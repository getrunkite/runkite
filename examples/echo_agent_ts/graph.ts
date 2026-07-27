/**
 * Simple echo agent for TypeScript runner validation -- direct TS mirror
 * of examples/echo_agent/graph.py. Receives a user message and echoes it
 * back. Proves the TypeScript runner works with a real LangGraph.js graph,
 * same as the Python spike proved it for LangGraph.
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

function echoNode(state: typeof State.State) {
  const lastMessage = state.messages[state.messages.length - 1];
  return { messages: [{ role: "ai", content: lastMessage.content }] };
}

const builder = new StateGraph(State).addNode("echo", echoNode).addEdge(START, "echo").addEdge("echo", END);

export const graph = builder.compile();
