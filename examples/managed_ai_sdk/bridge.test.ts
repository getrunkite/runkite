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
  cancelManagedChatRun,
  createManagedChatResponse,
  type ManagedChatDisconnectPolicy,
  type ManagedChatOptions,
} from "./bridge.ts";

const threadId = "server-owned-thread";
const runId = "server-owned-run";
const user: UIMessage = {
  id: "user-one",
  role: "user",
  parts: [{ type: "text", text: "Create a note" }],
};
const encoder = new TextEncoder();

function frame(event: string, data: unknown, id = ""): string {
  return `${id ? `id: ${id}\n` : ""}event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
}

function native(chunk: UIMessageChunk, seq: number, generation = 1): string {
  return frame(
    "custom:ai_sdk",
    chunk,
    `${runId}_evt_${seq}${generation > 1 ? `:g${generation}` : ""}`,
  );
}

function normalFrames(generation = 1): string[] {
  return [
    native(
      { type: "start", messageId: `${runId}:g${generation}:assistant` },
      1,
      generation,
    ),
    native({ type: "start-step" }, 2, generation),
    native({ type: "text-start", id: "text-one" }, 3, generation),
    native(
      { type: "text-delta", id: "text-one", delta: "Hello " },
      4,
      generation,
    ),
    native(
      { type: "text-delta", id: "text-one", delta: "world" },
      5,
      generation,
    ),
    native({ type: "text-end", id: "text-one" }, 6, generation),
    native(
      {
        type: "tool-input-available",
        toolCallId: "call-one",
        toolName: "createNote",
        input: { title: "A draft" },
      },
      7,
      generation,
    ),
    native(
      {
        type: "tool-output-available",
        toolCallId: "call-one",
        output: { note_id: "note-one" },
      },
      8,
      generation,
    ),
    native({ type: "finish-step" }, 9, generation),
    native({ type: "finish", finishReason: "stop" }, 10, generation),
    frame(
      "end",
      { status: "success" },
      `${runId}_evt_11${generation > 1 ? `:g${generation}` : ""}`,
    ),
  ];
}

function stream(
  parts: string[],
  fragmentBytes?: number,
): ReadableStream<Uint8Array> {
  const bytes = encoder.encode(parts.join(""));
  let offset = 0;
  return new ReadableStream({
    pull(output) {
      if (offset === bytes.length) {
        output.close();
        return;
      }
      const end = Math.min(
        bytes.length,
        offset + (fragmentBytes ?? bytes.length),
      );
      output.enqueue(bytes.slice(offset, end));
      offset = end;
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
    frames?: string[];
    fragmentBytes?: number;
    stream?: ReadableStream<Uint8Array>;
    streamResponse?: (signal?: AbortSignal | null) => Promise<Response>;
    wait?: (signal?: AbortSignal | null) => Promise<Response>;
    create?: (signal?: AbortSignal | null) => Promise<Response>;
    lookup?: (signal?: AbortSignal | null) => Promise<Response>;
    disconnectPolicy?: ManagedChatDisconnectPolicy;
    expectedMessageId?: string;
    onMessageStart?: (
      messageId: string,
      signal: AbortSignal,
    ) => void | Promise<void>;
    cancelStatus?: number;
  } = {},
) {
  const calls: { path: string; init?: RequestInit }[] = [];
  const cancelled = deferred<void>();
  const result = (status = "success", generation = 1) => ({
    run: { run_id: runId, thread_id: threadId, status },
    values: {
      ai_sdk: { generation, message_id: `${runId}:g${generation}:assistant` },
    },
  });
  const fetcher: typeof fetch = async (url, init) => {
    const path = new URL(String(url)).pathname;
    calls.push({ path, init });
    assert.equal(
      new Headers(init?.headers).get("authorization"),
      "Bearer server-secret",
    );
    assert.equal(init?.redirect, "error");
    if (path.endsWith("/runs") && init?.method === "POST") {
      return settings.create
        ? settings.create(init?.signal)
        : Response.json({ run_id: runId, thread_id: threadId });
    }
    if (path === `/threads/${threadId}/runs/${runId}`)
      return settings.lookup
        ? settings.lookup(init?.signal)
        : Response.json({
            run_id: runId,
            thread_id: threadId,
            status: "running",
          });
    if (path.endsWith("/stream")) {
      if (settings.streamResponse) return settings.streamResponse(init?.signal);
      return new Response(
        settings.stream ??
          stream(settings.frames ?? normalFrames(), settings.fragmentBytes),
        {
          headers: { "Content-Type": "text/event-stream" },
        },
      );
    }
    if (path.endsWith("/wait"))
      return settings.wait
        ? settings.wait(init?.signal)
        : Response.json(result());
    if (path.endsWith("/cancel")) {
      cancelled.resolve();
      return new Response(null, { status: settings.cancelStatus ?? 200 });
    }
    throw new Error(`Unexpected fixture request: ${path}`);
  };
  const options: ManagedChatOptions = {
    baseUrl: "http://plane",
    apiKey: "server-secret",
    threadId,
    agentId: "server-agent",
    runId,
    fetch: fetcher,
    disconnectPolicy: settings.disconnectPolicy,
    expectedMessageId: settings.expectedMessageId,
    onMessageStart: settings.onMessageStart,
  };
  const client = new DefaultChatTransport({
    api: "http://app/api/chat",
    fetch: async (url, init) =>
      createManagedChatResponse(new Request(String(url), init), options),
  });
  const attachClient = new DefaultChatTransport({
    api: "http://app/api/chat",
    fetch: async (url, init) =>
      attachManagedChatResponse(new Request(String(url), init), {
        ...options,
        runId,
      }),
  });
  return {
    calls,
    options,
    client,
    attachClient,
    result,
    cancelled: cancelled.promise,
  };
}

async function attached(
  f: ReturnType<typeof fixture>,
  abortSignal?: AbortSignal,
) {
  const source = await f.attachClient.reconnectToStream({
    chatId: "application-chat",
    abortSignal,
  });
  assert.ok(source);
  return source;
}

async function within<T>(promise: Promise<T>): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      promise,
      new Promise<never>((_resolve, reject) => {
        timer = setTimeout(
          () => reject(new Error("test operation did not settle")),
          500,
        );
      }),
    ]);
  } finally {
    if (timer) clearTimeout(timer);
  }
}

function blockedUpstream() {
  const stopped = deferred<void>();
  const body = new ReadableStream<Uint8Array>({
    start(output) {
      output.enqueue(
        encoder.encode(
          native({ type: "start", messageId: `${runId}:g1:assistant` }, 1),
        ),
      );
    },
    cancel() {
      stopped.resolve();
    },
  });
  return { body, stopped: stopped.promise };
}

function waitForAbort(signal?: AbortSignal | null): Promise<Response> {
  assert.ok(signal);
  return new Promise((_resolve, reject) => {
    const abort = () => reject(signal.reason);
    signal.addEventListener("abort", abort, { once: true });
    if (signal.aborted) abort();
  });
}

async function start(
  client: DefaultChatTransport<UIMessage>,
  abortSignal?: AbortSignal,
) {
  return client.sendMessages({
    chatId: "browser-chat-id",
    trigger: "submit-message",
    messageId: undefined,
    messages: [user],
    abortSignal,
    body: {
      thread_id: "browser-cannot-select-thread",
      agent_id: "browser-cannot-select-agent",
    },
  });
}

async function collect(
  source: ReadableStream<UIMessageChunk>,
): Promise<UIMessageChunk[]> {
  const result: UIMessageChunk[] = [];
  for await (const chunk of source) result.push(chunk);
  return result;
}

test("native AI SDK transport and UI consumer render managed text and tool results", async () => {
  const f = fixture();
  const chunks: UIMessageChunk[] = [];
  const rendered = (await start(f.client)).pipeThrough(
    new TransformStream({
      transform(chunk, output) {
        chunks.push(chunk);
        output.enqueue(chunk);
      },
    }),
  );
  let final: UIMessage | undefined;
  for await (const message of readUIMessageStream({ stream: rendered }))
    final = message;
  assert.equal(final?.id, `${runId}:g1:assistant`);
  assert.deepEqual(
    final?.parts
      .filter((part) => part.type === "text")
      .map((part) => part.text),
    ["Hello world"],
  );
  const tool = final?.parts.find((part) => part.type === "tool-createNote");
  assert.ok(tool && "state" in tool && tool.state === "output-available");
  assert.deepEqual(tool.output, { note_id: "note-one" });
  assert.equal(chunks.filter((chunk) => chunk.type === "finish").length, 1);
  assert.deepEqual(
    f.calls.map((call) => call.path),
    [
      `/threads/${threadId}/runs`,
      `/threads/${threadId}/runs/${runId}/stream`,
      `/threads/${threadId}/runs/${runId}/wait`,
    ],
  );
  assert.deepEqual(JSON.parse(String(f.calls[0].init?.body)), {
    run_id: runId,
    agent_id: "server-agent",
    input: { messages: [{ role: "user", content: "Create a note" }] },
    stream_mode: ["custom:ai_sdk", "values"],
    multitask_strategy: "reject",
  });
});

test(
  "UI finish waits for the durable run and only the new user turn enters the plane",
  { timeout: 3_000 },
  async () => {
    const pending = deferred<Response>();
    const waiting = deferred<void>();
    const f = fixture({
      wait: async () => {
        waiting.resolve();
        return pending.promise;
      },
    });
    const chunks: UIMessageChunk[] = [];
    const source = await f.client.sendMessages({
      chatId: "browser-chat-id",
      trigger: "submit-message",
      messageId: undefined,
      abortSignal: undefined,
      messages: [
        {
          id: "past",
          role: "assistant",
          parts: [{ type: "text", text: "untrusted browser history" }],
        },
        user,
      ],
    });
    const completed = (async () => {
      for await (const chunk of source) chunks.push(chunk);
    })();
    await waiting.promise;
    assert.equal(
      chunks.some((chunk) => chunk.type === "finish"),
      false,
    );
    assert.equal(
      JSON.parse(String(f.calls[0].init?.body)).input.messages.length,
      1,
    );
    pending.resolve(Response.json(f.result()));
    await completed;
    assert.equal(chunks.filter((chunk) => chunk.type === "finish").length, 1);
  },
);

test("SSE replay duplicates are removed without duplicating text", async () => {
  const parts = normalFrames();
  parts.splice(4, 0, parts[3]);
  const f = fixture({ frames: parts });
  const chunks = await collect(await start(f.client));
  assert.equal(
    chunks
      .filter((chunk) => chunk.type === "text-delta")
      .map((chunk) => chunk.delta)
      .join(""),
    "Hello world",
  );
  assert.equal(chunks.filter((chunk) => chunk.type === "finish").length, 1);
});

test("fragmented CRLF SSE preserves multibyte text and native schema validation", async () => {
  const parts = normalFrames();
  parts[3] = native(
    { type: "text-delta", id: "text-one", delta: "Hello é " },
    4,
  );
  const f = fixture({
    frames: parts.map((part) => part.replaceAll("\n", "\r\n")),
    fragmentBytes: 1,
  });
  const chunks = await collect(await start(f.client));
  assert.equal(
    chunks
      .filter((chunk) => chunk.type === "text-delta")
      .map((chunk) => chunk.delta)
      .join(""),
    "Hello é world",
  );
  assert.equal(chunks.at(-1)?.type, "finish");
});

test("a stale lower attempt cannot add text to the current UI attempt", async () => {
  const parts = normalFrames(2);
  parts.splice(
    5,
    0,
    native({ type: "text-delta", id: "text-one", delta: "superseded" }, 40),
  );
  const f = fixture({
    frames: parts,
    wait: async () => Response.json(f.result("success", 2)),
  });
  const chunks = await collect(await start(f.client));
  assert.equal(
    chunks
      .filter((chunk) => chunk.type === "text-delta")
      .map((chunk) => chunk.delta)
      .join(""),
    "Hello world",
  );
  assert.equal(chunks.at(-1)?.type, "finish");
});

test("a changed attempt is explicit delivery failure rather than mixed assistant output", async () => {
  const parts = normalFrames();
  parts.splice(
    5,
    0,
    native({ type: "start", messageId: `${runId}:g2:assistant` }, 1, 2),
  );
  const f = fixture({ frames: parts });
  const chunks = await collect(await start(f.client));
  assert.equal(chunks.filter((chunk) => chunk.type === "error").length, 1);
  assert.equal(
    chunks.some((chunk) => chunk.type === "finish"),
    false,
  );
  assert.equal(
    f.calls.filter((call) => call.path.endsWith("/cancel")).length,
    1,
  );
});

for (const status of ["error", "timeout", "interrupted"]) {
  test(`durable ${status} overrides an advisory successful stream`, async () => {
    const f = fixture({ wait: async () => Response.json(f.result(status)) });
    const chunks = await collect(await start(f.client));
    assert.equal(
      chunks.some((chunk) => chunk.type === "finish"),
      false,
    );
    assert.equal(
      chunks.at(-1)?.type,
      status === "interrupted" ? "abort" : "error",
    );
  });
}

test("a durable result from another generation never confirms an old UI finish", async () => {
  const f = fixture({
    wait: async () => Response.json(f.result("success", 2)),
  });
  const chunks = await collect(await start(f.client));
  assert.equal(chunks.at(-1)?.type, "error");
  assert.equal(
    chunks.some((chunk) => chunk.type === "finish"),
    false,
  );
  assert.equal(
    f.calls.some((call) => call.path.endsWith("/cancel")),
    false,
    "already completed run is not cancelled",
  );
});

test("an unconfirmed stream closes with an error and cancels the managed run", async () => {
  const f = fixture({ frames: normalFrames().slice(0, -1) });
  const chunks = await collect(await start(f.client));
  assert.equal(chunks.at(-1)?.type, "error");
  assert.equal(
    chunks.some((chunk) => chunk.type === "finish"),
    false,
  );
  assert.equal(
    f.calls.filter((call) => call.path.endsWith("/cancel")).length,
    1,
  );
});

test("invalid native UI chunks fail closed without exposing upstream payloads", async () => {
  const f = fixture({
    frames: [
      frame(
        "custom:ai_sdk",
        {
          type: "text-delta",
          id: "one",
          delta: { secret: "never-an-error-message" },
        },
        "evt_1",
      ),
    ],
  });
  const chunks = await collect(await start(f.client));
  assert.equal(chunks.at(-1)?.type, "error");
  assert.doesNotMatch(
    JSON.stringify(chunks),
    /secret|server-secret|never-an-error/,
  );
  assert.equal(
    f.calls.filter((call) => call.path.endsWith("/cancel")).length,
    1,
  );
});

test(
  "request cancellation explicitly cancels the run, including while stream read is blocked",
  { timeout: 3_000 },
  async () => {
    const active = deferred<void>();
    const blocked = new ReadableStream<Uint8Array>({
      start(output) {
        output.enqueue(
          encoder.encode(
            native({ type: "start", messageId: `${runId}:g1:assistant` }, 1),
          ),
        );
        active.resolve();
      },
    });
    const f = fixture({ stream: blocked });
    const abort = new AbortController();
    const source = await start(f.client, abort.signal);
    const completed = collect(source);
    await active.promise;
    abort.abort(new Error("application request stopped"));
    const chunks = await completed;
    assert.equal(chunks.at(-1)?.type, "abort");
    assert.equal(
      f.calls.filter((call) => call.path.endsWith("/cancel")).length,
      1,
    );
  },
);

test(
  "cancelling the returned response body cancels the run once",
  { timeout: 3_000 },
  async () => {
    const blocked = new ReadableStream<Uint8Array>({
      start(output) {
        output.enqueue(
          encoder.encode(
            native({ type: "start", messageId: `${runId}:g1:assistant` }, 1),
          ),
        );
      },
    });
    const f = fixture({ stream: blocked });
    const response = await createManagedChatResponse(
      new Request("http://app/api/chat", {
        method: "POST",
        body: JSON.stringify({ messages: [user] }),
      }),
      f.options,
    );
    assert.equal(response.headers.get("x-runkite-run-id"), runId);
    assert.equal(response.headers.get("x-vercel-ai-ui-message-stream"), "v1");
    assert.doesNotMatch(JSON.stringify([...response.headers]), /server-secret/);
    const reader = response.body!.getReader();
    await reader.read();
    await reader.cancel();
    await f.cancelled;
    assert.equal(
      f.calls.filter((call) => call.path.endsWith("/cancel")).length,
      1,
    );
  },
);

test(
  "abort during bounded run creation cancels after creation is confirmed",
  { timeout: 3_000 },
  async () => {
    const entered = deferred<void>();
    const pending = deferred<Response>();
    const f = fixture({
      create: async () => {
        entered.resolve();
        return pending.promise;
      },
    });
    const abort = new AbortController();
    const opening = start(f.client, abort.signal);
    await entered.promise;
    abort.abort(new Error("stopped before creation response"));
    pending.resolve(Response.json({ run_id: runId, thread_id: threadId }));
    await assert.rejects(opening, /stopped before creation response/);
    assert.equal(
      f.calls.filter((call) => call.path.endsWith("/cancel")).length,
      1,
    );
    assert.equal(
      f.calls.some((call) => call.path.endsWith("/stream")),
      false,
    );
  },
);

test("unsupported browser input is rejected before any plane request", async () => {
  for (const input of [
    { messages: [{ ...user, role: "assistant" }] },
    {
      messages: [
        { ...user, parts: [{ type: "file", url: "https://untrusted" }] },
      ],
    },
    { messages: [user], trigger: "regenerate-message" },
    {
      messages: [
        { ...user, parts: [{ type: "text", text: "x".repeat(65_536) }] },
      ],
    },
  ]) {
    const f = fixture();
    await assert.rejects(
      createManagedChatResponse(
        new Request("http://app/api/chat", {
          method: "POST",
          body: JSON.stringify(input),
        }),
        f.options,
      ),
    );
    assert.deepEqual(f.calls, []);
  }
});

test("pre-aborted request performs no model run or cancellation", async () => {
  const f = fixture();
  const abort = new AbortController();
  abort.abort(new Error("already stopped"));
  await assert.rejects(start(f.client, abort.signal), /already stopped/);
  assert.deepEqual(f.calls, []);
});

test("failed durable completion confirmation never becomes a UI finish", async () => {
  const f = fixture({
    wait: async () =>
      Response.json({ error: "private upstream detail" }, { status: 503 }),
  });
  const chunks = await collect(await start(f.client));
  assert.equal(chunks.at(-1)?.type, "error");
  assert.equal(
    chunks.some((chunk) => chunk.type === "finish"),
    false,
  );
  assert.doesNotMatch(JSON.stringify(chunks), /private upstream detail/);
  assert.equal(
    f.calls.filter((call) => call.path.endsWith("/cancel")).length,
    1,
  );
});

test("a lost create response attempts cancellation using the preallocated run ID", async () => {
  const f = fixture({
    create: async () => {
      throw new Error("creation response lost");
    },
  });
  await assert.rejects(start(f.client), /creation response lost/);
  assert.equal(
    f.calls.filter((call) => call.path.endsWith("/cancel")).length,
    1,
  );
  assert.equal(
    f.calls.some((call) => call.path.endsWith("/stream")),
    false,
  );
});

test(
  "an unconfirmed cancellation reaches the application's recovery callback",
  { timeout: 3_000 },
  async () => {
    const blocked = new ReadableStream<Uint8Array>({
      start(output) {
        output.enqueue(
          encoder.encode(
            native({ type: "start", messageId: `${runId}:g1:assistant` }, 1),
          ),
        );
      },
    });
    const f = fixture({ stream: blocked, cancelStatus: 503 });
    let unconfirmed = 0;
    f.options.onCancelError = () => {
      unconfirmed++;
    };
    const abort = new AbortController();
    const completed = collect(await start(f.client, abort.signal));
    abort.abort(new Error("stopped"));
    const chunks = await completed;
    assert.equal(chunks.at(-1)?.type, "abort");
    assert.equal(unconfirmed, 1);
    assert.equal(
      f.calls.filter((call) => call.path.endsWith("/cancel")).length,
      1,
    );
  },
);

test("malformed Unicode fails before a managed run is created", async () => {
  const f = fixture();
  const malformed = {
    ...user,
    parts: [{ type: "text", text: String.fromCharCode(0xd800) }],
  };
  await assert.rejects(
    createManagedChatResponse(
      new Request("http://app/api/chat", {
        method: "POST",
        body: JSON.stringify({ messages: [malformed] }),
      }),
      f.options,
    ),
    /invalid Unicode/,
  );
  await assert.rejects(
    createManagedChatResponse(
      new Request("http://app/api/chat", {
        method: "POST",
        body: new Uint8Array([0xff]),
      }),
      f.options,
    ),
  );
  assert.deepEqual(f.calls, []);
});

test(
  "stream header timeout cancels the already-created managed run",
  { timeout: 3_000 },
  async () => {
    const f = fixture({
      streamResponse: async (signal) =>
        new Promise((_resolve, reject) => {
          signal!.addEventListener("abort", () => reject(signal!.reason), {
            once: true,
          });
        }),
    });
    f.options.timeoutMs = 5;
    await assert.rejects(start(f.client), /stream setup timed out/);
    assert.equal(
      f.calls.filter((call) => call.path.endsWith("/cancel")).length,
      1,
    );
  },
);

test("attach reads the existing run and its durable result without dispatch or cancellation", async () => {
  const f = fixture();
  const chunks = await collect(await attached(f));
  assert.equal(chunks.at(-1)?.type, "finish");
  assert.deepEqual(
    f.calls.map((call) => [call.path, call.init?.method ?? "GET"]),
    [
      [`/threads/${threadId}/runs/${runId}`, "GET"],
      [`/threads/${threadId}/runs/${runId}/stream`, "GET"],
      [`/threads/${threadId}/runs/${runId}/wait`, "GET"],
    ],
  );
});

test("aborting an attach request closes its upstream reader without canceling the run", async () => {
  const upstream = blockedUpstream();
  const f = fixture({ stream: upstream.body });
  const abort = new AbortController();
  const reader = (await attached(f, abort.signal)).getReader();
  assert.equal((await reader.read()).value?.type, "start");
  const pending = reader.read();
  abort.abort(new Error("browser disconnected"));
  assert.equal((await within(pending)).value?.type, "abort");
  assert.equal((await reader.read()).done, true);
  await within(upstream.stopped);
  assert.ok(f.calls.every((call) => (call.init?.method ?? "GET") === "GET"));
});

test("canceling an attached response body releases its upstream reader without canceling the run", async () => {
  const upstream = blockedUpstream();
  const f = fixture({ stream: upstream.body });
  const reader = (await attached(f)).getReader();
  assert.equal((await reader.read()).value?.type, "start");
  await within(reader.cancel());
  await within(upstream.stopped);
  assert.ok(f.calls.every((call) => (call.init?.method ?? "GET") === "GET"));
});

test("detached creation requires a previously allocated server-owned run ID", async () => {
  const f = fixture({ disconnectPolicy: "detach" });
  await assert.rejects(
    createManagedChatResponse(
      new Request("http://app/api/chat", {
        method: "POST",
        body: JSON.stringify({ messages: [user] }),
      }),
      { ...f.options, runId: undefined },
    ),
    /persisted server-owned run ID/,
  );
  assert.deepEqual(f.calls, []);
});

test("a browser abort while detached creation completes does not cancel or retry its run", async () => {
  const entered = deferred<void>();
  const created = deferred<Response>();
  const f = fixture({
    disconnectPolicy: "detach",
    create: async () => {
      entered.resolve();
      return created.promise;
    },
  });
  const abort = new AbortController();
  const opening = start(f.client, abort.signal);
  await entered.promise;
  abort.abort(new Error("browser left before creation response"));
  created.resolve(Response.json({ run_id: runId, thread_id: threadId }));
  await assert.rejects(opening, /browser left before creation response/);
  assert.deepEqual(
    f.calls.map((call) => call.path),
    [`/threads/${threadId}/runs`],
  );
});

test("a lost detached creation response leaves the application-owned run available for recovery", async () => {
  const f = fixture({
    disconnectPolicy: "detach",
    create: async () => {
      throw new Error("creation response lost");
    },
  });
  await assert.rejects(start(f.client), /creation response lost/);
  assert.deepEqual(
    f.calls.map((call) => call.path),
    [`/threads/${threadId}/runs`],
  );
});

test("detached stream setup failure never cancels the successfully created run", async () => {
  const f = fixture({
    disconnectPolicy: "detach",
    streamResponse: async () => new Response(null, { status: 503 }),
  });
  await assert.rejects(start(f.client), /did not return an event stream/);
  assert.equal(f.calls.filter((call) => call.path.endsWith("/runs")).length, 1);
  assert.equal(
    f.calls.some((call) => call.path.endsWith("/cancel")),
    false,
  );
});

test("detached delivery failure reports an error without canceling or dispatching again", async () => {
  const f = fixture({
    disconnectPolicy: "detach",
    frames: normalFrames().slice(0, -1),
  });
  const chunks = await collect(await start(f.client));
  assert.equal(chunks.at(-1)?.type, "error");
  assert.equal(
    chunks.some((chunk) => chunk.type === "finish"),
    false,
  );
  assert.equal(f.calls.filter((call) => call.path.endsWith("/runs")).length, 1);
  assert.equal(
    f.calls.some((call) => call.path.endsWith("/cancel")),
    false,
  );
});

for (const status of ["pending", "running"]) {
  test(`explicit cancellation verifies ${status} run identity before issuing one cancel`, async () => {
    const f = fixture({
      lookup: async () =>
        Response.json({ run_id: runId, thread_id: threadId, status }),
    });
    await cancelManagedChatRun({ ...f.options, runId });
    assert.deepEqual(
      f.calls.map((call) => [call.path, call.init?.method ?? "GET"]),
      [
        [`/threads/${threadId}/runs/${runId}`, "GET"],
        [`/threads/${threadId}/runs/${runId}/cancel`, "POST"],
      ],
    );
  });
}

for (const status of ["success", "error", "timeout", "interrupted"]) {
  test(`explicit cancellation does not mutate an already ${status} run`, async () => {
    const f = fixture({
      lookup: async () =>
        Response.json({ run_id: runId, thread_id: threadId, status }),
    });
    await cancelManagedChatRun({ ...f.options, runId });
    assert.equal(f.calls.length, 1);
    assert.equal(f.calls[0].init?.method, "GET");
  });
}

for (const incorrect of [
  { run_id: "another-run", thread_id: threadId, status: "running" },
  { run_id: runId, thread_id: "another-thread", status: "running" },
  { run_id: runId, thread_id: threadId, status: "unknown" },
]) {
  test(`explicit cancellation refuses an unverified lookup: ${JSON.stringify(incorrect)}`, async () => {
    const f = fixture({ lookup: async () => Response.json(incorrect) });
    await assert.rejects(
      cancelManagedChatRun({ ...f.options, runId }),
      /identity|status/,
    );
    assert.equal(f.calls.length, 1);
    assert.equal(f.calls[0].init?.method, "GET");
  });
}

test("explicit cancellation exposes a failed lookup without sending a mutation", async () => {
  const f = fixture({
    lookup: async () => new Response(null, { status: 503 }),
  });
  await assert.rejects(
    cancelManagedChatRun({ ...f.options, runId }),
    /lookup failed/,
  );
  assert.equal(f.calls.length, 1);
  assert.equal(f.calls[0].init?.method, "GET");
});

test("explicit cancellation rejects an unconfirmed cancel response", async () => {
  const f = fixture({ cancelStatus: 503 });
  await assert.rejects(
    cancelManagedChatRun({ ...f.options, runId }),
    /cancellation was not confirmed/,
  );
  assert.equal(
    f.calls.filter((call) => call.path.endsWith("/cancel")).length,
    1,
  );
});

for (const [missing, truncate] of [
  ["prefix", (parts: string[]) => parts.slice(2)],
  [
    "middle frame",
    (parts: string[]) => parts.filter((_part, index) => index !== 4),
  ],
  [
    "middle block",
    (parts: string[]) => parts.filter((_part, index) => index < 3 || index > 5),
  ],
] as const) {
  test(`attach refuses a missing replay ${missing} instead of finishing partial output`, async () => {
    const f = fixture({ frames: truncate(normalFrames()) });
    const chunks = await collect(await attached(f));
    assert.equal(chunks.at(-1)?.type, "error");
    assert.match(JSON.stringify(chunks.at(-1)), /replay is unavailable/);
    assert.equal(
      chunks.some((chunk) => chunk.type === "finish"),
      false,
    );
    assert.ok(f.calls.every((call) => (call.init?.method ?? "GET") === "GET"));
  });
}

test("a native error cannot become a successful attach even when durable status says success", async () => {
  const parts = normalFrames();
  parts.splice(
    -1,
    0,
    native({ type: "error", errorText: "private provider detail" }, 11),
  );
  const ordered = parts.map((part, index) =>
    part.replace(/^id: [^\n]+\n/, `id: ${runId}_evt_${index + 1}\n`),
  );
  const f = fixture({ frames: ordered });
  const chunks = await collect(await attached(f));
  assert.equal(chunks.at(-1)?.type, "error");
  assert.equal(
    chunks.some((chunk) => chunk.type === "finish"),
    false,
  );
  assert.doesNotMatch(JSON.stringify(chunks), /private provider detail/);
  assert.equal(
    f.calls.some((call) => call.path.endsWith("/cancel")),
    false,
  );
});

test("full replay sequence includes runner lifecycle and message mirrors but ignores control-plane IDs", async () => {
  const parts = normalFrames();
  parts.unshift(frame("lifecycle", { event: "running" }, "placeholder"));
  parts.splice(
    5,
    0,
    frame("messages", [{ content: "Hello " }, {}], "placeholder"),
  );
  const ordered = parts.map((part, index) =>
    part.replace(/^id: [^\n]+\n/, `id: ${runId}_evt_${index + 1}\n`),
  );
  ordered.splice(
    4,
    0,
    frame("metadata", { run_id: runId }, "cp-lifecycle-one"),
  );
  const f = fixture({ frames: ordered });
  const chunks = await collect(await attached(f));
  assert.equal(chunks.at(-1)?.type, "finish");
  assert.equal(
    chunks
      .filter((chunk) => chunk.type === "text-delta")
      .map((chunk) => chunk.delta)
      .join(""),
    "Hello world",
  );
});

test("attach bounds a stalled run lookup request and preserves the existing run", async () => {
  let lookupSignal: AbortSignal | null | undefined;
  const f = fixture({
    lookup: async (signal) => {
      lookupSignal = signal;
      return waitForAbort(signal);
    },
  });
  f.options.timeoutMs = 5;
  await assert.rejects(within(attached(f)), { name: "TimeoutError" });
  assert.equal(lookupSignal?.aborted, true);
  assert.equal(f.calls.length, 1);
});

test("attach bounds a stalled lookup body and cancels its reader without mutating the run", async () => {
  let canceled = false;
  const f = fixture({
    lookup: async () =>
      new Response(
        new ReadableStream({
          start(output) {
            output.enqueue(encoder.encode('{"run_id":'));
          },
          cancel() {
            canceled = true;
          },
        }),
      ),
  });
  f.options.timeoutMs = 5;
  await assert.rejects(within(attached(f)), { name: "TimeoutError" });
  assert.equal(canceled, true);
  assert.equal(f.calls.length, 1);
});

test("a detached create response body has a deadline and does not cancel an uncertain run", async () => {
  let canceled = false;
  const f = fixture({
    disconnectPolicy: "detach",
    create: async () =>
      new Response(
        new ReadableStream({
          start(output) {
            output.enqueue(encoder.encode('{"run_id":'));
          },
          cancel() {
            canceled = true;
          },
        }),
      ),
  });
  f.options.timeoutMs = 5;
  await assert.rejects(within(start(f.client)), { name: "TimeoutError" });
  assert.equal(canceled, true);
  assert.equal(f.calls.length, 1);
});

test("attach bounds a stalled durable wait request without confirming UI finish", async () => {
  let waitSignal: AbortSignal | null | undefined;
  const f = fixture({
    wait: async (signal) => {
      waitSignal = signal;
      return waitForAbort(signal);
    },
  });
  f.options.timeoutMs = 5;
  const chunks = await within(collect(await attached(f)));
  assert.equal(waitSignal?.aborted, true);
  assert.equal(chunks.at(-1)?.type, "error");
  assert.equal(
    chunks.some((chunk) => chunk.type === "finish"),
    false,
  );
  assert.equal(
    f.calls.some((call) => call.path.endsWith("/cancel")),
    false,
  );
});

test("attach bounds a stalled durable wait body and closes its reader without confirming UI finish", async () => {
  let canceled = false;
  const f = fixture({
    wait: async () =>
      new Response(
        new ReadableStream({
          start(output) {
            output.enqueue(encoder.encode('{"run":'));
          },
          cancel() {
            canceled = true;
          },
        }),
      ),
  });
  f.options.timeoutMs = 5;
  const chunks = await within(collect(await attached(f)));
  assert.equal(canceled, true);
  assert.equal(chunks.at(-1)?.type, "error");
  assert.equal(
    chunks.some((chunk) => chunk.type === "finish"),
    false,
  );
  assert.equal(
    f.calls.some((call) => call.path.endsWith("/cancel")),
    false,
  );
});

test("attach refuses a newer retained generation before replacing a saved partial assistant", async () => {
  const f = fixture({
    frames: normalFrames(2),
    expectedMessageId: `${runId}:g1:assistant`,
    wait: async () => Response.json(f.result("success", 2)),
  });
  const chunks = await collect(await attached(f));
  assert.equal(chunks.at(-1)?.type, "error");
  assert.equal(
    chunks.some((chunk) => chunk.type === "start" || chunk.type === "finish"),
    false,
  );
  assert.ok(f.calls.every((call) => (call.init?.method ?? "GET") === "GET"));
});

test("attach accepts the saved assistant identity when its retained generation matches", async () => {
  const f = fixture({
    frames: normalFrames(2),
    expectedMessageId: `${runId}:g2:assistant`,
    wait: async () => Response.json(f.result("success", 2)),
  });
  const chunks = await collect(await attached(f));
  const first = chunks[0];
  assert.ok(first.type === "start");
  assert.equal(first.messageId, `${runId}:g2:assistant`);
  assert.equal(chunks.at(-1)?.type, "finish");
});

test("a foreign saved assistant identity is rejected before any plane request", async () => {
  const f = fixture({ expectedMessageId: "another-run:g1:assistant" });
  await assert.rejects(attached(f), /message|identity/i);
  assert.deepEqual(f.calls, []);
});

test("the app persists the assistant identity before its first UI start becomes visible", async () => {
  const entered = deferred<string>();
  const persisted = deferred<void>();
  let callbacks = 0;
  const f = fixture({
    onMessageStart: async (messageId) => {
      callbacks++;
      entered.resolve(messageId);
      await persisted.promise;
    },
  });
  const chunks: UIMessageChunk[] = [];
  const source = await attached(f);
  const completed = (async () => {
    for await (const chunk of source) chunks.push(chunk);
  })();
  assert.equal(await entered.promise, `${runId}:g1:assistant`);
  await Promise.resolve();
  assert.equal(
    chunks.length,
    0,
    "the partial response must not outrun its saved identity",
  );
  persisted.resolve();
  await completed;
  assert.equal(callbacks, 1);
  assert.equal(chunks[0].type, "start");
  assert.equal(chunks.at(-1)?.type, "finish");
});

test("failed assistant-identity persistence stops delivery without a second dispatch or cancel", async () => {
  let callbacks = 0;
  const f = fixture({
    onMessageStart: async () => {
      callbacks++;
      throw new Error("private application database error");
    },
  });
  const chunks = await collect(await attached(f));
  assert.equal(callbacks, 1);
  assert.equal(chunks.at(-1)?.type, "error");
  assert.equal(
    chunks.some((chunk) => chunk.type === "start" || chunk.type === "finish"),
    false,
  );
  assert.doesNotMatch(
    JSON.stringify(chunks),
    /private application database error/,
  );
  assert.ok(f.calls.every((call) => (call.init?.method ?? "GET") === "GET"));
});

test("assistant-identity persistence has a deadline and cannot emit a late UI start", async () => {
  const entered = deferred<AbortSignal>();
  const persisted = deferred<void>();
  const f = fixture({
    onMessageStart: async (_messageId, signal) => {
      entered.resolve(signal);
      await persisted.promise;
    },
  });
  f.options.timeoutMs = 5;
  const completed = collect(await attached(f));
  const callbackSignal = await entered.promise;
  const chunks = await within(completed);
  assert.equal(callbackSignal.aborted, true);
  assert.equal(chunks.at(-1)?.type, "error");
  assert.equal(
    chunks.some((chunk) => chunk.type === "start" || chunk.type === "finish"),
    false,
  );
  persisted.resolve();
  await Promise.resolve();
  assert.equal(
    chunks.length,
    1,
    "a late app write cannot restart a closed delivery",
  );
  assert.ok(f.calls.every((call) => (call.init?.method ?? "GET") === "GET"));
});

test("a browser disconnect aborts pending identity persistence without a late start or remote cancellation", async () => {
  const entered = deferred<AbortSignal>();
  const persisted = deferred<void>();
  const abort = new AbortController();
  const f = fixture({
    onMessageStart: async (_messageId, signal) => {
      entered.resolve(signal);
      await persisted.promise;
    },
  });
  const completed = collect(await attached(f, abort.signal));
  const callbackSignal = await entered.promise;
  abort.abort(new Error("browser disconnected during app persistence"));
  const chunks = await within(completed);
  assert.equal(callbackSignal.aborted, true);
  assert.equal(chunks.at(-1)?.type, "abort");
  persisted.resolve();
  await Promise.resolve();
  assert.equal(chunks.length, 1);
  assert.equal(
    chunks.some((chunk) => chunk.type === "start" || chunk.type === "finish"),
    false,
  );
  assert.ok(f.calls.every((call) => (call.init?.method ?? "GET") === "GET"));
});
