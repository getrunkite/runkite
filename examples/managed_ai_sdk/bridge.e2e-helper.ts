/** Called by the runner's live control-plane test after it creates a fresh thread. */
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
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
} from "./bridge.ts";

type LiveBridgeOptions = {
  baseUrl: string;
  threadId: string;
  agentId: string;
  apiKey?: string;
};

export async function verifyManagedBridge(
  options: LiveBridgeOptions,
): Promise<void> {
  const runId = randomUUID();
  const apiKey = options.apiKey ?? "local-e2e-key";
  const client = new DefaultChatTransport({
    api: "http://application/api/chat",
    fetch: async (url, init) => {
      const response = await createManagedChatResponse(
        new Request(String(url), init),
        {
          baseUrl: options.baseUrl,
          apiKey,
          threadId: options.threadId,
          agentId: options.agentId,
          runId,
        },
      );
      assert.equal(response.headers.get("x-runkite-run-id"), runId);
      assert.equal(
        JSON.stringify([...response.headers]).includes(apiKey),
        false,
      );
      return response;
    },
  });
  const raw: UIMessageChunk[] = [];
  const stream = (
    await client.sendMessages({
      chatId: "application-owned-chat",
      trigger: "submit-message",
      messageId: undefined,
      messages: [
        {
          id: "new-user-message",
          role: "user",
          parts: [{ type: "text", text: "Create a note" }],
        },
      ],
      abortSignal: undefined,
    })
  ).pipeThrough(
    new TransformStream({
      transform(chunk, output) {
        raw.push(chunk);
        output.enqueue(chunk);
      },
    }),
  );
  let message: UIMessage | undefined;
  for await (const current of readUIMessageStream({ stream }))
    message = current;
  assert.equal(message?.id, `${runId}:g1:assistant`);
  assert.equal(
    message?.parts
      .filter((part) => part.type === "text")
      .map((part) => part.text)
      .join(""),
    "The answer",
  );
  const tool = message?.parts.find((part) => part.type === "tool-createNote");
  assert.ok(tool && "state" in tool && tool.state === "output-available");
  assert.deepEqual(tool.input, { title: "The original note" });
  assert.deepEqual(tool.output, { noteId: "note-one" });
  assert.equal(raw.filter((chunk) => chunk.type === "finish").length, 1);
  assert.equal(
    raw.some((chunk) => chunk.type === "error" || chunk.type === "abort"),
    false,
  );
  const durableResponse = await fetch(
    `${options.baseUrl}/threads/${encodeURIComponent(options.threadId)}/runs/${runId}`,
    {
      headers: { Authorization: `Bearer ${apiKey}` },
      signal: AbortSignal.timeout(10_000),
    },
  );
  assert.ok(durableResponse.ok);
  const durable = await durableResponse.json();
  assert.equal(durable.status, "success");
  assert.equal(durable.output.ai_sdk.message_id, message?.id);
  assert.ok(durable.output.usage.total_tokens > 0);
}

async function detachAfterText(
  source: ReadableStream<UIMessageChunk>,
  disconnect?: () => void,
): Promise<void> {
  const reader = source.getReader();
  try {
    for (;;) {
      const next = await reader.read();
      assert.equal(next.done, false, "The held model ended before detaching");
      assert.notEqual(next.value?.type, "error");
      assert.notEqual(next.value?.type, "abort");
      if (next.value?.type === "text-delta") {
        assert.ok(next.value.delta.length > 0);
        disconnect?.();
        return;
      }
    }
  } finally {
    await reader.cancel();
    reader.releaseLock();
  }
}

async function renderReplay(source: ReadableStream<UIMessageChunk>) {
  const chunks: UIMessageChunk[] = [];
  const stream = source.pipeThrough(
    new TransformStream({
      transform(chunk, output) {
        chunks.push(chunk);
        output.enqueue(chunk);
      },
    }),
  );
  let message: UIMessage | undefined;
  for await (const current of readUIMessageStream({ stream }))
    message = current;
  assert.equal(
    chunks.some((chunk) => chunk.type === "error" || chunk.type === "abort"),
    false,
    JSON.stringify(chunks),
  );
  assert.equal(chunks.filter((chunk) => chunk.type === "finish").length, 1);
  return message;
}

function assertReconnectedMessage(
  message: UIMessage | undefined,
  runId: string,
): void {
  assert.equal(message?.id, `${runId}:g1:assistant`);
  assert.equal(
    message?.parts
      .filter((part) => part.type === "text")
      .map((part) => part.text)
      .join(""),
    "The answer",
    "Full replay must not duplicate the previously rendered text",
  );
  const tool = message?.parts.find((part) => part.type === "tool-createNote");
  assert.ok(tool && "state" in tool && tool.state === "output-available");
  assert.deepEqual(tool.output, { noteId: "note-one" });
}

export async function verifyManagedReconnectBridge(
  options: LiveBridgeOptions & {
    assertDetached: (runId: string) => Promise<void>;
    release: (runId: string) => Promise<void>;
  },
): Promise<string> {
  const runId = randomUUID();
  const apiKey = options.apiKey ?? "local-e2e-key";
  const runOptions = {
    baseUrl: options.baseUrl,
    apiKey,
    threadId: options.threadId,
    runId,
  };
  const savedMessages = new Map<string, string>();
  let persistedStarts = 0;
  const onMessageStart = async (messageId: string) => {
    assert.equal(messageId, `${runId}:g1:assistant`);
    const saved = savedMessages.get(runId);
    if (saved !== undefined) assert.equal(messageId, saved);
    // Stand in for the app's awaited write to its server-owned turn record.
    await Promise.resolve();
    savedMessages.set(runId, messageId);
    persistedStarts++;
  };
  const planeCalls: Array<{ path: string; method: string }> = [];
  const fetcher: typeof fetch = async (url, init) => {
    planeCalls.push({
      path: new URL(String(url)).pathname,
      method: init?.method ?? "GET",
    });
    return fetch(url, init);
  };
  const client = new DefaultChatTransport({
    api: "http://application/api/chat",
    fetch: async (url, init) => {
      const request = new Request(String(url), init);
      if (request.method === "GET")
        assert.equal(savedMessages.get(runId), `${runId}:g1:assistant`);
      const response =
        request.method === "GET"
          ? await attachManagedChatResponse(request, {
              ...runOptions,
              fetch: fetcher,
              expectedMessageId: savedMessages.get(runId),
              onMessageStart,
            })
          : await createManagedChatResponse(request, {
              ...runOptions,
              agentId: options.agentId,
              disconnectPolicy: "detach",
              fetch: fetcher,
              onMessageStart,
            });
      assert.equal(response.headers.get("x-runkite-run-id"), runId);
      assert.equal(
        JSON.stringify([...response.headers]).includes(apiKey),
        false,
      );
      return response;
    },
  });
  const disconnected = new AbortController();
  await detachAfterText(
    await client.sendMessages({
      chatId: "application-owned-chat",
      trigger: "submit-message",
      messageId: undefined,
      messages: [
        {
          id: "one-user-turn",
          role: "user",
          parts: [{ type: "text", text: "Create one note" }],
        },
      ],
      abortSignal: disconnected.signal,
    }),
    () => {
      assert.equal(savedMessages.get(runId), `${runId}:g1:assistant`);
      assert.equal(
        persistedStarts,
        1,
        "Persist the verified assistant before rendering the first connection",
      );
      disconnected.abort(
        new Error("Browser disconnected while the model was running"),
      );
    },
  );
  await options.assertDetached(runId);

  const firstAttach = await client.reconnectToStream({
    chatId: "application-owned-chat",
  });
  assert.ok(
    firstAttach,
    "A running managed run must have an attachable stream",
  );
  await detachAfterText(firstAttach, () => {
    assert.equal(savedMessages.get(runId), `${runId}:g1:assistant`);
    assert.equal(
      persistedStarts,
      2,
      "The attached start must pass the same awaited identity hook",
    );
  });
  await options.assertDetached(runId);

  const secondAttach = await client.reconnectToStream({
    chatId: "application-owned-chat",
  });
  assert.ok(secondAttach);
  const completed = renderReplay(secondAttach);
  await options.release(runId);
  assertReconnectedMessage(await completed, runId);
  assert.equal(persistedStarts, 3);

  const completedAttach = await client.reconnectToStream({
    chatId: "application-owned-chat",
  });
  assert.ok(
    completedAttach,
    "A completed managed run must replay its retained UI events",
  );
  assertReconnectedMessage(await renderReplay(completedAttach), runId);
  assert.equal(persistedStarts, 4);
  assert.equal(
    planeCalls.filter(
      (call) => call.method === "POST" && call.path.endsWith("/runs"),
    ).length,
    1,
    "Attaching must never create another run",
  );
  assert.equal(
    planeCalls.filter((call) => call.path.endsWith("/cancel")).length,
    0,
    "Detaching either route must not cancel the managed run",
  );
  assert.equal(
    planeCalls.filter((call) => call.method !== "GET").length,
    1,
    "The original create is the only write; every attach must be read-only",
  );
  return runId;
}

export async function verifyManagedExplicitCancelBridge(
  options: LiveBridgeOptions,
): Promise<string> {
  const runId = randomUUID();
  const runOptions = {
    baseUrl: options.baseUrl,
    apiKey: options.apiKey ?? "local-e2e-key",
    threadId: options.threadId,
    runId,
  };
  let savedMessageId: string | undefined;
  const client = new DefaultChatTransport({
    api: "http://application/api/chat",
    fetch: (url, init) =>
      createManagedChatResponse(new Request(String(url), init), {
        ...runOptions,
        agentId: options.agentId,
        disconnectPolicy: "detach",
        onMessageStart: async (messageId) => {
          await Promise.resolve();
          savedMessageId = messageId;
        },
      }),
  });
  await detachAfterText(
    await client.sendMessages({
      chatId: "application-owned-chat",
      trigger: "submit-message",
      messageId: undefined,
      messages: [
        {
          id: "one-user-turn",
          role: "user",
          parts: [{ type: "text", text: "Wait until explicitly stopped" }],
        },
      ],
      abortSignal: undefined,
    }),
    () => assert.equal(savedMessageId, `${runId}:g1:assistant`),
  );
  const stillActive = await fetch(
    `${options.baseUrl}/threads/${encodeURIComponent(options.threadId)}/runs/${runId}`,
    {
      headers: { Authorization: `Bearer ${runOptions.apiKey}` },
      signal: AbortSignal.timeout(10_000),
    },
  );
  assert.ok(stillActive.ok);
  const run = await stillActive.json();
  assert.ok(
    ["pending", "running"].includes(run.status),
    "Detaching must leave the slow run active",
  );
  await cancelManagedChatRun(runOptions);
  return runId;
}
