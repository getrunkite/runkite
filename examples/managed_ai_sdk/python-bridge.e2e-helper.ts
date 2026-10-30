/** Opt-in native UI transport against an installed Python worker and real plane. */
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { pathToFileURL } from "node:url";
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
  type ManagedChatOptions,
} from "./bridge.ts";

type PythonProfile = Exclude<
  NonNullable<ManagedChatOptions["profile"]>,
  "ai-sdk"
>;

type Options = {
  baseUrl: string;
  apiKey: string;
  profile: PythonProfile;
  agentId: string;
  slowAgentId: string;
  typedAgentId?: string;
  tenantId: string;
  recovered: { threadId: string; runId: string; generation: number };
};

type Rendered = { message: UIMessage | undefined; chunks: UIMessageChunk[] };

async function render(
  stream: ReadableStream<UIMessageChunk>,
): Promise<Rendered> {
  const chunks: UIMessageChunk[] = [];
  const observed = stream.pipeThrough(
    new TransformStream({
      transform(chunk, output) {
        chunks.push(chunk);
        output.enqueue(chunk);
      },
    }),
  );
  let message: UIMessage | undefined;
  for await (const current of readUIMessageStream({ stream: observed }))
    message = current;
  return { message, chunks };
}

function successful(result: Rendered): void {
  assert.equal(
    result.chunks.some(
      (chunk) => chunk.type === "error" || chunk.type === "abort",
    ),
    false,
    JSON.stringify(result.chunks),
  );
  assert.equal(
    result.chunks.filter((chunk) => chunk.type === "finish").length,
    1,
  );
  assert.equal(
    result.message?.parts.some((part) => part.type.startsWith("tool-")),
    false,
    "The text bridge must not invent native tool UI",
  );
}

function text(message: UIMessage | undefined): string {
  return (
    message?.parts
      .filter((part) => part.type === "text")
      .map((part) => part.text)
      .join("") ?? ""
  );
}

async function detachAfterText(
  stream: ReadableStream<UIMessageChunk>,
): Promise<UIMessageChunk[]> {
  const reader = stream.getReader();
  const chunks: UIMessageChunk[] = [];
  try {
    for (;;) {
      const next = await reader.read();
      assert.equal(next.done, false, "The slow fixture ended before detach");
      assert.ok(next.value);
      chunks.push(next.value);
      assert.notEqual(next.value.type, "error", JSON.stringify(chunks));
      assert.notEqual(next.value.type, "abort", JSON.stringify(chunks));
      assert.notEqual(next.value.type, "finish", JSON.stringify(chunks));
      if (next.value.type === "text-delta") {
        assert.equal(next.value.delta, "Waiting");
        return chunks;
      }
    }
  } finally {
    await reader.cancel();
    reader.releaseLock();
  }
}

export async function verifyManagedPythonBridge(options: Options) {
  const headers = { Authorization: `Bearer ${options.apiKey}` };
  const calls: Array<{ method: string; path: string }> = [];
  const observedFetch: typeof fetch = async (url, init) => {
    calls.push({
      method: init?.method ?? "GET",
      path: new URL(String(url)).pathname,
    });
    return fetch(url, init);
  };
  const request = async (path: string, body?: unknown) => {
    const response = await fetch(options.baseUrl + path, {
      method: body === undefined ? "GET" : "POST",
      headers: { ...headers, "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(20_000),
    });
    assert.ok(response.ok, `${path}: HTTP ${response.status}`);
    return response.json();
  };
  const newThread = async () =>
    (await request("/threads", {})).thread_id as string;
  const runOptions = (threadId: string, runId: string) => ({
    baseUrl: options.baseUrl,
    apiKey: options.apiKey,
    profile: options.profile,
    threadId,
    runId,
    fetch: observedFetch,
    timeoutMs: 20_000,
  });
  const ownedMessages = new Map<string, string>();
  const starts = new Map<string, number>();
  const persistStart = (runId: string) => async (messageId: string) => {
    const existing = ownedMessages.get(runId);
    if (existing !== undefined) assert.equal(messageId, existing);
    await Promise.resolve();
    ownedMessages.set(runId, messageId);
    starts.set(runId, (starts.get(runId) ?? 0) + 1);
  };
  const client = (threadId: string, runId: string, agentId: string) =>
    new DefaultChatTransport({
      api: "http://application/api/chat",
      fetch: async (url, init) => {
        const request = new Request(String(url), init);
        const response =
          request.method === "GET"
            ? await attachManagedChatResponse(request, {
                ...runOptions(threadId, runId),
                expectedMessageId: ownedMessages.get(runId),
                onMessageStart: persistStart(runId),
              })
            : await createManagedChatResponse(request, {
                ...runOptions(threadId, runId),
                agentId,
                disconnectPolicy: "detach",
                onMessageStart: persistStart(runId),
              });
        assert.equal(response.headers.get("x-runkite-run-id"), runId);
        assert.equal(
          JSON.stringify([...response.headers]).includes(options.apiKey),
          false,
        );
        return response;
      },
    });
  const send = async (
    transport: DefaultChatTransport<UIMessage>,
    prompt: string,
  ) =>
    transport.sendMessages({
      chatId: "application-owned-chat",
      trigger: "submit-message",
      messageId: undefined,
      messages: [
        {
          id: randomUUID(),
          role: "user",
          parts: [{ type: "text", text: prompt }],
        },
      ],
      abortSignal: AbortSignal.timeout(25_000),
    });

  const threadId = await newThread();
  const firstRunId = randomUUID();
  const firstClient = client(threadId, firstRunId, options.agentId);
  const first = await render(await send(firstClient, "bridge first turn"));
  successful(first);
  assert.equal(first.message?.id, `${firstRunId}:g1:assistant`);
  assert.equal(text(first.message), "users=1; tool_results=1");
  assert.equal(starts.get(firstRunId), 1);
  const firstReplayStream = await firstClient.reconnectToStream({
    chatId: "application-owned-chat",
  });
  assert.ok(firstReplayStream);
  const firstReplay = await render(firstReplayStream);
  successful(firstReplay);
  assert.deepEqual(firstReplay.message, first.message);
  assert.equal(starts.get(firstRunId), 2);

  const secondRunId = randomUUID();
  const second = await render(
    await send(
      client(threadId, secondRunId, options.agentId),
      "bridge second turn",
    ),
  );
  successful(second);
  assert.equal(second.message?.id, `${secondRunId}:g1:assistant`);
  assert.equal(text(second.message), "users=2; tool_results=2");
  assert.notEqual(
    text(second.message),
    text(first.message),
    "Old assistant text cannot satisfy current-turn validation",
  );

  const recovered = options.recovered;
  assert.ok(recovered.generation >= 2);
  const recoveryTransport = new DefaultChatTransport({
    api: "http://application/api/chat",
    fetch: (url, init) =>
      attachManagedChatResponse(new Request(String(url), init), {
        ...runOptions(recovered.threadId, recovered.runId),
        onMessageStart: persistStart(recovered.runId),
      }),
  });
  const replay = await recoveryTransport.reconnectToStream({
    chatId: "application-owned-chat",
  });
  assert.ok(replay);
  const recoveredResult = await render(replay);
  successful(recoveredResult);
  assert.equal(
    recoveredResult.message?.id,
    `${recovered.runId}:g${recovered.generation}:assistant`,
  );
  assert.equal(text(recoveredResult.message), "users=1; tool_results=1");
  const mismatched = new DefaultChatTransport({
    api: "http://application/api/chat",
    fetch: (url, init) =>
      attachManagedChatResponse(new Request(String(url), init), {
        ...runOptions(recovered.threadId, recovered.runId),
        expectedMessageId: `${recovered.runId}:g1:assistant`,
      }),
  });
  const mismatchStream = await mismatched.reconnectToStream({
    chatId: "application-owned-chat",
  });
  assert.ok(mismatchStream);
  const mismatch = await render(mismatchStream);
  assert.equal(
    mismatch.chunks.filter((chunk) => chunk.type === "finish").length,
    0,
  );
  assert.equal(
    mismatch.chunks.filter((chunk) => chunk.type === "error").length,
    1,
  );

  const slowThreadId = await newThread();
  const slowRunId = randomUUID();
  const slowClient = client(slowThreadId, slowRunId, options.slowAgentId);
  try {
    await detachAfterText(
      await send(slowClient, "bridge detach then explicit stop"),
    );
    assert.equal(ownedMessages.get(slowRunId), `${slowRunId}:g1:assistant`);
    assert.equal(
      (await request(`/threads/${slowThreadId}/runs/${slowRunId}`)).status,
      "running",
    );
    const runningReplay = await slowClient.reconnectToStream({
      chatId: "application-owned-chat",
    });
    assert.ok(runningReplay);
    await detachAfterText(runningReplay);
    assert.equal(starts.get(slowRunId), 2);
    assert.equal(
      (await request(`/threads/${slowThreadId}/runs/${slowRunId}`)).status,
      "running",
    );
    assert.equal(
      calls.filter((call) => call.path.endsWith(`/${slowRunId}/cancel`)).length,
      0,
    );
    await cancelManagedChatRun(runOptions(slowThreadId, slowRunId));
    const stopped = await request(`/threads/${slowThreadId}/runs/${slowRunId}`);
    assert.equal(stopped.status, "interrupted");
    const stoppedReplayStream = await slowClient.reconnectToStream({
      chatId: "application-owned-chat",
    });
    assert.ok(stoppedReplayStream);
    const stoppedReplay = await render(stoppedReplayStream);
    assert.equal(
      stoppedReplay.chunks.filter((chunk) => chunk.type === "finish").length,
      0,
    );
    assert.equal(
      stoppedReplay.chunks.filter((chunk) => chunk.type === "abort").length,
      1,
    );
  } finally {
    await cancelManagedChatRun(runOptions(slowThreadId, slowRunId));
  }

  let typedRunId: string | undefined;
  if (options.typedAgentId) {
    typedRunId = randomUUID();
    const typedThreadId = await newThread();
    const typedClient = client(typedThreadId, typedRunId, options.typedAgentId);
    const typed = await render(
      await send(typedClient, "bridge typed-only result"),
    );
    successful(typed);
    assert.equal(text(typed.message), "");
    assert.equal(
      typed.chunks.filter((chunk) => chunk.type.startsWith("text-")).length,
      0,
    );
    assert.equal(
      typed.message?.parts.filter((part) => part.type === "data-result").length,
      1,
    );
    const result = typed.message?.parts.find(
      (part) => part.type === "data-result",
    );
    assert.ok(result && "data" in result);
    assert.deepEqual(result.data, {
      count: 7,
      note: "validated",
      tenant: options.tenantId,
    });
    const typedReplayStream = await typedClient.reconnectToStream({
      chatId: "application-owned-chat",
    });
    assert.ok(typedReplayStream);
    const typedReplay = await render(typedReplayStream);
    successful(typedReplay);
    assert.deepEqual(typedReplay.message, typed.message);
  }
  const creates = calls.filter(
    (call) => call.method === "POST" && call.path.endsWith("/runs"),
  );
  assert.equal(
    creates.length,
    options.typedAgentId ? 4 : 3,
    "Attach must never dispatch another run",
  );
  return {
    firstRunId,
    secondRunId,
    slowRunId,
    typedRunId,
    recoveredRunId: recovered.runId,
  };
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href
) {
  let input = "";
  for await (const part of process.stdin) input += part;
  const summary = await verifyManagedPythonBridge(JSON.parse(input) as Options);
  process.stdout.write(JSON.stringify(summary) + "\n");
}
