import assert from "node:assert/strict";
import { test } from "node:test";
import {
  HttpAgent,
  EventType,
  type Event,
  type Message,
  type AgentSubscriber,
} from "@ag-ui/client";
import { EventSchema } from "@ag-ui/core/schemas";
import { createManagedAGUIRoutes, type AGUIOperation } from "./routes.ts";
import { reconstructManagedAGUI } from "./replay.ts";
import { createManagedAGUIResponse } from "./bridge.ts";

const runId = "managed-run";
const threadId = "managed-thread";
const appRunId = "app-turn";
const appThreadId = "app-chat";
const input = "New user turn 🌍";
const messageId = `${runId}:g1:assistant`;
const encoder = new TextEncoder();

function frame(
  event: string,
  data: unknown,
  sequence: number,
  generation = 1,
): string {
  return `id: ${runId}_evt_${sequence}${generation > 1 ? `:g${generation}` : ""}\r\nevent: ${event}\r\ndata: ${JSON.stringify(data)}\r\n\r\n`;
}
function fragmented(text: string, width = 1): ReadableStream<Uint8Array> {
  const bytes = encoder.encode(text);
  let offset = 0;
  return new ReadableStream({
    pull(output) {
      if (offset === bytes.length) return output.close();
      output.enqueue(bytes.slice(offset, offset + width));
      offset += Math.min(width, bytes.length - offset);
    },
  });
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { resolve, promise };
}

type Settings = {
  parts?: (frames: string[]) => string[];
  status?: "success" | "error" | "timeout" | "interrupted";
  wait?: (response: Record<string, unknown>) => Promise<Response>;
  stream?: ReadableStream<Uint8Array>;
  profile?: "google-adk" | "ai-sdk";
  chunks?: Record<string, unknown>[];
  cancelStatus?: number;
  onBind?: (id: string) => Promise<void>;
  expectedMessageId?: string;
};

function fixture(settings: Settings = {}) {
  const pieces = ["Hello ", "🌍", " e", "\u0301", " भारत", "\nnext"];
  const text = pieces.join("");
  const operations: AGUIOperation[] = [];
  const calls: {
    path: string;
    method: string;
    body?: any;
    credential: string | null;
  }[] = [];
  let bound: string | undefined;
  let authorized = true;
  let currentStatus = settings.status ?? "success";
  const values = {
    messages: [
      { role: "user", content: input },
      { role: "assistant", content: text },
    ],
    google_adk: { framework: "google-adk", generation: 1 },
    ai_sdk: { framework: "ai-sdk", generation: 1, message_id: messageId },
    secret: "upstream-private-metadata",
  };
  const normal = [
    frame("lifecycle", { event: "running" }, 1),
    ...pieces.map((part, index) =>
      frame(
        "messages",
        [{ id: `${runId}:assistant`, type: "ai", content: part }, {}],
        index + 2,
      ),
    ),
    frame("values", values, pieces.length + 2),
    frame("end", { status: "success" }, pieces.length + 3),
  ];
  const native = settings.chunks?.map((chunk, index) =>
    frame("custom:ai_sdk", chunk, index + 1),
  );
  if (native)
    native.push(frame("end", { status: "success" }, native.length + 1));
  const fetcher: typeof fetch = async (url, init) => {
    const request = new Request(url, init);
    const path = new URL(request.url).pathname;
    calls.push({
      path,
      method: request.method,
      credential: request.headers.get("Authorization"),
      body:
        request.method === "POST" && path.endsWith("/runs")
          ? await request.json()
          : undefined,
    });
    if (path.endsWith("/cancel")) {
      if (settings.cancelStatus)
        return new Response(null, { status: settings.cancelStatus });
      currentStatus = "interrupted";
      return new Response(null, { status: 204 });
    }
    if (path.endsWith("/stream"))
      return new Response(
        settings.stream ??
          fragmented((settings.parts?.(normal) ?? native ?? normal).join("")),
        { headers: { "Content-Type": "text/event-stream" } },
      );
    if (path.endsWith("/wait")) {
      const response = {
        run: {
          run_id: runId,
          thread_id: threadId,
          status: currentStatus,
          input: { messages: [{ role: "user", content: input }] },
          metadata: { _runkite_completion_generation: 1 },
        },
        values,
      };
      return settings.wait ? settings.wait(response) : Response.json(response);
    }
    return Response.json({
      run_id: runId,
      thread_id: threadId,
      status: "running",
    });
  };
  const routes = createManagedAGUIRoutes({
    baseUrl: "https://plane.example",
    apiKey: "server-only-key",
    fetch: fetcher,
    timeoutMs: 200,
    async resolveTurn(_request, operation) {
      operations.push(operation);
      if (!authorized) return new Response(null, { status: 401 });
      return {
        threadId,
        runId,
        agentId: "owned-agent",
        profile: settings.profile ?? "google-adk",
        aguiThreadId: appThreadId,
        aguiRunId: appRunId,
        get expectedMessageId() {
          return settings.expectedMessageId ?? bound;
        },
        async onMessageStart(id) {
          if (bound && bound !== id) throw new Error("attempt changed");
          await settings.onBind?.(id);
          bound = id;
        },
      };
    },
  });
  const events: Event[] = [];
  const appFetch = async (url: string, init: RequestInit) => {
    const request = new Request(url, init);
    const response =
      request.method === "GET"
        ? await routes.attach(request)
        : await routes.create(request);
    assert.equal(
      [...response.headers].some((entry) =>
        entry.join().includes("server-only-key"),
      ),
      false,
    );
    return response;
  };
  const agent = new HttpAgent({
    url: "https://app.example/agui",
    threadId: appThreadId,
    initialMessages: [{ id: "user-message", role: "user", content: input }],
    fetch: appFetch,
  });
  const subscriber: AgentSubscriber = {
    onEvent({ event }) {
      events.push(EventSchema.parse(event));
    },
  };
  return {
    agent,
    subscriber,
    events,
    routes,
    calls,
    operations,
    text,
    appFetch,
    setAuthorized(value: boolean) {
      authorized = value;
    },
    setStatus(value: typeof currentStatus) {
      currentStatus = value;
    },
    get bound() {
      return bound;
    },
  };
}

test("official AG-UI client reconstructs byte-fragmented Unicode text and terminal success", async () => {
  const f = fixture();
  await f.agent.runAgent({ runId: appRunId }, f.subscriber);
  const assistant = f.agent.messages.find(
    (message) => message.id === messageId,
  );
  assert.equal(assistant?.role, "assistant");
  assert.equal(assistant?.content, f.text);
  assert.equal(f.bound, messageId);
  assert.deepEqual(
    f.events.map((event) => event.type),
    [
      EventType.RUN_STARTED,
      EventType.TEXT_MESSAGE_START,
      ...Array(6).fill(EventType.TEXT_MESSAGE_CONTENT),
      EventType.TEXT_MESSAGE_END,
      EventType.RUN_FINISHED,
    ],
  );
  const terminal = f.events.at(-1)!;
  assert.equal(
    terminal.type === EventType.RUN_FINISHED && terminal.outcome?.type,
    "success",
  );
  assert.equal(JSON.stringify(f.events).includes("server-only-key"), false);
  assert.equal(JSON.stringify(f.events).includes("upstream-private"), false);
  assert.equal(f.calls[0]?.path, `/threads/${threadId}/runs`);
  assert.deepEqual(f.calls[0]?.body.input, {
    messages: [{ role: "user", content: input }],
  });
  assert.ok(
    f.calls.every((call) => call.credential === "Bearer server-only-key"),
  );
});

test("AG-UI finish waits for durable confirmation", async () => {
  const gate = deferred<Response>();
  const reached = deferred<void>();
  const f = fixture({
    wait: async () => {
      reached.resolve();
      return gate.promise;
    },
  });
  const pending = f.agent.runAgent({ runId: appRunId }, f.subscriber);
  await reached.promise;
  assert.equal(
    f.events.some((event) => event.type === EventType.RUN_FINISHED),
    false,
  );
  gate.resolve(
    Response.json({
      run: {
        run_id: runId,
        thread_id: threadId,
        status: "success",
        input: { messages: [{ role: "user", content: input }] },
        metadata: { _runkite_completion_generation: 1 },
      },
      values: {
        messages: [
          { role: "user", content: input },
          { role: "assistant", content: f.text },
        ],
        google_adk: { framework: "google-adk", generation: 1 },
      },
    }),
  );
  await pending;
  assert.equal(f.events.at(-1)?.type, EventType.RUN_FINISHED);
});

for (const status of ["error", "timeout", "interrupted"] as const)
  test(`durable ${status} cannot become AG-UI success or native approval`, async () => {
    const f = fixture({ status });
    await f.agent.runAgent({ runId: appRunId }, f.subscriber).catch(() => {});
    assert.equal(f.events.at(-1)?.type, EventType.RUN_ERROR);
    assert.equal(
      f.events.some((event) => event.type === EventType.RUN_FINISHED),
      false,
    );
    assert.equal(
      f.events.some(
        (event) =>
          event.type.startsWith("TOOL_") ||
          event.type === EventType.STATE_SNAPSHOT,
      ),
      false,
    );
  });

test("mixed managed generations cannot combine text or finish", async () => {
  const f = fixture({
    parts: (frames) => [
      frames[0]!,
      frames[1]!,
      frame(
        "messages",
        [
          {
            id: `${runId}:assistant`,
            type: "ai",
            content: "foreign-generation",
          },
          {},
        ],
        3,
        2,
      ),
      ...frames.slice(2),
    ],
  });
  await f.agent.runAgent({ runId: appRunId }, f.subscriber).catch(() => {});
  assert.equal(f.events.at(-1)?.type, EventType.RUN_ERROR);
  assert.equal(JSON.stringify(f.events).includes("foreign-generation"), false);
  assert.equal(
    f.events.some((event) => event.type === EventType.RUN_FINISHED),
    false,
  );
});

test("unsupported native tool event is not fabricated into an AG-UI tool", async () => {
  const f = fixture({
    profile: "ai-sdk",
    chunks: [
      { type: "start", messageId },
      {
        type: "tool-input-start",
        toolCallId: "private-call",
        toolName: "private-tool",
      },
    ],
  });
  await f.agent.runAgent({ runId: appRunId }, f.subscriber).catch(() => {});
  assert.equal(f.events.at(-1)?.type, EventType.RUN_ERROR);
  assert.equal(JSON.stringify(f.events).includes("private-tool"), false);
  assert.equal(
    f.events.some(
      (event) =>
        event.type.startsWith("TOOL_") || event.type === EventType.RUN_FINISHED,
    ),
    false,
  );
});

test("a valid native completion without a text part fails creation and retained reconstruction consistently", async () => {
  const f = fixture({
    profile: "ai-sdk",
    chunks: [
      { type: "start", messageId },
      { type: "start-step" },
      { type: "finish-step" },
      { type: "finish", finishReason: "stop" },
    ],
  });
  await f.agent.runAgent({ runId: appRunId }, f.subscriber).catch(() => {});
  assert.deepEqual(
    f.events.map((event) => event.type),
    [EventType.RUN_STARTED, EventType.RUN_ERROR],
  );
  assert.equal(
    f.agent.messages.some((message) => message.id === messageId),
    false,
  );
  await assert.rejects(
    reconstructManagedAGUI({
      attachUrl: "https://app.example/attach",
      threadId: appThreadId,
      runId: appRunId,
      assistantMessageId: messageId,
      messages: [{ id: "u", role: "user", content: input }],
      fetch: f.appFetch,
    }),
  );
  assert.equal(
    f.calls.filter((call) => call.method === "POST").length,
    1,
  );
});

test("application authorization and protocol IDs cannot select another managed run", async () => {
  const f = fixture();
  f.setAuthorized(false);
  const denied = await f.routes.create(
    new Request("https://app.example/agui", { method: "POST", body: "{}" }),
  );
  assert.equal(denied.status, 401);
  assert.equal(f.calls.length, 0);
  f.setAuthorized(true);
  const response = await f.routes.create(
    new Request("https://app.example/agui", {
      method: "POST",
      body: JSON.stringify({
        threadId: "foreign",
        runId: appRunId,
        messages: [{ id: "u", role: "user", content: input }],
        tools: [],
        context: [],
      }),
    }),
  );
  assert.equal(response.status, 400);
  assert.equal(f.calls.length, 0);
  const wrongMethod = await f.routes.stop(
    new Request("https://app.example/stop"),
  );
  assert.equal(wrongMethod.status, 405);
});

test("client tools, resume, nonempty state and malformed Unicode fail before dispatch", async () => {
  for (const extra of [
    { tools: [{ name: "execute", description: "x", parameters: {} }] },
    { state: { injected: true } },
    { forwardedProps: { agentId: "foreign" } },
    { resume: [{ interruptId: "fake", status: "success", value: true }] },
    { messages: [{ id: "u", role: "user", content: "\ud800" }] },
  ]) {
    const f = fixture();
    const response = await f.routes.create(
      new Request("https://app.example/agui", {
        method: "POST",
        body: JSON.stringify({
          threadId: appThreadId,
          runId: appRunId,
          messages: [{ id: "u", role: "user", content: input }],
          tools: [],
          context: [],
          ...extra,
        }),
      }),
    );
    assert.equal(response.status, 400);
    assert.equal(f.calls.length, 0);
  }
});

test("full reattachment reconstructs a replacement response rather than appending", async () => {
  const f = fixture();
  await f.agent.runAgent({ runId: appRunId }, f.subscriber);
  const saved: Message[] = [
    { id: "u", role: "user", content: input },
    { id: messageId, role: "assistant", content: "partial" },
  ];
  const before = f.calls.length;
  const replayed = await reconstructManagedAGUI({
    attachUrl: "https://app.example/attach",
    threadId: appThreadId,
    runId: appRunId,
    assistantMessageId: messageId,
    messages: saved,
    fetch: f.appFetch,
  });
  assert.equal(
    replayed.filter((message) => message.id === messageId).length,
    1,
  );
  assert.equal(
    replayed.find((message) => message.id === messageId)?.content,
    f.text,
  );
  assert.equal(
    saved.find((message) => message.id === messageId)?.content,
    "partial",
  );
  assert.ok(f.calls.slice(before).every((call) => call.method === "GET"));
});

test("native client abort detaches delivery without cancelling the managed run", async () => {
  let cancelled = false;
  const f = fixture({
    stream: new ReadableStream({
      start(output) {
        output.enqueue(
          encoder.encode(
            frame("lifecycle", { event: "running" }, 1) +
              frame(
                "messages",
                [
                  { id: `${runId}:assistant`, type: "ai", content: "partial" },
                  {},
                ],
                2,
              ),
          ),
        );
      },
      cancel() {
        cancelled = true;
      },
    }),
  });
  const pending = f.agent.runAgent(
    { runId: appRunId },
    {
      onTextMessageContentEvent() {
        f.agent.abortRun();
      },
    },
  );
  await pending.catch(() => {});
  assert.equal(f.bound, messageId);
  assert.equal(
    f.calls.some((call) => call.path.endsWith("/cancel")),
    false,
  );
  assert.equal(cancelled, true);
});

test("response body cancellation also detaches without remote cancellation", async () => {
  let cancelled = false;
  const f = fixture({
    stream: new ReadableStream({
      start(output) {
        output.enqueue(
          encoder.encode(
            frame("lifecycle", { event: "running" }, 1) +
              frame(
                "messages",
                [
                  { id: `${runId}:assistant`, type: "ai", content: "partial" },
                  {},
                ],
                2,
              ),
          ),
        );
      },
      cancel() {
        cancelled = true;
      },
    }),
  });
  const response = await f.routes.create(
    new Request("https://app.example/agui", {
      method: "POST",
      body: JSON.stringify({
        threadId: appThreadId,
        runId: appRunId,
        messages: [{ id: "u", role: "user", content: input }],
        tools: [],
        context: [],
      }),
    }),
  );
  const reader = response.body!.getReader();
  for (;;) {
    const next = await reader.read();
    assert.equal(next.done, false);
    if (
      new TextDecoder()
        .decode(next.value)
        .includes(EventType.TEXT_MESSAGE_CONTENT)
    )
      break;
  }
  await reader.cancel();
  reader.releaseLock();
  assert.equal(cancelled, true);
  assert.equal(
    f.calls.some((call) => call.path.endsWith("/cancel")),
    false,
  );
});

test("explicit authenticated Stop cancels only the resolved owned run", async () => {
  const f = fixture();
  const stopped = await f.routes.stop(
    new Request("https://app.example/stop?run_id=foreign", {
      method: "POST",
      body: JSON.stringify({ runId: "foreign" }),
    }),
  );
  assert.equal(stopped.status, 204);
  assert.deepEqual(f.operations, ["stop"]);
  assert.deepEqual(
    f.calls.map((call) => [call.path, call.method]),
    [
      [`/threads/${threadId}/runs/${runId}`, "GET"],
      [`/threads/${threadId}/runs/${runId}/cancel`, "POST"],
    ],
  );
  f.setAuthorized(false);
  const denied = await f.routes.stop(
    new Request("https://app.example/stop", { method: "POST" }),
  );
  assert.equal(denied.status, 401);
  assert.equal(f.calls.length, 2);
});

test("failed Stop is not reported as accepted cancellation", async () => {
  const f = fixture({ cancelStatus: 503 });
  await assert.rejects(
    f.routes.stop(new Request("https://app.example/stop", { method: "POST" })),
  );
});

for (const [name, settings] of [
  ["truncated source", { parts: (frames: string[]) => frames.slice(0, 2) }],
  [
    "failed durable wait",
    { wait: async () => new Response(null, { status: 503 }) },
  ],
  [
    "uncommitted assistant binding",
    {
      onBind: async () => {
        throw new Error("private database failure");
      },
    },
  ],
  [
    "stored assistant from another attempt",
    { expectedMessageId: `${runId}:g2:assistant` },
  ],
] satisfies [string, Settings][])
  test(`${name} produces no successful native finish`, async () => {
    const f = fixture(settings);
    await f.agent.runAgent({ runId: appRunId }, f.subscriber).catch(() => {});
    assert.equal(f.events.at(-1)?.type, EventType.RUN_ERROR);
    assert.equal(
      f.events.some((event) => event.type === EventType.RUN_FINISHED),
      false,
    );
    assert.equal(JSON.stringify(f.events).includes("private database"), false);
    if (name.includes("binding") || name.includes("another attempt"))
      assert.equal(
        f.events.some((event) => event.type === EventType.TEXT_MESSAGE_CONTENT),
        false,
      );
  });

test("duplicate retained source events do not duplicate native text", async () => {
  const f = fixture({
    parts: (frames) => frames.flatMap((item) => [item, item]),
  });
  await f.agent.runAgent({ runId: appRunId }, f.subscriber);
  assert.equal(
    f.agent.messages.find((message) => message.id === messageId)?.content,
    f.text,
  );
});

test("native input bounds, protocol and transport are checked before dispatch", async () => {
  for (const [headers, body] of [
    [{ Accept: "application/vnd.ag-ui+proto" }, "{}"],
    [{ Accept: "text/event-stream;q=0" }, "{}"],
    [{}, " ".repeat(65 * 1024)],
    [
      {},
      JSON.stringify({
        threadId: appThreadId,
        runId: appRunId,
        protocolVersion: "999",
        messages: [{ id: "u", role: "user", content: input }],
        tools: [],
        context: [],
      }),
    ],
    [
      {},
      JSON.stringify({
        threadId: appThreadId,
        runId: appRunId,
        messages: [{ id: "u", role: "user", content: input }],
        tools: [],
        context: [],
        unknown: true,
      }),
    ],
  ] satisfies [Record<string, string>, string][]) {
    const f = fixture();
    const response = await f.routes.create(
      new Request("https://app.example/agui", {
        method: "POST",
        headers,
        body,
      }),
    );
    assert.equal(response.status, 400);
    assert.equal(f.calls.length, 0);
  }
});

test("failed replay leaves previously saved messages untouched", async () => {
  const f = fixture({ status: "error" });
  const saved: Message[] = [
    { id: "u", role: "user", content: input },
    { id: messageId, role: "assistant", content: "partial" },
  ];
  await assert.rejects(
    reconstructManagedAGUI({
      attachUrl: "https://app.example/attach",
      threadId: appThreadId,
      runId: appRunId,
      assistantMessageId: messageId,
      messages: saved,
      fetch: f.appFetch,
    }),
  );
  assert.equal(saved[1]?.content, "partial");
  assert.ok(f.calls.every((call) => call.method === "GET"));
});

test("replay refuses another stored application or assistant identity", async () => {
  for (const identity of [
    { threadId: "another-app-chat" },
    { runId: "another-app-turn" },
    { assistantMessageId: `${runId}:g2:assistant` },
  ]) {
    const f = fixture();
    const saved: Message[] = [
      { id: "u", role: "user", content: input },
      { id: messageId, role: "assistant", content: "partial" },
    ];
    await assert.rejects(
      reconstructManagedAGUI({
        attachUrl: "https://app.example/attach",
        threadId: appThreadId,
        runId: appRunId,
        assistantMessageId: messageId,
        messages: saved,
        fetch: f.appFetch,
        ...identity,
      }),
    );
    assert.equal(saved[1]?.content, "partial");
    assert.ok(f.calls.every((call) => call.method === "GET"));
  }
});

test("intake cancellation cannot extend oversized or already-aborted rejection", async () => {
  for (const alreadyAborted of [false, true]) {
    const f = fixture();
    let cancelled = false;
    const controller = new AbortController();
    if (alreadyAborted) controller.abort(new Error("disconnected"));
    const request = new Request("https://app.example/agui", {
      method: "POST",
      signal: controller.signal,
      duplex: "half",
      body: new ReadableStream<Uint8Array>({
        start(output) {
          if (!alreadyAborted) output.enqueue(new Uint8Array(65 * 1024));
        },
        cancel() {
          cancelled = true;
          return new Promise<void>(() => {});
        },
      }),
    } as RequestInit);
    let timer: ReturnType<typeof setTimeout> | undefined;
    try {
      const result = alreadyAborted
        ? createManagedAGUIResponse(request, {
            baseUrl: "https://plane.example",
            apiKey: "server-key",
            threadId,
            agentId: "owned-agent",
            runId,
            aguiThreadId: appThreadId,
            aguiRunId: appRunId,
          }).then(
            () => "unexpected success",
            () => "aborted",
          )
        : f.routes.create(request);
      const response = await Promise.race([
        result,
        new Promise<never>((_, reject) => {
          timer = setTimeout(
            () => reject(new Error("intake rejection hung on cancellation")),
            200,
          );
        }),
      ]);
      if (alreadyAborted) assert.equal(response, "aborted");
      else assert.equal((response as Response).status, 400);
      assert.equal(cancelled, true);
      assert.equal(f.calls.length, 0);
    } finally {
      clearTimeout(timer);
    }
  }
});

test("specific SSE exclusion overrides an acceptable wildcard before dispatch", async () => {
  for (const accept of [
    "text/event-stream;q=0, */*;q=1",
    "text/event-stream ;q=0, */*;q=1",
    "text/event-stream \t; q=0, */*;q=1",
    "text/* ;q=0, */*;q=1",
  ]) {
    const f = fixture();
    const response = await f.routes.create(
      new Request("https://app.example/agui", {
        method: "POST",
        headers: { Accept: accept },
        body: JSON.stringify({
          threadId: appThreadId,
          runId: appRunId,
          messages: [{ id: "u", role: "user", content: input }],
          tools: [],
          context: [],
        }),
      }),
    );
    assert.equal(response.status, 400, accept);
    assert.equal(f.calls.length, 0, accept);
  }
});
