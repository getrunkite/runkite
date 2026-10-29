/** Called by the runner's live control-plane test after it creates a fresh thread. */
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import {
  DefaultChatTransport,
  readUIMessageStream,
  type UIMessage,
  type UIMessageChunk,
} from "ai";
import { createManagedChatResponse } from "./bridge.ts";

export async function verifyManagedBridge(options: {
  baseUrl: string;
  threadId: string;
  agentId: string;
  apiKey?: string;
}): Promise<void> {
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
