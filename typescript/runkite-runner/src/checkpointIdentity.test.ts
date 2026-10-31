import assert from "node:assert/strict";
import test from "node:test";
import { Annotation, END, MemorySaver, START, StateGraph } from "@langchain/langgraph";
import { LangGraphAdapter, type RunnableGraph } from "./adapter.js";
import { CheckpointerManager } from "./checkpoint.js";
import type { RunAssignment, RunEvent } from "./executeRun.js";
import { checkpointThreadId } from "./tenantCtx.js";

function assignment(tenant: string, thread: string): RunAssignment {
  return { run_id: "identity-run", tenant_id: tenant, thread_id: thread, graph_id: "graph", input: {} };
}

test("persistent legacy ownership is not inferred from an otherwise valid new named pair", async () => {
  for (const [legacyTenant, legacyThread, newTenant, newThread] of [
    ["default", "acme:target", "acme", "target"],
    ["acme:west", "target", "acme", "west:target"],
  ]) {
    const State = Annotation.Root({ ownerOnly: Annotation<string>() });
    const saver = new MemorySaver();
    const graph = new StateGraph(State)
      .addNode("keep", (state) => state)
      .addEdge(START, "keep")
      .addEdge("keep", END)
      .compile({ checkpointer: saver });
    await graph.invoke(
      { ownerOnly: "legacy owner's synthetic conversation" },
      {
        configurable: { thread_id: checkpointThreadId(legacyTenant, legacyThread) },
      },
    );
    const manager = new CheckpointerManager();
    manager.mode = "direct-postgres";
    manager.checkpointer = saver;
    const adapter = new LangGraphAdapter("unused.json");
    adapter.attachCheckpointer(manager);
    let lookups = 0;
    adapter.getGraph = () => {
      lookups++;
      return graph as unknown as RunnableGraph;
    };
    const events: RunEvent[] = [];
    const status = await adapter.execute(
      assignment(newTenant, newThread),
      async (event) => void events.push(event),
      () => false,
    );
    assert.equal(status, "error", `legacy owner data escaped: ${JSON.stringify(events)}`);
    assert.equal(lookups, 0);
    assert.match((events[0].data as { message: string }).message, /ownership/);
  }
});

for (const [ownerTenant, ownerThread, callerTenant, callerThread] of [
  ["acme", "target", "default", "acme:target"],
  ["acme", "west:target", "acme:west", "target"],
]) {
  test(`native saver refuses the aliased ${callerTenant}/${callerThread} identity before state access`, async () => {
    const State = Annotation.Root({ ownerOnly: Annotation<string>() });
    const saver = new MemorySaver();
    const graph = new StateGraph(State)
      .addNode("keep", (state) => state)
      .addEdge(START, "keep")
      .addEdge("keep", END)
      .compile({ checkpointer: saver });
    const owner = { configurable: { thread_id: checkpointThreadId(ownerTenant, ownerThread) } };
    const caller = { configurable: { thread_id: checkpointThreadId(callerTenant, callerThread) } };
    assert.deepEqual(owner, caller, "the historical key framing demonstrates the alias");
    await graph.invoke({ ownerOnly: "synthetic owner conversation" }, owner);
    for (const mode of ["direct-postgres", "memory"] as const) {
      const manager = new CheckpointerManager();
      manager.mode = mode;
      manager.checkpointer = saver;
      const adapter = new LangGraphAdapter("unused.json");
      adapter.attachCheckpointer(manager);
      let lookups = 0;
      adapter.getGraph = () => {
        lookups++;
        return graph as unknown as RunnableGraph;
      };
      const events: RunEvent[] = [];
      assert.equal(
        await adapter.execute(
          assignment(callerTenant, callerThread),
          async (event) => void events.push(event),
          () => false,
        ),
        "error",
      );
      assert.equal(lookups, 0);
      assert.deepEqual(
        events.map((event) => event.method),
        ["error"],
      );
      assert.match((events[0].data as { message: string }).message, /ambiguous native LangGraph/);
    }
    assert.equal((await graph.getState(owner)).values.ownerOnly, "synthetic owner conversation");
  });
}

test("native ambiguity fails before factory construction", async () => {
  for (const [mode, tenant, thread] of [
    ["memory", "default", "cron:daily"],
    ["direct-postgres", "acme", "target"],
  ] as const) {
    const manager = new CheckpointerManager();
    manager.mode = mode;
    const adapter = new LangGraphAdapter("unused.json");
    adapter.attachCheckpointer(manager);
    let built = 0;
    adapter.isFactory = () => true;
    adapter.buildFactoryGraph = () => {
      built++;
      throw new Error("factory must not run");
    };
    assert.equal(
      await adapter.execute(
        assignment(tenant, thread),
        async () => {},
        () => false,
      ),
      "error",
    );
    assert.equal(built, 0);
  }
});

test("proxy saver retains exact plane identities despite a native delimiter shape", async () => {
  for (const [tenant, thread] of [
    ["default", "cron:daily"],
    ["acme:west", "target"],
  ]) {
    const manager = new CheckpointerManager();
    manager.mode = "proxy-http";
    const adapter = new LangGraphAdapter("unused.json");
    adapter.attachCheckpointer(manager);
    let key: string | undefined;
    adapter.getGraph = () => ({
      async stream(_input, config: any) {
        key = config.configurable.thread_id;
        return (async function* () {
          yield ["values", { answer: "proxy path" }];
        })();
      },
    });
    assert.equal(
      await adapter.execute(
        assignment(tenant, thread),
        async () => {},
        () => false,
      ),
      "success",
    );
    assert.equal(key, checkpointThreadId(tenant, thread));
  }
});

test("safe native keys retain their existing conversation rows", async () => {
  for (const [mode, tenant, thread] of [
    ["memory", "acme", "west:target"],
    ["direct-postgres", "default", "existing-target"],
  ] as const) {
    const State = Annotation.Root({ prior: Annotation<string>() });
    const saver = new MemorySaver();
    const graph = new StateGraph(State)
      .addNode("keep", (state) => state)
      .addEdge(START, "keep")
      .addEdge("keep", END)
      .compile({ checkpointer: saver });
    const owner = { configurable: { thread_id: checkpointThreadId(tenant, thread) } };
    await graph.invoke({ prior: "existing native conversation" }, owner);
    const manager = new CheckpointerManager();
    manager.mode = mode;
    manager.checkpointer = saver;
    const adapter = new LangGraphAdapter("unused.json");
    adapter.attachCheckpointer(manager);
    adapter.getGraph = () => graph as unknown as RunnableGraph;
    assert.equal(
      await adapter.execute(
        assignment(tenant, thread),
        async () => {},
        () => false,
      ),
      "success",
    );
    assert.equal((await graph.getState(owner)).values.prior, "existing native conversation");
  }
});
