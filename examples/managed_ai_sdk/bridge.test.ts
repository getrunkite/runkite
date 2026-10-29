import assert from "node:assert/strict";
import { test } from "node:test";
import {
  DefaultChatTransport,
  readUIMessageStream,
  type UIMessage,
  type UIMessageChunk,
} from "ai";
import {
  createManagedChatResponse,
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
    wait?: () => Promise<Response>;
    create?: () => Promise<Response>;
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
        ? settings.create()
        : Response.json({ run_id: runId, thread_id: threadId });
    }
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
      return settings.wait ? settings.wait() : Response.json(result());
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
  };
  const client = new DefaultChatTransport({
    api: "http://app/api/chat",
    fetch: async (url, init) =>
      createManagedChatResponse(new Request(String(url), init), options),
  });
  return { calls, options, client, result, cancelled: cancelled.promise };
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
