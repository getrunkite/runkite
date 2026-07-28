/**
 * Factory graph example -- TS mirror of examples/factory_agent/graph.py.
 * Proves LangGraph SDK-compatible per-request graph factories work
 * end-to-end in the TypeScript runner too: a fresh graph instance built
 * per run, with runtime.user populated from whatever the control
 * plane's auth provider resolved for the request that created the run.
 *
 * Mirrors the shape of a real production graph.ts using this pattern
 * (`async function graph(config, runtime)`) without any real business
 * logic -- just enough to prove the mechanism: a module-level counter
 * that would leak across concurrent runs if the graph were shared (the
 * bug this feature exists to avoid), and the authenticated identity
 * echoed back in the response.
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

let instanceCounter = 0;

/** Per-request factory (any parameter order/subset of config/runtime is
 * supported -- see factoryGraph.ts). Deliberately untyped params, same
 * as Python's example importing ServerRuntime purely for a type hint --
 * a real graph.ts author has zero dependency on the runner package;
 * classification is based on parameter NAMES alone, not any imported
 * type. */
export async function graph(config: Record<string, unknown>, runtime: { user: { identity: string } | null }) {
  instanceCounter += 1;
  const myInstanceId = instanceCounter;

  const user = runtime.user;
  const identity = user ? user.identity : "anonymous";

  function respond(_state: typeof State.State) {
    return {
      messages: [{ role: "ai", content: `instance=${myInstanceId} identity=${identity}` }],
    };
  }

  const builder = new StateGraph(State).addNode("respond", respond).addEdge(START, "respond").addEdge("respond", END);
  return builder.compile();
}
