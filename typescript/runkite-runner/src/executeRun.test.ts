import { test } from "node:test";
import assert from "node:assert/strict";
import { executeRun, buildRunConfig, type RunAssignment, type RunEvent } from "./executeRun.js";
import type { LangGraphAdapter, RunnableGraph } from "./adapter.js";
import { RunnerUser } from "./runnerUser.js";

/** Minimal fake adapter exposing a single STATIC graph, avoiding a real
 * LangGraphAdapter (which needs a real config file + dynamic import).
 * isFactory always returns false -- see factoryGraph.test.ts and the
 * dedicated factory-graph cases below for the other path. */
function fakeAdapter(graph: RunnableGraph): LangGraphAdapter {
  return { getGraph: () => graph, isFactory: () => false } as unknown as LangGraphAdapter;
}

function assignment(overrides: Partial<RunAssignment> = {}): RunAssignment {
  return {
    run_id: "run-1",
    thread_id: "thread-1",
    graph_id: "test_graph",
    input: { messages: [{ role: "user", content: "hi" }] },
    stream_modes: ["values"],
    ...overrides,
  };
}

async function* asyncGen<T>(items: T[]): AsyncGenerator<T> {
  for (const item of items) yield item;
}

test("executeRun emits lifecycle running, values, and end success for a normal stream", async () => {
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([
        ["values", { messages: [{ role: "user", content: "hi" }] }],
        [
          "values",
          {
            messages: [
              { role: "user", content: "hi" },
              { role: "ai", content: "hello" },
            ],
          },
        ],
      ]);
    },
  };
  const events: RunEvent[] = [];
  const status = await executeRun(
    fakeAdapter(graph),
    assignment(),
    async (e) => void events.push(e),
    () => false,
  );

  assert.equal(status, "success");
  assert.deepEqual(
    events.map((e) => e.method),
    ["lifecycle", "values", "values", "end"],
  );
  assert.deepEqual(events[0].data, { event: "running" });
  assert.deepEqual(events[3].data, { status: "success" });
  // seq is monotonic and event_id embeds run_id + seq, matching the
  // control plane's expected shape (see runner-protocol/PROTOCOL.md).
  assert.deepEqual(
    events.map((e) => e.seq),
    [1, 2, 3, 4],
  );
  assert.equal(events[0].event_id, "run-1_evt_1");
});

test("executeRun detects __interrupt__ and emits lifecycle interrupted + input.requested + end interrupted", async () => {
  // This is the ACTUAL shape a real interrupt()/Command(resume) round trip
  // produces against the installed @langchain/langgraph version (verified
  // live: {id, value}, no "ns" field) -- not the {ns, resumable, when}
  // shape some published LangGraph.js docs/examples show for a different
  // version. Getting this shape wrong silently produces a synthesized
  // fallback interrupt_id that a client can never correctly correlate
  // back to a specific interrupt when a node raises more than one.
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([
        {
          __interrupt__: [{ id: "abc123", value: { question: "approve?" } }],
        },
      ]);
    },
  };
  const events: RunEvent[] = [];
  const status = await executeRun(
    fakeAdapter(graph),
    assignment(),
    async (e) => void events.push(e),
    () => false,
  );

  assert.equal(status, "interrupted");
  assert.deepEqual(
    events.map((e) => e.method),
    ["lifecycle", "lifecycle", "input.requested", "end"],
  );
  assert.deepEqual(events[1].data, { event: "interrupted" });
  assert.deepEqual(events[2].data, { interrupt_id: "abc123", value: { question: "approve?" } });
  assert.deepEqual(events[3].data, { status: "interrupted" });
});

test("executeRun captures usage from AI turns that ran before an interrupt", async () => {
  // A HITL pause must not drop tokens already spent getting there: the
  // first chunk is a normal AI turn carrying real usage_metadata, the
  // second chunk interrupts. Those tokens were already billed by the
  // provider before the pause happened.
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([
        {
          messages: [{ type: "ai", content: "thinking...", usage_metadata: { input_tokens: 120, output_tokens: 40 } }],
        },
        { __interrupt__: [{ id: "abc123", value: { question: "approve?" } }] },
      ]);
    },
  };
  const events: RunEvent[] = [];
  const status = await executeRun(
    fakeAdapter(graph),
    assignment(),
    async (e) => void events.push(e),
    () => false,
  );

  assert.equal(status, "interrupted");
  assert.equal(events.at(-1)?.method, "end");
  assert.deepEqual(events.at(-1)?.data, { status: "interrupted" });

  const usageEvents = events.filter((e) => e.method === "values" && (e.data as any)?.usage);
  assert.equal(usageEvents.length, 1, "expected exactly one values event carrying usage before the interrupted end");
  const usage = (usageEvents[0].data as any).usage;
  assert.equal(usage.prompt_tokens, 120);
  assert.equal(usage.completion_tokens, 40);

  // The usage-carrying event must land before "end", not after.
  const usageIdx = events.indexOf(usageEvents[0]);
  assert.ok(usageIdx < events.length - 1, "usage event must precede the final end event");
});

test("executeRun skipPrefix covers prior messages that have no id", async () => {
  // Bare concat reducers leave messages id-less; skipIds alone cannot
  // exclude them. getState length → skipPrefix must still drop prior turns.
  const graph: RunnableGraph = {
    async getState() {
      return {
        values: {
          messages: [
            { type: "human", content: "hi" },
            { type: "ai", content: "turn 1", usage_metadata: { input_tokens: 50, output_tokens: 20 } },
          ],
        },
      };
    },
    async stream() {
      return asyncGen([
        {
          messages: [
            { type: "human", content: "hi" },
            { type: "ai", content: "turn 1", usage_metadata: { input_tokens: 50, output_tokens: 20 } },
            { type: "human", content: "again" },
            { type: "ai", content: "turn 2", usage_metadata: { input_tokens: 90, output_tokens: 35 } },
          ],
        },
      ]);
    },
  };
  const events: RunEvent[] = [];
  const status = await executeRun(
    fakeAdapter(graph),
    assignment(),
    async (e) => void events.push(e),
    () => false,
  );
  assert.equal(status, "success");
  const usageEvents = events.filter((e) => e.method === "values" && (e.data as any)?.usage);
  const usage = (usageEvents[0].data as any).usage;
  assert.equal(usage.prompt_tokens, 90);
  assert.equal(usage.completion_tokens, 35);
});

test("executeRun does not recount usage from messages that existed before this run", async () => {
  // A live dogfood run on a real multi-turn thread found this: turn 2's
  // reported usage included turn 1's tokens again, because a stateful
  // graph's "values" snapshot is the *entire* message history (Runkite's
  // injected checkpointer keeps every prior AIMessage.usage_metadata in
  // state), not just the new turn. getState() reports what already
  // existed before this run started; that message's usage must be
  // excluded even though it is still present in the final "values" chunk.
  const graph: RunnableGraph = {
    async getState() {
      return { values: { messages: [{ id: "ai-turn-1", type: "ai", content: "turn 1 reply" }] } };
    },
    async stream() {
      return asyncGen([
        {
          messages: [
            { id: "human-turn-1", type: "human", content: "hi" },
            {
              id: "ai-turn-1",
              type: "ai",
              content: "turn 1 reply",
              usage_metadata: { input_tokens: 50, output_tokens: 20 },
            },
            { id: "human-turn-2", type: "human", content: "again" },
            {
              id: "ai-turn-2",
              type: "ai",
              content: "turn 2 reply",
              usage_metadata: { input_tokens: 90, output_tokens: 35 },
            },
          ],
        },
      ]);
    },
  };
  const events: RunEvent[] = [];
  const status = await executeRun(
    fakeAdapter(graph),
    assignment(),
    async (e) => void events.push(e),
    () => false,
  );

  assert.equal(status, "success");
  const usageEvents = events.filter((e) => e.method === "values" && (e.data as any)?.usage);
  assert.equal(usageEvents.length, 1);
  const usage = (usageEvents[0].data as any).usage;
  assert.equal(usage.prompt_tokens, 90, "must not include turn 1's 50 prompt tokens again");
  assert.equal(usage.completion_tokens, 35, "must not include turn 1's 20 completion tokens again");
});

test("executeRun resume does not recount pre-interrupt usage as new", async () => {
  // The interrupted run and its resume are two separate run_ids in two
  // separate usage_events rows. If the resumed run's final "values"
  // snapshot still contains the pre-interrupt AIMessage (it does -- HITL
  // resume never removes it) and usage is re-summed from scratch, that
  // one real provider call gets billed twice in Spend.
  const graph: RunnableGraph = {
    async getState() {
      return { values: { messages: [{ id: "ai-draft", type: "ai", content: "draft" }] } };
    },
    async stream() {
      return asyncGen([
        {
          messages: [
            { id: "human-1", type: "human", content: "do it" },
            { id: "ai-draft", type: "ai", content: "draft", usage_metadata: { input_tokens: 30, output_tokens: 15 } },
          ],
          approved: true,
        },
      ]);
    },
  };
  const events: RunEvent[] = [];
  const status = await executeRun(
    fakeAdapter(graph),
    assignment({ input: null, resume_command: { response: true } }),
    async (e) => void events.push(e),
    () => false,
  );

  assert.equal(status, "success");
  const usageEvents = events.filter((e) => e.method === "values" && (e.data as any)?.usage);
  assert.equal(usageEvents.length, 0, "resume must report no new usage -- the draft's tokens were already billed");
});

test("executeRun flags a real AI reply with no extractable usage as unmetered", async () => {
  // Simulates a brand-new/unrecognized provider integration: a real
  // AI-shaped reply with no usage_metadata field at all. Must surface as
  // an explicit unmetered marker, not silently look identical to a graph
  // that never called an LLM.
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([{ messages: [{ type: "ai", content: "a real reply from a provider we've never seen" }] }]);
    },
  };
  const events: RunEvent[] = [];
  const status = await executeRun(
    fakeAdapter(graph),
    assignment(),
    async (e) => void events.push(e),
    () => false,
  );

  assert.equal(status, "success");
  const usageEvents = events.filter((e) => e.method === "values" && (e.data as any)?.usage);
  assert.equal(usageEvents.length, 1);
  assert.deepEqual((usageEvents[0].data as any).usage, {
    prompt_tokens: 0,
    completion_tokens: 0,
    total_tokens: 0,
    unmetered: true,
  });
});

test("executeRun captures usage before interrupt with stream_modes updates only", async () => {
  // Same metering contract as the values-mode interrupt test, but with
  // stream_modes: ["updates"] alone — no lastValues snapshot ever exists,
  // so usage must come from incremental usageTotals, and the updates
  // chunk is wrapped as {nodeName: {messages: [...]}} (no top-level
  // messages key). Regression for both the fallback and the walker.
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([
        {
          agent: {
            messages: [{ type: "ai", content: "thinking...", usage_metadata: { input_tokens: 77, output_tokens: 33 } }],
          },
        },
        { __interrupt__: [{ id: "abc123", value: { question: "approve?" } }] },
      ]);
    },
  };
  const events: RunEvent[] = [];
  const status = await executeRun(
    fakeAdapter(graph),
    assignment({ stream_modes: ["updates"] }),
    async (e) => void events.push(e),
    () => false,
  );

  assert.equal(status, "interrupted");
  const usageEvents = events.filter((e) => e.method === "values" && (e.data as any)?.usage);
  assert.equal(usageEvents.length, 1, "expected usage via incremental updates-mode fallback");
  const usage = (usageEvents[0].data as any).usage;
  assert.equal(usage.prompt_tokens, 77);
  assert.equal(usage.completion_tokens, 33);
  assert.ok(events.indexOf(usageEvents[0]) < events.length - 1, "usage event must precede end");
});

test("executeRun falls back to ns-based interrupt_id when id is absent", async () => {
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([{ __interrupt__: [{ value: { question: "approve?" }, ns: ["approval:xyz"] }] }]);
    },
  };
  const events: RunEvent[] = [];
  await executeRun(
    fakeAdapter(graph),
    assignment(),
    async (e) => void events.push(e),
    () => false,
  );

  const requested = events.find((e) => e.method === "input.requested")!;
  assert.equal((requested.data as { interrupt_id: string }).interrupt_id, "approval:xyz");
});

test("executeRun maps resume_command to a Command({resume}) input", async () => {
  let capturedInput: unknown;
  const graph: RunnableGraph = {
    async stream(input) {
      capturedInput = input;
      return asyncGen([["values", { messages: [] }]]);
    },
  };
  await executeRun(
    fakeAdapter(graph),
    assignment({ resume_command: { response: true } }),
    async () => {},
    () => false,
  );

  const { Command } = await import("@langchain/langgraph");
  assert.ok(capturedInput instanceof Command, "expected input to be a Command instance");
  assert.equal((capturedInput as InstanceType<typeof Command>).resume, true);
});

test("executeRun stops and reports interrupted when isCancelled() becomes true mid-stream", async () => {
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([
        ["values", { messages: [{ role: "user", content: "hi" }] }],
        [
          "values",
          {
            messages: [
              { role: "user", content: "hi" },
              { role: "ai", content: "should not be emitted" },
            ],
          },
        ],
      ]);
    },
  };
  let cancelled = false;
  const events: RunEvent[] = [];
  const status = await executeRun(
    fakeAdapter(graph),
    assignment(),
    async (e) => {
      events.push(e);
      if (e.method === "values") cancelled = true; // cancel right after the first values event
    },
    () => cancelled,
  );

  assert.equal(status, "interrupted");
  assert.deepEqual(
    events.map((e) => e.method),
    ["lifecycle", "values", "end"],
  );
  assert.deepEqual(events[2].data, { status: "interrupted" });
});

test("executeRun emits an error event and returns status error when the graph throws", async () => {
  const graph: RunnableGraph = {
    async stream() {
      throw new Error("boom");
    },
  };
  const events: RunEvent[] = [];
  const status = await executeRun(
    fakeAdapter(graph),
    assignment(),
    async (e) => void events.push(e),
    () => false,
  );

  assert.equal(status, "error");
  assert.deepEqual(
    events.map((e) => e.method),
    ["lifecycle", "error"],
  );
  assert.equal((events[1].data as { message: string }).message, "boom");
});

test("executeRun serializes LangChain-message-like objects via toDict/toJSON", async () => {
  class FakeMessage {
    constructor(
      private role: string,
      private content: string,
    ) {}
    toDict() {
      return { role: this.role, content: this.content };
    }
  }
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([["values", { messages: [new FakeMessage("ai", "hello")] }]]);
    },
  };
  const events: RunEvent[] = [];
  await executeRun(
    fakeAdapter(graph),
    assignment(),
    async (e) => void events.push(e),
    () => false,
  );

  const valuesEvent = events.find((e) => e.method === "values")!;
  assert.deepEqual(valuesEvent.data, { messages: [{ role: "ai", content: "hello" }] });
});

test("executeRun emits a tool_call event for a new AIMessage.tool_calls entry", async () => {
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([
        [
          "values",
          {
            messages: [
              { role: "ai", content: "", tool_calls: [{ id: "call_1", name: "get_weather", args: { city: "SF" } }] },
            ],
          },
        ],
      ]);
    },
  };
  const events: RunEvent[] = [];
  const status = await executeRun(
    fakeAdapter(graph),
    assignment(),
    async (e) => void events.push(e),
    () => false,
  );

  assert.equal(status, "success");
  assert.deepEqual(
    events.map((e) => e.method),
    ["lifecycle", "tool_call", "values", "end"],
  );
  assert.deepEqual(events[1].data, { name: "get_weather", args: { city: "SF" }, id: "call_1" });
});

test("executeRun dedupes a tool_call already seen in an earlier chunk (e.g. re-streamed on a later graph step)", async () => {
  const toolCallMsg = {
    role: "ai",
    content: "",
    tool_calls: [{ id: "call_1", name: "get_weather", args: { city: "SF" } }],
  };
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([
        ["values", { messages: [toolCallMsg] }],
        ["values", { messages: [toolCallMsg, { role: "tool", content: "72F" }] }],
      ]);
    },
  };
  const events: RunEvent[] = [];
  await executeRun(
    fakeAdapter(graph),
    assignment(),
    async (e) => void events.push(e),
    () => false,
  );

  const toolCallEvents = events.filter((e) => e.method === "tool_call");
  assert.equal(
    toolCallEvents.length,
    1,
    "expected exactly one tool_call event despite the same id appearing in two chunks",
  );
});

test("executeRun emits a separate tool_call event per distinct id, and does not recurse past a message that has tool_calls", async () => {
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([
        [
          "values",
          {
            messages: [
              {
                role: "ai",
                content: "",
                tool_calls: [
                  { id: "call_1", name: "get_weather", args: { city: "SF" } },
                  { id: "call_2", name: "get_time", args: { tz: "PST" } },
                ],
                // Nested field that would itself contain a tool_calls-shaped
                // value -- must NOT be scanned once the message's own
                // tool_calls array already matched (mirrors Python's
                // if/elif branching, not an unconditional recursive scan).
                extra: { tool_calls: [{ id: "call_should_not_emit", name: "ignored" }] },
              },
            ],
          },
        ],
      ]);
    },
  };
  const events: RunEvent[] = [];
  await executeRun(
    fakeAdapter(graph),
    assignment(),
    async (e) => void events.push(e),
    () => false,
  );

  const toolCallEvents = events.filter((e) => e.method === "tool_call");
  assert.deepEqual(
    toolCallEvents.map((e) => (e.data as { id: string }).id),
    ["call_1", "call_2"],
  );
});

test("executeRun ignores a tool_calls entry with no id (can't be deduped/correlated)", async () => {
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([["values", { messages: [{ role: "ai", tool_calls: [{ name: "no_id_here", args: {} }] }] }]]);
    },
  };
  const events: RunEvent[] = [];
  await executeRun(
    fakeAdapter(graph),
    assignment(),
    async (e) => void events.push(e),
    () => false,
  );

  assert.equal(events.filter((e) => e.method === "tool_call").length, 0);
});

test("buildRunConfig sets thread_id, run_id, assistant_id, and graph_id on configurable", () => {
  const config = buildRunConfig(assignment({ run_id: "run-1", thread_id: "thread-1", graph_id: "my_graph" }));
  assert.equal(config.configurable.thread_id, "thread-1");
  assert.equal(config.configurable.run_id, "run-1");
  assert.equal(config.configurable.generation, 0);
  assert.equal(config.configurable.assistant_id, "my_graph");
  assert.equal(config.configurable.graph_id, "my_graph");
});

test("buildRunConfig echoes assignment.generation onto configurable", () => {
  const config = buildRunConfig(assignment({ generation: 3 }));
  assert.equal(config.configurable.generation, 3);
});

test("buildRunConfig preserves an existing config.configurable's own fields", () => {
  const config = buildRunConfig(assignment({ config: { configurable: { recursion_limit: 10 } } }));
  assert.equal(config.configurable.recursion_limit, 10);
  assert.equal(config.configurable.run_id, "run-1");
});

test("buildRunConfig omits langgraph_auth_user/user_id/user_display_name when assignment has no user", () => {
  const config = buildRunConfig(assignment());
  assert.equal("langgraph_auth_user" in config.configurable, false);
  assert.equal("user_id" in config.configurable, false);
  assert.equal("user_display_name" in config.configurable, false);
});

test("buildRunConfig sets langgraph_auth_user/user_id/user_display_name from assignment.user", () => {
  const config = buildRunConfig(
    assignment({
      user: { identity: "alice", display_name: "Alice A.", is_authenticated: true, email: "alice@example.com" },
    }),
  );
  assert.ok(config.configurable.langgraph_auth_user instanceof RunnerUser);
  assert.equal(config.configurable.langgraph_auth_user.identity, "alice");
  assert.equal(config.configurable.langgraph_auth_user.get("email"), "alice@example.com");
  assert.equal(config.configurable.user_id, "alice");
  assert.equal(config.configurable.user_display_name, "Alice A.");
});

test("buildRunConfig does not mutate the original assignment.config object", () => {
  const originalConfig = { configurable: { recursion_limit: 10 } };
  const a = assignment({ config: originalConfig });
  buildRunConfig(a);
  assert.deepEqual(
    originalConfig,
    { configurable: { recursion_limit: 10 } },
    "assignment.config must not be mutated by buildRunConfig",
  );
});

test("buildRunConfig maps checkpoint_ref to configurable.checkpoint_id", () => {
  const config = buildRunConfig(assignment({ checkpoint_ref: "  past-cp-42  " }));
  assert.equal(config.configurable.checkpoint_id, "past-cp-42");
  assert.equal("checkpoint_id" in buildRunConfig(assignment({ checkpoint_ref: null })).configurable, false);
  assert.equal("checkpoint_id" in buildRunConfig(assignment({ checkpoint_ref: "   " })).configurable, false);
});

test("buildRunConfig tenant-scopes configurable.thread_id for non-default tenants", () => {
  assert.equal(buildRunConfig(assignment({ thread_id: "t1" })).configurable.thread_id, "t1");
  assert.equal(buildRunConfig(assignment({ thread_id: "t1", tenant_id: "default" })).configurable.thread_id, "t1");
  assert.equal(buildRunConfig(assignment({ thread_id: "t1", tenant_id: "  " })).configurable.thread_id, "t1");
  assert.equal(buildRunConfig(assignment({ thread_id: "t1", tenant_id: "acme" })).configurable.thread_id, "acme:t1");
});

// -- Factory graph integration ------------------------------------------
// executeRun.ts's own responsibility here is narrow and already tested
// thoroughly at the unit level in factoryGraph.test.ts (classification,
// param extraction, open/close semantics) -- these cases verify
// executeRun.ts correctly WIRES that machinery in: it asks the adapter
// whether a graph_id is a factory, builds+opens it before streaming,
// and closes it afterward regardless of how the run ends.

function fakeFactoryAdapter(
  graph: RunnableGraph,
  opts: { onOpen?: () => void; onClose?: () => void } = {},
): LangGraphAdapter {
  let closed = false;
  return {
    isFactory: () => true,
    buildFactoryGraph: (_graphId: string, _config: unknown, _runContext: unknown) => ({
      open: async () => {
        opts.onOpen?.();
        return graph;
      },
      close: async () => {
        closed = true;
        opts.onClose?.();
      },
    }),
    getGraph: () => {
      throw new Error("getGraph should never be called for a factory graph_id");
    },
    // Exposed for assertions below.
    __wasClosed: () => closed,
  } as unknown as LangGraphAdapter;
}

test("executeRun builds and opens a factory graph (not getGraph) when adapter.isFactory returns true", async () => {
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([["values", { messages: [{ role: "ai", content: "hi from factory" }] }]]);
    },
  };
  let opened = false;
  const adapter = fakeFactoryAdapter(graph, { onOpen: () => (opened = true) });

  const events: RunEvent[] = [];
  const status = await executeRun(
    adapter,
    assignment(),
    async (e) => void events.push(e),
    () => false,
  );

  assert.equal(status, "success");
  assert.equal(opened, true);
  assert.deepEqual(
    events.map((e) => e.method),
    ["lifecycle", "values", "end"],
  );
});

test("executeRun closes the factory build after a successful run", async () => {
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([["values", {}]]);
    },
  };
  const adapter = fakeFactoryAdapter(graph);
  await executeRun(
    adapter,
    assignment(),
    async () => {},
    () => false,
  );
  assert.equal((adapter as any).__wasClosed(), true);
});

test("executeRun closes the factory build even when the graph throws mid-stream", async () => {
  const graph: RunnableGraph = {
    async stream() {
      throw new Error("boom");
    },
  };
  const adapter = fakeFactoryAdapter(graph);
  const status = await executeRun(
    adapter,
    assignment(),
    async () => {},
    () => false,
  );
  assert.equal(status, "error");
  assert.equal((adapter as any).__wasClosed(), true, "factory build must be closed even after an error");
});

test("executeRun closes the factory build even when open() itself throws", async () => {
  let closed = false;
  const adapter = {
    isFactory: () => true,
    buildFactoryGraph: () => ({
      open: async () => {
        throw new Error("open failed after allocating resources");
      },
      close: async () => {
        closed = true;
      },
    }),
    getGraph: () => {
      throw new Error("should not be called");
    },
  } as unknown as LangGraphAdapter;

  const status = await executeRun(
    adapter,
    assignment(),
    async () => {},
    () => false,
  );
  assert.equal(status, "error");
  assert.equal(closed, true, "factory build must be closed even when open() throws -- otherwise dispose()-ables leak");
});

test("executeRun closes the factory build even when the run is cancelled mid-stream", async () => {
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([
        ["values", { messages: [{ role: "user", content: "hi" }] }],
        ["values", { messages: [{ role: "ai", content: "should not be reached" }] }],
      ]);
    },
  };
  const adapter = fakeFactoryAdapter(graph);
  let cancelled = false;
  const status = await executeRun(
    adapter,
    assignment(),
    async (e) => {
      if (e.method === "values") cancelled = true;
    },
    () => cancelled,
  );
  assert.equal(status, "interrupted");
  assert.equal((adapter as any).__wasClosed(), true, "factory build must be closed even after a mid-stream cancel");
});

test("executeRun passes threadId/runId/user through to the factory's RunFactoryContext", async () => {
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([["values", {}]]);
    },
  };
  let capturedContext: any;
  const adapter = {
    isFactory: () => true,
    buildFactoryGraph: (_graphId: string, _config: unknown, runContext: unknown) => {
      capturedContext = runContext;
      return { open: async () => graph, close: async () => {} };
    },
    getGraph: () => {
      throw new Error("should not be called");
    },
  } as unknown as LangGraphAdapter;

  await executeRun(
    adapter,
    assignment({ run_id: "run-xyz", thread_id: "thread-xyz", user: { identity: "alice" } }),
    async () => {},
    () => false,
  );

  assert.equal(capturedContext.runId, "run-xyz");
  assert.equal(capturedContext.threadId, "thread-xyz");
  assert.deepEqual(capturedContext.user, { identity: "alice" });
});

test("executeRun sets configurable.thread_id and run_id on the config passed to stream()", async () => {
  let capturedConfig: any;
  const graph: RunnableGraph = {
    async stream(_input, config) {
      capturedConfig = config;
      return asyncGen([["values", {}]]);
    },
  };
  await executeRun(
    fakeAdapter(graph),
    assignment({ thread_id: "my-thread", run_id: "my-run" }),
    async () => {},
    () => false,
  );

  assert.equal(capturedConfig.configurable.thread_id, "my-thread");
  assert.equal(capturedConfig.configurable.run_id, "my-run");
});

test("executeRun denies a tool_call not in allowed_tools before continuing the stream", async () => {
  let pulls = 0;
  const graph: RunnableGraph = {
    async stream() {
      // Second yield throws: proves the runner stops after deny and does
      // not pull the next chunk (where ToolNode side effects would appear).
      return (async function* () {
        pulls += 1;
        yield [
          "values",
          {
            messages: [{ role: "ai", content: "", tool_calls: [{ id: "call_1", name: "forbidden", args: {} }] }],
          },
        ] as const;
        pulls += 1;
        throw new Error("second stream chunk must not be pulled after allowed_tools deny");
      })();
    },
  };
  const events: RunEvent[] = [];
  const status = await executeRun(
    fakeAdapter(graph),
    assignment({ allowed_tools: ["search"] }),
    async (e) => void events.push(e),
    () => false,
  );
  assert.equal(status, "error");
  assert.equal(pulls, 1, "runner must not pull past the denied tool_call chunk");
  assert.equal(events.map((e) => e.method).includes("tool_call"), true);
  assert.equal(events.map((e) => e.method).includes("tool_auth"), true);
  assert.equal(events.at(-1)?.method, "end");
  assert.deepEqual(events.at(-1)?.data, { status: "error" });
  const auth = events.find((e) => e.method === "tool_auth")!;
  assert.equal((auth.data as any).reason_code, "tool_not_allowed");
});

test("executeRun denies after empty-name chunk then same id with forbidden name", async () => {
  // Regression: marking seen on empty name would skip the named chunk and
  // false-allow a disallowed tool (messages-style streaming).
  let pulls = 0;
  const graph: RunnableGraph = {
    async stream() {
      return (async function* () {
        pulls += 1;
        yield [
          "values",
          {
            messages: [{ role: "ai", content: "", tool_calls: [{ id: "call_stream", name: "", args: {} }] }],
          },
        ] as const;
        pulls += 1;
        yield [
          "values",
          {
            messages: [{ role: "ai", content: "", tool_calls: [{ id: "call_stream", name: "forbidden", args: {} }] }],
          },
        ] as const;
        pulls += 1;
        throw new Error("third stream chunk must not be pulled after allowed_tools deny");
      })();
    },
  };
  const events: RunEvent[] = [];
  const status = await executeRun(
    fakeAdapter(graph),
    assignment({ allowed_tools: ["search"] }),
    async (e) => void events.push(e),
    () => false,
  );
  assert.equal(status, "error");
  assert.equal(pulls, 2, "runner must pull the named chunk and stop after deny");
  assert.equal(events.map((e) => e.method).includes("tool_auth"), true);
  assert.equal(events.at(-1)?.method, "end");
  assert.deepEqual(events.at(-1)?.data, { status: "error" });
  const auth = events.find((e) => e.method === "tool_auth")!;
  assert.equal((auth.data as any).reason_code, "tool_not_allowed");
  assert.equal((auth.data as any).tool, "forbidden");
});
