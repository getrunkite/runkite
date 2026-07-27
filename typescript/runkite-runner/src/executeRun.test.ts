import { test } from "node:test";
import assert from "node:assert/strict";
import { executeRun, type RunAssignment, type RunEvent } from "./executeRun.js";
import type { LangGraphAdapter, RunnableGraph } from "./adapter.js";

/** Minimal fake adapter exposing a single graph, avoiding a real
 * LangGraphAdapter (which needs a real config file + dynamic import). */
function fakeAdapter(graph: RunnableGraph): LangGraphAdapter {
  return { getGraph: () => graph } as unknown as LangGraphAdapter;
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
        ["values", { messages: [{ role: "user", content: "hi" }, { role: "ai", content: "hello" }] }],
      ]);
    },
  };
  const events: RunEvent[] = [];
  const status = await executeRun(fakeAdapter(graph), assignment(), async (e) => void events.push(e), () => false);

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
  const status = await executeRun(fakeAdapter(graph), assignment(), async (e) => void events.push(e), () => false);

  assert.equal(status, "interrupted");
  assert.deepEqual(
    events.map((e) => e.method),
    ["lifecycle", "lifecycle", "input.requested", "end"],
  );
  assert.deepEqual(events[1].data, { event: "interrupted" });
  assert.deepEqual(events[2].data, { interrupt_id: "abc123", value: { question: "approve?" } });
  assert.deepEqual(events[3].data, { status: "interrupted" });
});

test("executeRun falls back to ns-based interrupt_id when id is absent", async () => {
  const graph: RunnableGraph = {
    async stream() {
      return asyncGen([{ __interrupt__: [{ value: { question: "approve?" }, ns: ["approval:xyz"] }] }]);
    },
  };
  const events: RunEvent[] = [];
  await executeRun(fakeAdapter(graph), assignment(), async (e) => void events.push(e), () => false);

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
        ["values", { messages: [{ role: "user", content: "hi" }, { role: "ai", content: "should not be emitted" }] }],
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
  const status = await executeRun(fakeAdapter(graph), assignment(), async (e) => void events.push(e), () => false);

  assert.equal(status, "error");
  assert.deepEqual(
    events.map((e) => e.method),
    ["lifecycle", "error"],
  );
  assert.equal((events[1].data as { message: string }).message, "boom");
});

test("executeRun serializes LangChain-message-like objects via toDict/toJSON", async () => {
  class FakeMessage {
    constructor(private role: string, private content: string) {}
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
  await executeRun(fakeAdapter(graph), assignment(), async (e) => void events.push(e), () => false);

  const valuesEvent = events.find((e) => e.method === "values")!;
  assert.deepEqual(valuesEvent.data, { messages: [{ role: "ai", content: "hello" }] });
});

test("executeRun sets configurable.thread_id and run_id on the config passed to stream()", async () => {
  let capturedConfig: any;
  const graph: RunnableGraph = {
    async stream(_input, config) {
      capturedConfig = config;
      return asyncGen([["values", {}]]);
    },
  };
  await executeRun(fakeAdapter(graph), assignment({ thread_id: "my-thread", run_id: "my-run" }), async () => {}, () => false);

  assert.equal(capturedConfig.configurable.thread_id, "my-thread");
  assert.equal(capturedConfig.configurable.run_id, "my-run");
});
