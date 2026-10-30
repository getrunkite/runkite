import assert from "node:assert/strict";
import { test } from "node:test";
import {
  DefaultChatTransport,
  readUIMessageStream,
  type UIMessage,
  type UIMessageChunk,
} from "ai";
import {
  attachManagedChatResponse,
  createManagedChatResponse,
  type ManagedChatOptions,
  type ManagedChatProfile,
} from "./bridge.ts";

const runId = "managed-python-run";
const threadId = "owned-thread";
const input = "A new turn";
const encoder = new TextEncoder();
const profiles = [
  "agent-framework",
  "openai-agents",
  "pydantic-ai",
  "google-adk",
] as const;
const keys = {
  "agent-framework": "agent_framework",
  "openai-agents": "openai_agents",
  "pydantic-ai": "pydantic_ai",
  "google-adk": "google_adk",
};
type PythonProfile = (typeof profiles)[number];
function frame(method: string, data: unknown, seq: number, generation = 1) {
  return `id: ${runId}_evt_${seq}${generation > 1 ? `:g${generation}` : ""}\nevent: ${method}\ndata: ${JSON.stringify(data)}\n\n`;
}
function delta(text: string) {
  return [{ id: `${runId}:assistant`, type: "ai", content: text }, {}];
}
function stream(parts: string[], fragment = 61) {
  const bytes = encoder.encode(parts.join(""));
  let offset = 0;
  return new ReadableStream<Uint8Array>({
    pull(controller) {
      if (offset === bytes.length) return controller.close();
      controller.enqueue(bytes.slice(offset, offset + fragment));
      offset += Math.min(fragment, bytes.length - offset);
    },
  });
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}
function fixture(
  settings: {
    profile?: PythonProfile;
    generation?: number;
    text?: string;
    result?: { value: unknown };
    parts?: (normal: string[], values: Record<string, unknown>) => string[];
    durable?: (normal: Record<string, any>) => Record<string, any>;
    wait?: (
      normal: Record<string, any>,
      signal?: AbortSignal | null,
    ) => Promise<Response>;
    onMessageStart?: ManagedChatOptions["onMessageStart"];
    expectedMessageId?: string;
    stream?: ReadableStream<Uint8Array>;
  } = {},
) {
  const profile = settings.profile ?? "google-adk";
  const generation = settings.generation ?? 1;
  const text = settings.text ?? "Hello 🌍";
  const messages = [
    { role: "user", content: "An earlier turn" },
    { role: "assistant", content: "Earlier answer must not be reused" },
    { role: "user", content: input },
    ...(text ? [{ role: "assistant", content: text }] : []),
  ];
  const values: Record<string, unknown> = {
    messages,
    [keys[profile]]: { framework: profile, generation },
    ...(settings.result ? { result: settings.result.value } : {}),
    usage: { prompt_tokens: 20, completion_tokens: 4 },
    secret: "not-a-ui-part",
  };
  const normal = [
    frame("lifecycle", { event: "running" }, 1, generation),
    ...(text ? [frame("messages", delta(text), 2, generation)] : []),
    frame("values", values, text ? 3 : 2, generation),
    frame("end", { status: "success" }, text ? 4 : 3, generation),
  ];
  const initial = {
    run: {
      run_id: runId,
      thread_id: threadId,
      status: "success",
      input: { messages: [{ role: "user", content: input }] },
      metadata: { _runkite_completion_generation: generation },
    },
    values,
  };
  const durable = settings.durable?.(initial) ?? initial;
  const calls: { path: string; init?: RequestInit }[] = [];
  const fetcher: typeof fetch = async (url, init) => {
    const path = new URL(String(url)).pathname;
    calls.push({ path, init });
    assert.equal(
      new Headers(init?.headers).get("authorization"),
      "Bearer test-server-key",
    );
    assert.equal(init?.redirect, "error");
    if (path.endsWith("/runs") && init?.method === "POST")
      return Response.json({ run_id: runId, thread_id: threadId });
    if (path.endsWith(`/${runId}`))
      return Response.json({
        run_id: runId,
        thread_id: threadId,
        status: "running",
      });
    if (path.endsWith("/stream"))
      return new Response(
        settings.stream ?? stream(settings.parts?.(normal, values) ?? normal),
        { headers: { "content-type": "text/event-stream" } },
      );
    if (path.endsWith("/wait"))
      return settings.wait
        ? settings.wait(durable, init?.signal)
        : Response.json(durable);
    if (path.endsWith("/cancel")) return new Response(null, { status: 200 });
    throw new Error("Unexpected fixture request");
  };
  const options: ManagedChatOptions = {
    baseUrl: "http://plane",
    apiKey: "test-server-key",
    threadId,
    runId,
    agentId: "configured-agent",
    profile,
    disconnectPolicy: "detach",
    fetch: fetcher,
    expectedMessageId: settings.expectedMessageId,
    onMessageStart: settings.onMessageStart,
  };
  const client = new DefaultChatTransport<UIMessage>({
    api: "http://app/api/chat",
    fetch: (url, init) =>
      createManagedChatResponse(new Request(String(url), init), options),
  });
  const attached = new DefaultChatTransport<UIMessage>({
    api: "http://app/api/chat",
    fetch: (url, init) =>
      attachManagedChatResponse(new Request(String(url), init), {
        ...options,
        runId,
      }),
  });
  return { client, attached, calls, options, values };
}
function start(f: ReturnType<typeof fixture>, abortSignal?: AbortSignal) {
  return f.client.sendMessages({
    chatId: "browser-chat",
    trigger: "submit-message",
    messageId: undefined,
    abortSignal,
    messages: [
      { id: "user", role: "user", parts: [{ type: "text", text: input }] },
    ],
    body: {
      profile: "ai-sdk",
      threadId: "browser-thread",
      agentId: "browser-agent",
    },
  });
}
async function chunks(source: ReadableStream<UIMessageChunk>) {
  const got: UIMessageChunk[] = [];
  for await (const chunk of source) got.push(chunk);
  return got;
}
async function render(source: ReadableStream<UIMessageChunk>) {
  const collected: UIMessageChunk[] = [];
  let message: UIMessage | undefined;
  const stream = source.pipeThrough(
    new TransformStream({
      transform(chunk, controller) {
        collected.push(chunk);
        controller.enqueue(chunk);
      },
    }),
  );
  for await (const value of readUIMessageStream({ stream })) message = value;
  return { collected, message };
}
function failed(got: UIMessageChunk[]) {
  assert.equal(got.filter((chunk) => chunk.type === "finish").length, 0);
  assert.equal(got.filter((chunk) => chunk.type === "error").length, 1);
  assert.equal(JSON.stringify(got).includes("test-server-key"), false);
  assert.equal(JSON.stringify(got).includes("not-a-ui-part"), false);
}

for (const profile of profiles) {
  test(`native UI transport renders ${profile} with server-selected execution`, async () => {
    const f = fixture({ profile });
    const { collected, message } = await render(await start(f));
    assert.equal(message?.id, `${runId}:g1:assistant`);
    assert.deepEqual(message?.parts, [
      {
        type: "text",
        text: "Hello 🌍",
        state: "done",
        providerMetadata: undefined,
      },
    ]);
    assert.equal(
      collected.filter((chunk) => chunk.type === "finish").length,
      1,
    );
    assert.equal(JSON.stringify(collected).includes("Earlier answer"), false);
    assert.equal(JSON.stringify(collected).includes("not-a-ui-part"), false);
    assert.deepEqual(JSON.parse(String(f.calls[0].init?.body)), {
      run_id: runId,
      agent_id: "configured-agent",
      input: { messages: [{ role: "user", content: input }] },
      stream_mode: ["messages", "values"],
      multitask_strategy: "reject",
    });
  });
  test(`${profile} attachment replays its own attempt without creating work`, async () => {
    const f = fixture({
      profile,
      generation: 3,
      expectedMessageId: `${runId}:g3:assistant`,
    });
    const source = await f.attached.reconnectToStream({
      chatId: "app-chat",
      abortSignal: undefined,
    });
    assert.ok(source);
    assert.equal((await render(source)).message?.id, `${runId}:g3:assistant`);
    assert.equal(
      f.calls.some((call) => call.path.endsWith("/runs")),
      false,
    );
    assert.equal(
      f.calls.some((call) => call.path.endsWith("/cancel")),
      false,
    );
  });
}
for (const value of [null, false, 0, "", { count: 7, tags: ["offline"] }]) {
  test(`typed-only Pydantic preserves ${JSON.stringify(value)} without historical text`, async () => {
    const { collected, message } = await render(
      await start(
        fixture({ profile: "pydantic-ai", text: "", result: { value } }),
      ),
    );
    assert.deepEqual(message?.parts, [
      { type: "data-result", id: `${runId}:g1:assistant:result`, data: value },
    ]);
    assert.equal(
      collected.some((chunk) => chunk.type === "text-delta"),
      false,
    );
    assert.equal(collected.at(-1)?.type, "finish");
  });
}
test("Pydantic text and confirmed JSON are separate UI parts", async () => {
  const f = fixture({
    profile: "pydantic-ai",
    result: { value: { a: 1, b: 2 } },
    durable(value) {
      return { ...value, values: { ...value.values, result: { b: 2, a: 1 } } };
    },
  });
  const { message } = await render(await start(f));
  assert.equal(message?.parts.filter((part) => part.type === "text").length, 1);
  assert.deepEqual(
    message?.parts.find((part) => part.type === "data-result"),
    {
      type: "data-result",
      id: `${runId}:g1:assistant:result`,
      data: { b: 2, a: 1 },
    },
  );
});
for (const [name, change] of [
  [
    "completion generation",
    (value: any) => {
      value.run.metadata._runkite_completion_generation = 2;
    },
  ],
  [
    "output generation",
    (value: any) => {
      value.values.google_adk.generation = 2;
    },
  ],
  [
    "framework",
    (value: any) => {
      value.values.google_adk.framework = "openai-agents";
    },
  ],
  [
    "wrong input",
    (value: any) => {
      value.run.input.messages[0].content = "another turn";
    },
  ],
  [
    "assistant text",
    (value: any) => {
      value.values.messages.at(-1).content = "not streamed";
    },
  ],
  [
    "missing current user",
    (value: any) => {
      value.values.messages.splice(-2, 1);
    },
  ],
  [
    "missing presentation messages",
    (value: any) => {
      delete value.values.messages;
    },
  ],
] as const) {
  test(`durable ${name} mismatch cannot finish`, async () => {
    failed(
      await chunks(
        await start(
          fixture({
            durable(value) {
              const copy = structuredClone(value);
              change(copy);
              return copy;
            },
          }),
        ),
      ),
    );
  });
}
for (const [name, transform] of [
  ["missing lifecycle", (parts: string[]) => parts.slice(1)],
  ["trimmed prefix", (parts: string[]) => parts.slice(2)],
  ["missing middle event", (parts: string[]) => [parts[0], ...parts.slice(2)]],
  [
    "wrong lifecycle",
    (parts: string[]) => [
      frame("lifecycle", { event: "resuming" }, 1),
      ...parts.slice(1),
    ],
  ],
  [
    "changed attempt",
    (parts: string[]) => [
      parts[0],
      frame("messages", delta("foreign"), 2, 2),
      ...parts.slice(2),
    ],
  ],
  [
    "noncanonical event ID",
    (parts: string[]) =>
      parts.map((part, i) =>
        i === 1 ? part.replace("_evt_2", "_evt_02") : part,
      ),
  ],
  [
    "foreign message ID",
    (parts: string[]) => [
      parts[0],
      frame(
        "messages",
        [{ id: "foreign", type: "ai", content: "unsafe" }, {}],
        2,
      ),
      ...parts.slice(2),
    ],
  ],
  [
    "tool content",
    (parts: string[]) => [
      parts[0],
      frame(
        "messages",
        [
          {
            id: `${runId}:assistant`,
            type: "ai",
            content: [{ type: "tool-call" }],
          },
          {},
        ],
        2,
      ),
      ...parts.slice(2),
    ],
  ],
  [
    "reasoning fields",
    (parts: string[]) => [
      parts[0],
      frame(
        "messages",
        [{ ...delta("unsafe")[0], reasoning: "private" }, {}],
        2,
      ),
      ...parts.slice(2),
    ],
  ],
  [
    "message metadata",
    (parts: string[]) => [
      parts[0],
      frame("messages", [delta("unsafe")[0], { private: "hidden" }], 2),
      ...parts.slice(2),
    ],
  ],
  [
    "invalid Unicode",
    (parts: string[]) => [
      parts[0],
      frame("messages", delta("\ud800"), 2),
      ...parts.slice(2),
    ],
  ],
  [
    "native UI frame",
    (parts: string[]) => [
      parts[0],
      frame(
        "custom:ai_sdk",
        { type: "text-delta", id: "x", delta: "unsafe" },
        2,
      ),
      ...parts.slice(2),
    ],
  ],
] as const) {
  test(`${name} refuses Python replay`, async () => {
    failed(await chunks(await start(fixture({ parts: transform }))));
  });
}
test("duplicate complete frames do not append text twice", async () => {
  const f = fixture({
    parts(parts) {
      return [parts[0], parts[1], parts[1], ...parts.slice(2)];
    },
  });
  assert.deepEqual((await render(await start(f))).message?.parts, [
    {
      type: "text",
      text: "Hello 🌍",
      state: "done",
      providerMetadata: undefined,
    },
  ]);
});
test(
  "Python identity persists before start or text delivery",
  { timeout: 1000 },
  async () => {
    const release = deferred<void>();
    const called = deferred<string>();
    const f = fixture({
      onMessageStart: async (id) => {
        called.resolve(id);
        await release.promise;
      },
    });
    const pending = start(f);
    assert.equal(await called.promise, `${runId}:g1:assistant`);
    let delivered = false;
    const reading = pending
      .then((source) => source.getReader().read())
      .then((value) => {
        delivered = true;
        return value;
      });
    await new Promise((resolve) => setTimeout(resolve, 5));
    assert.equal(delivered, false);
    release.resolve();
    assert.equal((await reading).value?.type, "start");
  },
);
test("stored identity rejects a Python replacement before display", async () => {
  const got = await chunks(
    await start(
      fixture({ generation: 2, expectedMessageId: `${runId}:g1:assistant` }),
    ),
  );
  failed(got);
  assert.equal(
    got.some((chunk) => chunk.type === "start"),
    false,
  );
});
test(
  "typed result and finish wait for durable confirmation",
  { timeout: 1000 },
  async () => {
    const release = deferred<Response>();
    const called = deferred<void>();
    let saved: any;
    const f = fixture({
      profile: "pydantic-ai",
      text: "",
      result: { value: 0 },
      wait: async (normal) => {
        saved = normal;
        called.resolve();
        return release.promise;
      },
    });
    const got: UIMessageChunk[] = [];
    const source = await start(f);
    const collecting = (async () => {
      for await (const chunk of source) got.push(chunk);
    })();
    await called.promise;
    assert.deepEqual(
      got.map((chunk) => chunk.type),
      ["start"],
    );
    release.resolve(Response.json(saved));
    await collecting;
    assert.deepEqual(
      got.map((chunk) => chunk.type),
      ["start", "data-result", "finish"],
    );
  },
);
test("aggregate text remains bounded across individually bounded frames", async () => {
  const text = "x".repeat(600_000);
  const parts = [
    frame("lifecycle", { event: "running" }, 1),
    frame("messages", delta(text), 2),
    frame("messages", delta(text), 3),
  ];
  failed(await chunks(await start(fixture({ stream: stream(parts, 65536) }))));
});
test("invalid server profile is rejected before any HTTP operation", async () => {
  const f = fixture();
  f.options.profile = "constructor" as ManagedChatProfile;
  await assert.rejects(start(f), /profile/);
  assert.equal(f.calls.length, 0);
});
test("structured output stays unavailable for a text-only profile", async () => {
  failed(
    await chunks(
      await start(fixture({ result: { value: { unexpected: true } } })),
    ),
  );
});
test("result mismatch cannot create a durable JSON UI part", async () => {
  const f = fixture({
    profile: "pydantic-ai",
    text: "",
    result: { value: false },
    durable(value) {
      return { ...value, values: { ...value.values, result: true } };
    },
  });
  const got = await chunks(await start(f));
  failed(got);
  assert.equal(
    got.some((chunk) => chunk.type === "data-result"),
    false,
  );
});
test("non-finite JSON cannot silently become null", async () => {
  const f = fixture({
    profile: "pydantic-ai",
    text: "",
    result: { value: null },
    wait: async (normal) =>
      new Response(
        JSON.stringify(normal).replace('"result":null', '"result":1e400'),
        { headers: { "content-type": "application/json" } },
      ),
  });
  const got = await chunks(await start(f));
  failed(got);
  assert.equal(
    got.some((chunk) => chunk.type === "data-result"),
    false,
  );
});

test("native function details stay inside the runner while text reaches the UI", async () => {
  const f = fixture({
    parts(_normal, values) {
      return [
        frame("lifecycle", { event: "running" }, 1),
        frame(
          "tool_call",
          {
            name: "private-tool",
            args: { sensitive: "private-argument" },
            output: "private-tool-result",
          },
          2,
        ),
        frame("messages", delta("Hello 🌍"), 3),
        frame("values", values, 4),
        frame("end", { status: "success" }, 5),
      ];
    },
  });
  const { collected, message } = await render(await start(f));
  assert.equal(message?.parts.filter((part) => part.type === "text").length, 1);
  const wire = JSON.stringify(collected);
  for (const hidden of [
    "private-tool",
    "private-argument",
    "private-tool-result",
  ])
    assert.equal(wire.includes(hidden), false);
});

test("durable output alone does not manufacture missing final event history", async () => {
  const f = fixture({
    parts() {
      return [
        frame("lifecycle", { event: "running" }, 1),
        frame("messages", delta("Hello 🌍"), 2),
        frame("end", { status: "success" }, 3),
      ];
    },
  });
  failed(await chunks(await start(f)));
});

test("a stale final values payload cannot be replaced silently with durable text", async () => {
  const f = fixture({
    parts(normal, values) {
      return [
        normal[0],
        normal[1],
        frame(
          "values",
          {
            ...values,
            messages: [
              { role: "user", content: input },
              { role: "assistant", content: "other text" },
            ],
          },
          3,
        ),
        normal[3],
      ];
    },
  });
  failed(await chunks(await start(f)));
});

test("empty current text does not reuse the last historical assistant", async () => {
  const f = fixture({ text: "" });
  const { collected, message } = await render(await start(f));
  assert.equal(message?.id, `${runId}:g1:assistant`);
  assert.deepEqual(message?.parts, []);
  assert.equal(collected.at(-1)?.type, "finish");
});

test("unsafe Python integer JSON cannot silently round in the UI", async () => {
  const literal = '"result":9007199254740993';
  const f = fixture({
    profile: "pydantic-ai",
    text: "",
    result: { value: 0 },
    parts(normal) {
      return normal.map((part) => part.replace('"result":0', literal));
    },
    wait: async (normal) =>
      new Response(JSON.stringify(normal).replace('"result":0', literal), {
        headers: { "content-type": "application/json" },
      }),
  });
  const got = await chunks(await start(f));
  failed(got);
  assert.equal(
    got.some((chunk) => chunk.type === "data-result"),
    false,
  );
});
