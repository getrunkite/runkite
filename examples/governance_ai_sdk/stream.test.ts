import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { simulateReadableStream, streamText } from "ai";
import { MockLanguageModelV4 } from "ai/test";
import { createGovernanceClient } from "runkite-governance";
import { NoteStore, type NoteJob } from "./state.ts";
import { createNoteTool } from "./tools.ts";

const note = { title: "Streamed request", body: "The original tool arguments" };
const textChunks = ["Creating ", "a note."];

type ModelStreamPart =
  Awaited<
    ReturnType<MockLanguageModelV4["doStream"]>
  >["stream"] extends ReadableStream<infer Part>
    ? Part
    : never;

function model() {
  const chunks: ModelStreamPart[] = [
    { type: "stream-start", warnings: [] },
    { type: "text-start", id: "text-one" },
    ...textChunks.map((delta): ModelStreamPart => ({
      type: "text-delta",
      id: "text-one",
      delta,
    })),
    { type: "text-end", id: "text-one" },
    { type: "tool-input-start", id: "call-one", toolName: "createNote" },
    { type: "tool-input-delta", id: "call-one", delta: JSON.stringify(note) },
    { type: "tool-input-end", id: "call-one" },
    {
      type: "tool-call",
      toolCallId: "call-one",
      toolName: "createNote",
      input: JSON.stringify(note),
    },
    {
      type: "finish",
      finishReason: { unified: "tool-calls", raw: undefined },
      usage: {
        inputTokens: { total: 1, noCache: 1, cacheRead: 0, cacheWrite: 0 },
        outputTokens: { total: 2, text: 2, reasoning: 0 },
      },
    },
  ];
  return new MockLanguageModelV4({
    doStream: {
      stream: simulateReadableStream({
        chunks,
        initialDelayInMs: null,
        chunkDelayInMs: null,
      }),
    },
  });
}

async function collect<Part>(stream: AsyncIterable<Part>): Promise<Part[]> {
  const parts: Part[] = [];
  for await (const part of stream) parts.push(part);
  return parts;
}

for (const effect of ["allow", "deny", "pending"] as const) {
  test(`AI SDK streamText carries text and the governed ${effect} tool result`, async (t) => {
    const directory = await mkdtemp(join(tmpdir(), "runkite-ai-sdk-stream-"));
    t.after(() => rm(directory, { recursive: true, force: true }));
    const store = new NoteStore(join(directory, "state.json"));
    const requests: { path: string; body: unknown }[] = [];
    const client = createGovernanceClient({
      baseUrl: "http://plane",
      fetch: async (url, init) => {
        const path = new URL(String(url)).pathname;
        requests.push({
          path,
          body: init?.body ? JSON.parse(String(init.body)) : undefined,
        });
        assert.equal(
          path,
          "/governance/decide",
          "the stream must not poll or create a managed run",
        );
        return Response.json({
          effect,
          ...(effect === "pending" ? { action_id: "approval-one" } : {}),
        });
      },
    });
    const jobs: NoteJob[] = [];
    const result = streamText({
      model: model(),
      prompt: "Create the note",
      tools: {
        createNote: createNoteTool({
          client,
          store,
          turnId: "stream-turn",
          onJob: (job) => jobs.push(job),
        }),
      },
      maxRetries: 0,
    });
    // Consume the full SDK stream, including tool results, rather than textStream alone.
    const parts = await collect(result.stream);
    assert.deepEqual(
      parts
        .filter((part) => part.type === "text-delta")
        .map((part) => part.text),
      textChunks,
    );
    assert.equal(await result.text, textChunks.join(""));
    assert.equal(
      parts.filter(
        (part) => part.type === "error" || part.type === "tool-error",
      ).length,
      0,
    );
    const calls = parts.filter((part) => part.type === "tool-call");
    assert.equal(calls.length, 1);
    assert.deepEqual(calls[0].input, note);
    const outputs = parts.filter((part) => part.type === "tool-result");
    assert.equal(outputs.length, 1);
    assert.equal(jobs.length, 1);
    assert.deepEqual(outputs[0].output, jobs[0]);
    assert.deepEqual(
      (await result.toolResults).map((part) => part.output),
      jobs,
    );
    assert.deepEqual(requests, [
      {
        path: "/governance/decide",
        body: {
          agent_id: "notes-app",
          connector: "workspace",
          tool: "createNote",
          args: note,
          run_id: jobs[0].id,
        },
      },
    ]);
    const state = await store.read();
    if (effect === "allow") {
      assert.equal(jobs[0].status, "done");
      assert.deepEqual(state.notes, { [jobs[0].id]: note });
    } else {
      assert.equal(jobs[0].status, effect === "deny" ? "denied" : "pending");
      assert.deepEqual(state.notes, {});
      if (effect === "pending") {
        assert.equal(state.jobs[jobs[0].id].actionId, "approval-one");
        assert.deepEqual(state.jobs[jobs[0].id].args, note);
      }
    }
  });
}

test(
  "AI SDK streamText exposes a delayed policy failure without executing the tool",
  { timeout: 5_000 },
  async (t) => {
    const directory = await mkdtemp(join(tmpdir(), "runkite-ai-sdk-stream-"));
    t.after(() => rm(directory, { recursive: true, force: true }));
    const store = new NoteStore(join(directory, "state.json"));
    let entered!: () => void;
    const deciding = new Promise<void>((resolve) => {
      entered = resolve;
    });
    let answer!: (response: Response) => void;
    const response = new Promise<Response>((resolve) => {
      answer = resolve;
    });
    const requests: string[] = [];
    const client = createGovernanceClient({
      baseUrl: "http://plane",
      timeoutMs: 1_000,
      fetch: async (url) => {
        requests.push(new URL(String(url)).pathname);
        entered();
        return response;
      },
    });
    const result = streamText({
      model: model(),
      prompt: "Create the note",
      tools: {
        createNote: createNoteTool({ client, store, turnId: "stream-turn" }),
      },
      maxRetries: 0,
    });
    const completed = collect(result.stream);
    await deciding;
    assert.deepEqual((await store.read()).notes, {});
    assert.deepEqual((await store.read()).jobs, {});
    answer(Response.json({ message: "plane unavailable" }, { status: 503 }));
    const parts = await completed;
    const errors = parts.filter((part) => part.type === "tool-error");
    assert.equal(errors.length, 1);
    assert.match(String(errors[0].error), /plane unavailable/);
    assert.equal(parts.filter((part) => part.type === "tool-result").length, 0);
    assert.equal((await result.toolResults).length, 0);
    assert.deepEqual(requests, ["/governance/decide"]);
    assert.deepEqual((await store.read()).notes, {});
    assert.deepEqual((await store.read()).jobs, {});
  },
);

test(
  "AI SDK streamText cancellation aborts an outstanding decision without executing",
  { timeout: 5_000 },
  async (t) => {
    const directory = await mkdtemp(join(tmpdir(), "runkite-ai-sdk-stream-"));
    t.after(() => rm(directory, { recursive: true, force: true }));
    const store = new NoteStore(join(directory, "state.json"));
    const controller = new AbortController();
    let entered!: () => void;
    const deciding = new Promise<void>((resolve) => {
      entered = resolve;
    });
    let decisionAborted = false;
    const requests: string[] = [];
    const client = createGovernanceClient({
      baseUrl: "http://plane",
      timeoutMs: 1_000,
      fetch: async (url, init) =>
        new Promise<Response>((_resolve, reject) => {
          requests.push(new URL(String(url)).pathname);
          const signal = init?.signal;
          assert.ok(signal);
          const abort = () => {
            decisionAborted = true;
            reject(signal.reason);
          };
          signal.addEventListener("abort", abort, { once: true });
          if (signal.aborted) abort();
          entered();
        }),
    });
    const result = streamText({
      model: model(),
      prompt: "Create the note",
      abortSignal: controller.signal,
      tools: {
        createNote: createNoteTool({ client, store, turnId: "stream-turn" }),
      },
      maxRetries: 0,
    });
    const completed = collect(result.stream);
    await deciding;
    controller.abort(new Error("application stream cancelled"));
    const parts = await completed;
    assert.equal(decisionAborted, true);
    assert.equal(parts.filter((part) => part.type === "tool-result").length, 0);
    assert.equal(parts.filter((part) => part.type === "abort").length, 1);
    assert.deepEqual(requests, ["/governance/decide"]);
    assert.deepEqual((await store.read()).notes, {});
    assert.deepEqual((await store.read()).jobs, {});
  },
);
