import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { test, type TestContext } from "node:test";
import { fileURLToPath } from "node:url";
import { jsonSchema, tool } from "ai";
import { MockLanguageModelV4 } from "ai/test";
import { AiSdkAdapter, type AiSdkAgentDefinition, type AiSdkAgentFactory } from "./aiSdkAdapter.js";
import { CompletionDeliveryError } from "./completion.js";
import type { RunAssignment, RunEvent } from "./executeRun.js";

type ModelPart =
  Awaited<ReturnType<MockLanguageModelV4["doStream"]>>["stream"] extends ReadableStream<infer Part> ? Part : never;

const nativeUsage = {
  inputTokens: { total: 10, noCache: 10, cacheRead: 0, cacheWrite: 0 },
  outputTokens: { total: 2, text: 2, reasoning: 0 },
};

function modelStream(parts: ModelPart[], stall = false) {
  return {
    stream: new ReadableStream<ModelPart>({
      start(controller) {
        for (const part of parts) controller.enqueue(part);
        if (!stall) controller.close();
      },
    }),
  };
}

function textParts(content = "The answer"): ModelPart[] {
  return [
    { type: "stream-start", warnings: [] },
    { type: "text-start", id: "text" },
    { type: "text-delta", id: "text", delta: content },
    { type: "text-end", id: "text" },
    { type: "finish", finishReason: { unified: "stop", raw: "stop" }, usage: nativeUsage },
  ];
}

function toolParts(): ModelPart[] {
  return [
    { type: "stream-start", warnings: [] },
    { type: "tool-input-start", id: "call-note", toolName: "createNote" },
    { type: "tool-input-delta", id: "call-note", delta: '{"title":"The original note"}' },
    { type: "tool-input-end", id: "call-note" },
    { type: "tool-call", toolCallId: "call-note", toolName: "createNote", input: '{"title":"The original note"}' },
    { type: "finish", finishReason: { unified: "tool-calls", raw: "tool-calls" }, usage: nativeUsage },
  ];
}

function textDefinition(onCall?: (prompt: unknown) => void): AiSdkAgentDefinition {
  return {
    model: new MockLanguageModelV4({
      modelId: "adapter-test-model",
      doStream: async (options) => {
        onCall?.(options.prompt);
        return modelStream(textParts());
      },
    }),
    maxRetries: 0,
  };
}

class StateService {
  readonly rows = new Map<string, { body: string; version: number; framework: string }>();
  readonly calls: Array<{ method: string; url: string; headers: Headers }> = [];
  readonly order: string[] = [];
  readFailure?: number;
  writeFailure?: number;
  responseEtag?: string | null;

  seed(threadId: string, tenant: string, body: unknown, framework = "ai-sdk") {
    this.rows.set(`${tenant}:${threadId}`, {
      body: typeof body === "string" ? body : JSON.stringify(body),
      version: 1,
      framework,
    });
  }

  read(threadId: string, tenant = "default") {
    const row = this.rows.get(`${tenant}:${threadId}`);
    assert.ok(row);
    return JSON.parse(row.body);
  }

  readonly fetch: typeof fetch = async (input, init) => {
    const url = new URL(String(input));
    const method = init?.method ?? "GET";
    const headers = new Headers(init?.headers);
    const encodedThread = /^\/internal\/checkpoints\/(.+)\/adapter-state$/.exec(url.pathname)?.[1];
    assert.ok(encodedThread, `unexpected state URL ${url.pathname}`);
    const threadId = decodeURIComponent(encodedThread);
    const key = `${headers.get("X-Runkite-Tenant-Id")}:${threadId}`;
    const prior = this.rows.get(key);
    this.calls.push({ method, url: url.pathname, headers });
    if (method === "GET") {
      this.order.push("load");
      if (this.readFailure) return new Response("read unavailable", { status: this.readFailure });
      if (!prior) return new Response(null, { status: 404 });
      const responseHeaders = new Headers({ "X-Runkite-Checkpoint-Framework": prior.framework });
      const etag = this.responseEtag === undefined ? `"${prior.version}"` : this.responseEtag;
      if (etag !== null) responseHeaders.set("ETag", etag);
      return new Response(prior.body, { headers: responseHeaders });
    }
    assert.equal(method, "PUT");
    this.order.push("save attempted");
    if (this.writeFailure) return new Response("write unavailable", { status: this.writeFailure });
    if (prior) {
      if (headers.get("If-Match") !== `"${prior.version}"`) return new Response("state changed", { status: 412 });
      assert.equal(headers.get("If-None-Match"), null);
    } else {
      assert.equal(headers.get("If-None-Match"), "*");
      assert.equal(headers.get("If-Match"), null);
    }
    const body = String(init?.body);
    JSON.parse(body);
    const version = (prior?.version ?? 0) + 1;
    this.rows.set(key, { body, version, framework: headers.get("X-Runkite-Checkpoint-Framework") ?? "" });
    this.order.push("saved");
    return new Response(null, { headers: { ETag: `"${version}"` } });
  };
}

const shared = globalThis as typeof globalThis & { __runkiteAiSdkTestFactories?: Map<string, AiSdkAgentFactory> };
const factories = (shared.__runkiteAiSdkTestFactories ??= new Map());

async function fixture(t: TestContext, factory: AiSdkAgentFactory, state = new StateService()) {
  const key = randomUUID();
  const directory = await mkdtemp(path.join(path.dirname(fileURLToPath(import.meta.url)), ".ai-sdk-test-"));
  factories.set(key, factory);
  t.after(async () => {
    factories.delete(key);
    await rm(directory, { recursive: true, force: true });
  });
  await writeFile(
    path.join(directory, "agent.mjs"),
    `export const agent = (context) => globalThis.__runkiteAiSdkTestFactories.get(${JSON.stringify(key)})(context);\n`,
  );
  const configPath = path.join(directory, "runkite.json");
  await writeFile(
    configPath,
    JSON.stringify({ runner_kind: "typescript-ai-sdk", graphs: { notes: "./agent.mjs:agent" } }),
  );
  const create = async () => {
    const adapter = new AiSdkAdapter(configPath, {
      httpAddress: "http://plane",
      runnerKind: "typescript-ai-sdk",
      runnerToken: "test-runner-token",
      fetch: state.fetch,
    });
    await adapter.load();
    t.after(() => adapter.close());
    return adapter;
  };
  return { adapter: await create(), create, state, configPath, directory };
}

function assignment(overrides: Partial<RunAssignment> = {}): RunAssignment {
  return {
    run_id: "run-one",
    thread_id: "thread-one",
    graph_id: "notes",
    tenant_id: "default",
    generation: 1,
    input: { messages: [{ role: "user", content: "Create a note" }] },
    stream_modes: ["messages", "values", "custom"],
    ...overrides,
  };
}

async function bounded<T>(promise: Promise<T>): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      promise,
      new Promise<never>((_resolve, reject) => {
        timer = setTimeout(() => reject(new Error("AI SDK test execution did not settle")), 2000);
      }),
    ]);
  } finally {
    if (timer) clearTimeout(timer);
  }
}

async function execute(adapter: AiSdkAdapter, run = assignment(), isCancelled = () => false) {
  const events: RunEvent[] = [];
  const status = await bounded(
    adapter.execute(
      run,
      async (event) => {
        events.push(event);
      },
      isCancelled,
    ),
  );
  return { status, events };
}

function finalValues(events: RunEvent[]) {
  const values = events.filter((event) => event.method === "values");
  assert.equal(values.length, 1);
  return values[0].data as {
    messages: unknown[];
    usage: {
      prompt_tokens: number;
      completion_tokens: number;
      total_tokens: number;
      model?: string;
      unmetered?: boolean;
    };
  };
}

function assertNoSuccessfulCompletion(events: RunEvent[]) {
  assert.equal(
    events.some((event) => event.method === "end" && (event.data as { status?: string }).status === "success"),
    false,
  );
  assert.equal(
    events.some((event) => event.method === "custom:ai_sdk" && (event.data as { type?: string }).type === "finish"),
    false,
  );
}

function nativeToolDefinition(
  effects: string[],
  onCall?: (prompt: unknown) => void,
  executeTool?: () => Promise<unknown>,
) {
  let calls = 0;
  return {
    model: new MockLanguageModelV4({
      modelId: "adapter-test-model",
      doStream: async (options) => {
        onCall?.(options.prompt);
        return modelStream(calls++ === 0 ? toolParts() : textParts("Created the note"));
      },
    }),
    maxRetries: 0,
    tools: {
      createNote: tool({
        inputSchema: jsonSchema<{ title: string }>({
          type: "object",
          properties: { title: { type: "string" } },
          required: ["title"],
        }),
        execute: async ({ title }) => {
          effects.push(title);
          return executeTool ? executeTool() : { noteId: "note-one" };
        },
      }),
    },
  } satisfies AiSdkAgentDefinition;
}

test("AI SDK text execution persists native history before publishing values and UI finish", async (t) => {
  const instance = await fixture(t, () => textDefinition());
  const events: RunEvent[] = [];
  const status = await instance.adapter.execute(
    assignment(),
    async (event) => {
      events.push(event);
      if (
        event.method === "values" ||
        event.method === "end" ||
        (event.method === "custom:ai_sdk" && (event.data as { type: string }).type === "finish")
      ) {
        assert.ok(instance.state.order.includes("saved"), "completion must wait for state commit");
      }
    },
    () => false,
  );
  assert.equal(status, "success");
  assert.equal(
    events.some((event) => event.method === "messages"),
    true,
  );
  assert.equal(events.at(-1)?.method, "end");
  const output = finalValues(events);
  assert.equal(output.usage.prompt_tokens, 10);
  assert.equal(output.usage.completion_tokens, 2);
  assert.equal(output.usage.model, "adapter-test-model");
  const stored = instance.state.read("thread-one");
  assert.deepEqual(
    stored.messages.map((message: { role: string }) => message.role),
    ["user", "assistant"],
  );
  assert.equal(stored.completed.run_id, "run-one");
  assert.ok(instance.state.calls.find((call) => call.method === "PUT")?.headers.get("If-None-Match") === "*");
});

test("AI SDK native tool results survive a new adapter and are replayed as model history", async (t) => {
  const effects: string[] = [];
  const prompts: unknown[] = [];
  const instance = await fixture(t, (context) =>
    context.runId === "run-one"
      ? nativeToolDefinition(effects)
      : textDefinition((prompt) => {
          prompts.push(prompt);
        }),
  );
  const first = await execute(instance.adapter);
  assert.equal(first.status, "success");
  assert.deepEqual(effects, ["The original note"]);
  const history = instance.state.read("thread-one").messages;
  assert.deepEqual(
    history.map((message: { role: string }) => message.role),
    ["user", "assistant", "tool", "assistant"],
  );
  assert.equal(history[2].content[0].toolCallId, "call-note");
  const restarted = await instance.create();
  const second = await execute(
    restarted,
    assignment({ run_id: "run-two", input: { messages: [{ role: "human", content: "What did you create?" }] } }),
  );
  assert.equal(second.status, "success");
  assert.equal(prompts.length, 1);
  assert.deepEqual(
    (prompts[0] as Array<{ role: string }>).map((message) => message.role),
    ["user", "assistant", "tool", "assistant", "user"],
  );
  assert.equal(finalValues(first.events).usage.total_tokens, 24);
  assert.equal(finalValues(second.events).usage.total_tokens, 12, "prior turns must not be billed again");
  assert.deepEqual(effects, ["The original note"]);
  const write = instance.state.calls.filter((call) => call.method === "PUT").at(-1);
  assert.equal(write?.headers.get("If-Match"), '"1"');
});

test("a reclaimed run replays an already committed result without executing its tool again", async (t) => {
  const effects: string[] = [];
  let factoryCalls = 0;
  const instance = await fixture(t, () => {
    factoryCalls++;
    return nativeToolDefinition(effects);
  });
  await assert.rejects(
    instance.adapter.execute(
      assignment(),
      async (event) => {
        if (event.method === "values") throw new CompletionDeliveryError("delivery failed after state committed");
      },
      () => false,
    ),
    CompletionDeliveryError,
  );
  assert.equal(instance.state.read("thread-one").completed.run_id, "run-one");
  const restarted = await instance.create();
  const recovered = await execute(restarted, assignment({ generation: 2 }));
  assert.equal(recovered.status, "success");
  assert.deepEqual(effects, ["The original note"]);
  assert.equal(factoryCalls, 1);
  assert.equal(instance.state.calls.filter((call) => call.method === "PUT").length, 1);
  const start = recovered.events.find(
    (event) => event.method === "custom:ai_sdk" && (event.data as { type: string }).type === "start",
  );
  assert.ok(start);
  assert.equal((start.data as { messageId: string }).messageId, "run-one:g2:assistant");
  const replayedMessages = recovered.events.filter((event) => event.method === "messages");
  assert.ok(replayedMessages.length, "Agent Protocol message subscribers also receive committed text on reclaim");
  assert.ok(replayedMessages.every((event) => (event.data as Array<{ id: string }>)[0].id === "run-one:g2:assistant"));
  assert.equal(finalValues(recovered.events).usage.total_tokens, 24);
});

test("a stop condition may complete after a fully resolved tool step", async (t) => {
  const effects: string[] = [];
  let modelCalls = 0;
  const instance = await fixture(t, () => ({
    ...nativeToolDefinition(effects, () => {
      modelCalls++;
    }),
    stopWhen: () => true,
  }));
  const result = await execute(instance.adapter);
  assert.equal(result.status, "success");
  assert.deepEqual(effects, ["The original note"]);
  assert.equal(modelCalls, 1);
  const state = instance.state.read("thread-one");
  assert.deepEqual(
    state.messages.map((message: { role: string }) => message.role),
    ["user", "assistant", "tool"],
  );
  assert.equal(state.messages.at(-1).content[0].toolCallId, "call-note");
  assert.equal(finalValues(result.events).usage.total_tokens, 12);
  assert.equal(result.events.at(-1)?.method, "end");
});

test("reclaim cannot reuse committed output for replacement input", async (t) => {
  let factoryCalls = 0;
  const instance = await fixture(t, () => {
    factoryCalls++;
    return textDefinition();
  });
  await execute(instance.adapter);
  const replay = await execute(
    await instance.create(),
    assignment({ generation: 2, input: { messages: [{ role: "user", content: "Replacement operation" }] } }),
  );
  assert.equal(replay.status, "error");
  assert.equal(factoryCalls, 1);
  assertNoSuccessfulCompletion(replay.events);
  assert.equal(instance.state.calls.filter((call) => call.method === "PUT").length, 1);
});

for (const status of [412, 503]) {
  test(`AI SDK state write HTTP ${status} cannot publish successful completion`, async (t) => {
    const state = new StateService();
    state.writeFailure = status;
    const instance = await fixture(t, () => textDefinition(), state);
    const events: RunEvent[] = [];
    await assert.rejects(
      bounded(
        instance.adapter.execute(
          assignment(),
          async (event) => {
            events.push(event);
          },
          () => false,
        ),
      ),
      CompletionDeliveryError,
    );
    assertNoSuccessfulCompletion(events);
    assert.equal(
      events.some((event) => event.method === "values"),
      false,
    );
    assert.equal(state.rows.size, 0);
  });
}

test("AI SDK state read failures prevent model and tool execution", async (t) => {
  const state = new StateService();
  state.readFailure = 503;
  let factoryCalls = 0;
  const instance = await fixture(
    t,
    () => {
      factoryCalls++;
      return textDefinition();
    },
    state,
  );
  const events: RunEvent[] = [];
  await assert.rejects(
    instance.adapter.execute(
      assignment(),
      async (event) => {
        events.push(event);
      },
      () => false,
    ),
    CompletionDeliveryError,
  );
  assert.equal(factoryCalls, 0);
  assertNoSuccessfulCompletion(events);
  assert.equal(
    state.calls.some((call) => call.method === "PUT"),
    false,
  );
});

for (const value of ["{bad json", { v: 99 }, { v: 1, framework: "langgraph", graph_id: "notes", messages: [] }]) {
  test(`invalid AI SDK state is not replaced: ${JSON.stringify(value)}`, async (t) => {
    const state = new StateService();
    state.seed("thread-one", "default", value);
    let factoryCalls = 0;
    const instance = await fixture(
      t,
      () => {
        factoryCalls++;
        return textDefinition();
      },
      state,
    );
    await assert.rejects(
      instance.adapter.execute(
        assignment(),
        async () => {},
        () => false,
      ),
      CompletionDeliveryError,
    );
    assert.equal(factoryCalls, 0);
    assert.equal(
      state.calls.some((call) => call.method === "PUT"),
      false,
    );
  });
}

for (const etag of [null, 'W/"1"', "1"]) {
  test(`unsafe checkpoint ETag ${String(etag)} prevents execution`, async (t) => {
    const state = new StateService();
    state.seed("thread-one", "default", {});
    state.responseEtag = etag;
    let factoryCalls = 0;
    const instance = await fixture(
      t,
      () => {
        factoryCalls++;
        return textDefinition();
      },
      state,
    );
    await assert.rejects(
      instance.adapter.execute(
        assignment(),
        async () => {},
        () => false,
      ),
      CompletionDeliveryError,
    );
    assert.equal(factoryCalls, 0);
  });
}

test("a state blob from another framework is refused before its contents are used", async (t) => {
  const state = new StateService();
  state.seed("thread-one", "default", {}, "langgraph");
  const instance = await fixture(
    t,
    () => {
      throw new Error("factory must not run");
    },
    state,
  );
  await assert.rejects(
    instance.adapter.execute(
      assignment(),
      async () => {},
      () => false,
    ),
    CompletionDeliveryError,
  );
});

test("AI SDK cancel before execution reports interrupted without invoking a factory", async (t) => {
  let factoryCalls = 0;
  const instance = await fixture(t, () => {
    factoryCalls++;
    return textDefinition();
  });
  const result = await execute(instance.adapter, assignment(), () => true);
  assert.equal(result.status, "interrupted");
  assert.equal(factoryCalls, 0);
  assertNoSuccessfulCompletion(result.events);
  assert.equal(
    result.events.some(
      (event) => event.method === "end" && (event.data as { status: string }).status === "interrupted",
    ),
    true,
  );
});

test("AI SDK cancellation interrupts a stalled model without fabricated usage", async (t) => {
  let cancelled = false;
  let observedSignal: AbortSignal | undefined;
  const instance = await fixture(t, (context) => {
    observedSignal = context.signal;
    return {
      model: new MockLanguageModelV4({
        modelId: "adapter-test-model",
        doStream: async () => modelStream(textParts().slice(0, 3), true),
      }),
      maxRetries: 0,
    };
  });
  const events: RunEvent[] = [];
  const status = await bounded(
    instance.adapter.execute(
      assignment(),
      async (event) => {
        events.push(event);
        if (event.method === "messages") cancelled = true;
      },
      () => cancelled,
    ),
  );
  assert.equal(status, "interrupted");
  assert.equal(observedSignal?.aborted, true);
  assertNoSuccessfulCompletion(events);
  assert.equal(finalValues(events).usage.total_tokens, 0);
  assert.equal(finalValues(events).usage.unmetered, true);
});

test("cancel while a tool waits retains model-reported usage before the unfinished step", async (t) => {
  let cancelled = false;
  const effects: string[] = [];
  const instance = await fixture(t, () =>
    nativeToolDefinition(effects, undefined, async () => {
      cancelled = true;
      return new Promise(() => {});
    }),
  );
  const result = await execute(instance.adapter, assignment(), () => cancelled);
  assert.equal(result.status, "interrupted");
  assert.deepEqual(effects, ["The original note"]);
  assertNoSuccessfulCompletion(result.events);
  const usage = finalValues(result.events).usage;
  assert.equal(usage.prompt_tokens, 10);
  assert.equal(usage.completion_tokens, 2);
  assert.equal(usage.total_tokens, 12);
  assert.equal(usage.model, "adapter-test-model");
});

test("allowed_tools denies a local executor before its side effects", async (t) => {
  const effects: string[] = [];
  const instance = await fixture(t, () => nativeToolDefinition(effects));
  const result = await execute(instance.adapter, assignment({ allowed_tools: [] }));
  assert.equal(result.status, "error");
  assert.deepEqual(effects, []);
  assertNoSuccessfulCompletion(result.events);
  assert.equal(
    result.events.some((event) => event.method === "error"),
    true,
  );
});

for (const overrides of [{ checkpoint_ref: "prior-checkpoint" }, { resume_command: { response: "approved" } }]) {
  test(`unsupported native continuation fails before model work: ${JSON.stringify(overrides)}`, async (t) => {
    let factoryCalls = 0;
    const instance = await fixture(t, () => {
      factoryCalls++;
      return textDefinition();
    });
    const result = await execute(instance.adapter, assignment(overrides));
    assert.equal(result.status, "error");
    assert.equal(factoryCalls, 0);
    assertNoSuccessfulCompletion(result.events);
  });
}

for (const mode of ["approval", "streaming", "provider"] as const) {
  test(`unsupported ${mode} tools are rejected before execution`, async (t) => {
    const effects: string[] = [];
    const instance = await fixture(t, () => {
      const definition = nativeToolDefinition(effects);
      if (mode === "approval") definition.tools.createNote.needsApproval = true;
      else if (mode === "streaming")
        definition.tools.createNote.execute = async function* () {
          yield { noteId: "never" };
        };
      else
        definition.tools.createNote = {
          type: "provider",
          id: "test.provider",
          name: "createNote",
          args: {},
        } as unknown as typeof definition.tools.createNote;
      return definition;
    });
    const result = await execute(instance.adapter);
    assert.equal(result.status, "error");
    assert.deepEqual(effects, []);
    assertNoSuccessfulCompletion(result.events);
  });
}

test("provider stream error cannot become successful completion even when SDK result promises resolve", async (t) => {
  const instance = await fixture(t, () => ({
    model: new MockLanguageModelV4({
      doStream: async () =>
        modelStream([
          ...textParts().slice(0, 4),
          { type: "error", error: new Error("provider stream failed") },
          { type: "finish", finishReason: { unified: "error", raw: "error" }, usage: nativeUsage },
        ]),
    }),
    maxRetries: 0,
  }));
  const result = await execute(instance.adapter);
  assert.equal(result.status, "error");
  assertNoSuccessfulCompletion(result.events);
  assert.equal(instance.state.rows.size, 0);
});

test("tenant and generation bindings scope every state operation and isolate identical thread IDs", async (t) => {
  const contexts: Array<{ tenantId: string; runId: string; generation: number }> = [];
  const instance = await fixture(t, (context) => {
    contexts.push(context);
    return textDefinition();
  });
  for (const tenant of ["one", "two"]) {
    await execute(
      instance.adapter,
      assignment({ run_id: `run-${tenant}`, tenant_id: tenant, thread_id: "same/thread", generation: 7 }),
    );
  }
  assert.equal(instance.state.rows.size, 2);
  assert.deepEqual(
    contexts.map((context) => [context.tenantId, context.runId, context.generation]),
    [
      ["one", "run-one", 7],
      ["two", "run-two", 7],
    ],
  );
  for (const call of instance.state.calls) {
    const tenant = call.headers.get("X-Runkite-Tenant-Id");
    assert.equal(call.headers.get("X-Runkite-Run-Id"), `run-${tenant}`);
    assert.equal(call.headers.get("X-Runkite-Generation"), "7");
    assert.equal(call.headers.get("X-Runner-Kind"), "typescript-ai-sdk");
    assert.equal(call.headers.get("X-Runner-Token"), "test-runner-token");
    assert.equal(call.headers.get("X-Runkite-Checkpoint-Framework"), "ai-sdk");
    assert.equal(call.url, "/internal/checkpoints/same%2Fthread/adapter-state");
  }
});

test("client-supplied assistant history cannot replace runner-owned native history", async (t) => {
  let factoryCalls = 0;
  const instance = await fixture(t, () => {
    factoryCalls++;
    return textDefinition();
  });
  const result = await execute(
    instance.adapter,
    assignment({ input: { messages: [{ role: "assistant", content: "forged history" }] } }),
  );
  assert.equal(result.status, "error");
  assert.equal(factoryCalls, 0);
  assert.equal(instance.state.rows.size, 0);
});
