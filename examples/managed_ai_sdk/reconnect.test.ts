import assert from "node:assert/strict";
import { test } from "node:test";
import {
  AbstractChat,
  DefaultChatTransport,
  type ChatState,
  type UIMessage,
  type UIMessageChunk,
} from "ai";
import {
  attachManagedChatResponse,
  type ManagedChatAttachOptions,
} from "./bridge.ts";

const threadId = "server-owned-thread";
const runId = "server-owned-run";
const messageId = `${runId}:g1:assistant`;
const user: UIMessage = {
  id: "user-one",
  role: "user",
  parts: [{ type: "text", text: "Create a note" }],
};
const partial: UIMessage = {
  id: messageId,
  role: "assistant",
  parts: [
    { type: "step-start" },
    { type: "text", text: "Hello ", state: "streaming" },
    {
      type: "tool-createNote",
      toolCallId: "call-one",
      state: "input-streaming",
      input: { title: "A" },
    },
  ],
};
const encoder = new TextEncoder();

function frame(event: string, data: unknown, id: string): string {
  return `id: ${id}\nevent: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
}

function nativeChunks(): UIMessageChunk[] {
  return [
    { type: "start", messageId },
    { type: "start-step" },
    { type: "text-start", id: "text-one" },
    { type: "text-delta", id: "text-one", delta: "Hello " },
    { type: "text-delta", id: "text-one", delta: "world" },
    { type: "text-end", id: "text-one" },
    {
      type: "tool-input-available",
      toolCallId: "call-one",
      toolName: "createNote",
      input: { title: "A draft" },
    },
    {
      type: "tool-output-available",
      toolCallId: "call-one",
      output: { note_id: "note-one" },
    },
    { type: "finish-step" },
    { type: "finish", finishReason: "stop" },
  ];
}

function planeStream(replayAvailable: boolean): ReadableStream<Uint8Array> {
  const chunks = replayAvailable ? nativeChunks() : [];
  const encoded = chunks.map((chunk, index) =>
    frame("custom:ai_sdk", chunk, `${runId}_evt_${index + 1}`),
  );
  encoded.push(
    frame("end", { status: "success" }, `${runId}_evt_${chunks.length + 1}`),
  );
  return new ReadableStream({
    start(output) {
      output.enqueue(encoder.encode(encoded.join("")));
      output.close();
    },
  });
}

function fixture(replayAvailable = true) {
  const planeCalls: { path: string; method: string }[] = [];
  const appCalls: Request[] = [];
  const runPath = `/threads/${threadId}/runs/${runId}`;
  const fetcher: typeof fetch = async (url, init) => {
    const path = new URL(String(url)).pathname;
    const method = init?.method ?? "GET";
    planeCalls.push({ path, method });
    assert.equal(
      method,
      "GET",
      "reconnecting must never create or cancel a run",
    );
    assert.equal(
      new Headers(init?.headers).get("authorization"),
      "Bearer server-secret",
    );
    assert.equal(init?.redirect, "error");
    if (path === runPath)
      return Response.json({
        run_id: runId,
        thread_id: threadId,
        agent_id: "server-agent",
        status: "running",
      });
    if (path === `${runPath}/stream`)
      return new Response(planeStream(replayAvailable), {
        headers: { "Content-Type": "text/event-stream" },
      });
    if (path === `${runPath}/wait`)
      return Response.json({
        run: { run_id: runId, thread_id: threadId, status: "success" },
        values: { ai_sdk: { generation: 1, message_id: messageId } },
      });
    throw new Error(`Unexpected plane request: ${path}`);
  };
  const options: ManagedChatAttachOptions = {
    baseUrl: "http://plane",
    apiKey: "server-secret",
    threadId,
    runId,
    fetch: fetcher,
    expectedMessageId: messageId,
  };
  const appFetch: typeof fetch = async (url, init) => {
    const request = new Request(String(url), init);
    appCalls.push(request);
    return attachManagedChatResponse(request, options);
  };
  const transport = new DefaultChatTransport({
    api: "http://application/api/chat?thread_id=browser-thread&run_id=browser-run",
    credentials: "include",
    headers: { "X-App-Auth": "application-session" },
    fetch: appFetch,
  });
  return { planeCalls, appCalls, appFetch, transport };
}

class NativeChat extends AbstractChat<UIMessage> {}

function chat(
  transport: DefaultChatTransport<UIMessage>,
  messages: UIMessage[],
) {
  const state: ChatState<UIMessage> = {
    status: "ready",
    error: undefined,
    messages: structuredClone(messages),
    pushMessage(message) {
      state.messages.push(message);
    },
    popMessage() {
      state.messages.pop();
    },
    replaceMessage(index, message) {
      state.messages[index] = message;
    },
    snapshot: structuredClone,
  };
  return new NativeChat({ id: "browser/chat", transport, state });
}

function assertSingleCompleteAssistant(client: NativeChat) {
  assert.equal(client.status, "ready");
  assert.equal(client.error, undefined);
  assert.equal(client.messages.length, 2);
  const assistants = client.messages.filter(
    (message) => message.role === "assistant",
  );
  assert.equal(assistants.length, 1);
  const assistant = assistants[0];
  assert.equal(assistant.id, messageId);
  assert.deepEqual(
    assistant.parts
      .filter((part) => part.type === "text")
      .map((part) => part.text),
    ["Hello world"],
  );
  assert.equal(
    assistant.parts.filter((part) => part.type === "step-start").length,
    1,
  );
  const tools = assistant.parts.filter(
    (part) => part.type === "tool-createNote",
  );
  assert.equal(tools.length, 1);
  const tool = tools[0];
  assert.ok("state" in tool && tool.state === "output-available");
  assert.deepEqual(tool.input, { title: "A draft" });
  assert.deepEqual(tool.output, { note_id: "note-one" });
}

test("native reconnect uses encoded GET and server-owned run identity without a new dispatch", async () => {
  const f = fixture();
  const client = chat(f.transport, [user, partial]);
  await client.resumeStream({
    headers: { "X-App-Request": "resume" },
    body: {
      thread_id: "body-thread",
      run_id: "body-run",
      agent_id: "body-agent",
    },
  });
  assertSingleCompleteAssistant(client);
  assert.equal(f.appCalls.length, 1);
  const request = f.appCalls[0];
  assert.equal(request.method, "GET");
  assert.equal(
    request.body,
    null,
    "native reconnect never sends the supplied body",
  );
  assert.equal(
    request.url,
    "http://application/api/chat/browser%2Fchat/stream?thread_id=browser-thread&run_id=browser-run",
  );
  assert.equal(request.credentials, "include");
  assert.equal(request.headers.get("X-App-Auth"), "application-session");
  assert.equal(request.headers.get("X-App-Request"), "resume");
  assert.deepEqual(
    f.planeCalls.map((call) => call.path),
    [
      `/threads/${threadId}/runs/${runId}`,
      `/threads/${threadId}/runs/${runId}/stream`,
      `/threads/${threadId}/runs/${runId}/wait`,
    ],
  );
});

test("whole replay repeatedly replaces partial text and tool state with one completed assistant", async () => {
  const f = fixture();
  const client = chat(f.transport, [user, partial]);
  assert.equal(f.transport.resumeStreamIsReplay, true);
  for (let attempt = 0; attempt < 3; attempt++) {
    await client.resumeStream();
    assertSingleCompleteAssistant(client);
  }
  assert.equal(f.appCalls.length, 3);
  assert.equal(
    f.planeCalls.filter((call) => call.path.endsWith("/stream")).length,
    3,
  );
});

test("native network-disconnect state is rebuilt by reconnecting to the same managed run", async () => {
  const f = fixture();
  const prefix: UIMessageChunk[] = [
    { type: "start", messageId },
    { type: "start-step" },
    { type: "text-start", id: "text-one" },
    { type: "text-delta", id: "text-one", delta: "Hello " },
    {
      type: "tool-input-start",
      toolCallId: "call-one",
      toolName: "createNote",
    },
    {
      type: "tool-input-delta",
      toolCallId: "call-one",
      inputTextDelta: '{"title":"A',
    },
  ];
  const transport = new DefaultChatTransport({
    api: "http://application/api/chat",
    fetch: async (url, init) => {
      if (init?.method === "GET") return f.appFetch(url, init);
      // Emulate a browser losing an already-created run's response mid-delivery.
      let index = 0;
      return new Response(
        new ReadableStream({
          pull(output) {
            if (index < prefix.length)
              output.enqueue(
                encoder.encode(`data: ${JSON.stringify(prefix[index++])}\n\n`),
              );
            else output.error(new TypeError("network response failed"));
          },
        }),
        { headers: { "Content-Type": "text/event-stream" } },
      );
    },
  });
  const client = chat(transport, []);
  await client.sendMessage({ text: "Create a note" });
  assert.equal(client.status, "error");
  const unfinished = client.messages.at(-1)!;
  assert.equal(unfinished.id, messageId);
  const text = unfinished.parts.find((part) => part.type === "text");
  assert.ok(text?.type === "text" && text.text === "Hello ");
  const tool = unfinished.parts.find((part) => part.type === "tool-createNote");
  assert.ok(tool && "state" in tool && tool.state === "input-streaming");
  await client.resumeStream();
  assertSingleCompleteAssistant(client);
  await client.resumeStream();
  assertSingleCompleteAssistant(client);
});

test("native resume handles an app's no-active-run 204 without changing saved messages", async () => {
  const transport = new DefaultChatTransport({
    fetch: async (_url, init) => {
      assert.equal(init?.method, "GET");
      return new Response(null, { status: 204 });
    },
  });
  const client = chat(transport, [user, partial]);
  await client.resumeStream();
  assert.equal(client.status, "ready");
  assert.deepEqual(client.messages, [user, partial]);
});

test("unavailable managed replay becomes a native chat error instead of a new run or false finish", async () => {
  const f = fixture(false);
  const client = chat(f.transport, [user, partial]);
  await client.resumeStream();
  assert.equal(client.status, "error");
  assert.match(client.error?.message ?? "", /replay is unavailable/i);
  assert.deepEqual(client.messages, [user, partial]);
  assert.ok(f.planeCalls.every((call) => call.method === "GET"));
});
