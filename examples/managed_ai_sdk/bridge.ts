/** Server-side AI SDK UI bridge. Authentication and chat ownership belong to the app. */
import { randomUUID } from "node:crypto";
import {
  asSchema,
  createUIMessageStreamResponse,
  uiMessageChunkSchema,
  type UIMessageChunk,
} from "ai";

export interface ManagedChatOptions {
  baseUrl: string;
  apiKey: string;
  /** Resolve this from authenticated application storage, never the browser body. */
  threadId: string;
  agentId: string;
  /** Allocate and persist this server-side if your app needs request retry idempotency. */
  runId?: string;
  fetch?: typeof globalThis.fetch;
  /** HTTP setup and durable-completion timeout. Does not limit a running stream. */
  timeoutMs?: number;
  onCancelError?: () => void;
}

type Frame = { event: string; id: string; data: string };
const MAX_FRAME_BYTES = 1024 * 1024;
const MAX_REQUEST_BYTES = 64 * 1024;
const MAX_EVENTS = 20_000;

function identifier(value: string): string {
  if (
    typeof value !== "string" ||
    !value ||
    value !== value.trim() ||
    Buffer.byteLength(value) > 256
  ) {
    throw new Error("Invalid server-side Runkite identifier");
  }
  return value;
}

function checkAbort(signal: AbortSignal): void {
  if (signal.aborted)
    throw signal.reason ?? new DOMException("Aborted", "AbortError");
}

async function readBody(
  body: ReadableStream<Uint8Array> | null,
  limit: number,
): Promise<string> {
  if (!body) throw new Error("Missing HTTP response body");
  const reader = body.getReader();
  const parts: Uint8Array[] = [];
  let size = 0;
  try {
    for (;;) {
      const next = await reader.read();
      if (next.done) break;
      size += next.value.length;
      if (size > limit)
        throw new Error("HTTP payload exceeds the example's size limit");
      parts.push(next.value);
    }
    return new TextDecoder("utf-8", { fatal: true }).decode(
      Buffer.concat(parts),
    );
  } finally {
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

async function* frames(
  body: ReadableStream<Uint8Array>,
  signal: AbortSignal,
): AsyncGenerator<Frame> {
  const reader = body.getReader();
  const decoder = new TextDecoder("utf-8", { fatal: true });
  const abort = () => {
    void reader.cancel().catch(() => {});
  };
  signal.addEventListener("abort", abort, { once: true });
  let buffer = "";
  let event = "message";
  let id = "";
  let data: string[] = [];
  let size = 0;
  try {
    for (;;) {
      checkAbort(signal);
      const next = await reader.read();
      checkAbort(signal);
      buffer += decoder.decode(next.value, { stream: !next.done });
      let newline: number;
      while ((newline = buffer.indexOf("\n")) >= 0) {
        const line = buffer.slice(0, newline).replace(/\r$/, "");
        buffer = buffer.slice(newline + 1);
        size += Buffer.byteLength(line);
        if (size > MAX_FRAME_BYTES)
          throw new Error("Runkite SSE frame is too large");
        if (!line) {
          if (data.length) yield { event, id, data: data.join("\n") };
          event = "message";
          id = "";
          data = [];
          size = 0;
        } else if (!line.startsWith(":")) {
          const colon = line.indexOf(":");
          const field = colon < 0 ? line : line.slice(0, colon);
          const value =
            colon < 0 ? "" : line.slice(colon + 1).replace(/^ /, "");
          if (field === "event") event = value;
          if (field === "id") id = value;
          if (field === "data") data.push(value);
        }
      }
      if (Buffer.byteLength(buffer) + size > MAX_FRAME_BYTES)
        throw new Error("Runkite SSE frame is too large");
      if (next.done) return;
    }
  } finally {
    signal.removeEventListener("abort", abort);
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

/**
 * Use from an authenticated Next/Nuxt server route. The plane owns conversation
 * history. Only a new text user message enters this initial example.
 */
export async function createManagedChatResponse(
  request: Request,
  options: ManagedChatOptions,
): Promise<Response> {
  const threadId = identifier(options.threadId);
  const agentId = identifier(options.agentId);
  const runId = identifier(options.runId ?? randomUUID());
  const base = new URL(options.baseUrl);
  if (
    !/^https?:$/.test(base.protocol) ||
    base.username ||
    base.password ||
    base.search ||
    base.hash
  ) {
    throw new Error("Invalid server-side Runkite URL");
  }
  const timeoutMs = options.timeoutMs ?? 30_000;
  if (!Number.isSafeInteger(timeoutMs) || timeoutMs < 1)
    throw new Error("Invalid HTTP timeout");
  if (typeof options.apiKey !== "string" || !options.apiKey.trim())
    throw new Error("A server-side Runkite key is required");
  const fetcher = options.fetch ?? globalThis.fetch;
  const headers = {
    Authorization: `Bearer ${options.apiKey}`,
    "Content-Type": "application/json",
  };
  const root = `${base.href.replace(/\/$/, "")}/threads/${encodeURIComponent(threadId)}`;
  const runUrl = `${root}/runs/${encodeURIComponent(runId)}`;
  checkAbort(request.signal);
  const input = JSON.parse(await readBody(request.body, MAX_REQUEST_BYTES));
  if (
    !input ||
    typeof input !== "object" ||
    input.trigger === "regenerate-message" ||
    !Array.isArray(input.messages)
  ) {
    throw new Error("This example accepts new user messages only");
  }
  const message = input.messages.at(-1);
  if (
    message?.role !== "user" ||
    !Array.isArray(message.parts) ||
    !message.parts.length ||
    message.parts.some(
      (part: unknown) =>
        !part ||
        typeof part !== "object" ||
        (part as { type?: unknown }).type !== "text" ||
        typeof (part as { text?: unknown }).text !== "string",
    )
  ) {
    throw new Error("This example accepts text user messages only");
  }
  const text = message.parts
    .map((part: { text: string }) => part.text)
    .join("");
  if (!text.trim()) throw new Error("The new user message is empty");
  for (const character of text) {
    const code = character.codePointAt(0)!;
    if (code >= 0xd800 && code <= 0xdfff)
      throw new Error("The new user message has invalid Unicode");
  }
  checkAbort(request.signal);

  let created = false;
  let creationStarted = false;
  let creationResponseReceived = false;
  let settled = false;
  let cancellation: Promise<void> | undefined;
  const controller = new AbortController();
  const signal = AbortSignal.any([request.signal, controller.signal]);
  const cancelRun = (): Promise<void> => {
    if (!created || settled) return Promise.resolve();
    cancellation ??= (async () => {
      const response = await fetcher(`${runUrl}/cancel`, {
        method: "POST",
        headers,
        redirect: "error",
        signal: AbortSignal.timeout(5_000),
      });
      await response.body?.cancel();
      if (!response.ok)
        throw new Error("Runkite cancellation was not confirmed");
    })().catch((error) => {
      if (options.onCancelError) options.onCancelError();
      else
        console.warn(
          "Runkite cancellation was not confirmed; inspect the managed run",
        );
      throw error;
    });
    return cancellation;
  };
  const abort = () => {
    void cancelRun().catch(() => {});
  };
  request.signal.addEventListener("abort", abort, { once: true });
  const cleanup = () => request.signal.removeEventListener("abort", abort);
  let response: Response;
  try {
    // Keep creation bounded but independent of browser disconnect. Once the
    // creation response arrives, a cancelled request explicitly cancels its run.
    creationStarted = true;
    const createdResponse = await fetcher(`${root}/runs`, {
      method: "POST",
      headers,
      redirect: "error",
      signal: AbortSignal.timeout(timeoutMs),
      body: JSON.stringify({
        run_id: runId,
        agent_id: agentId,
        input: { messages: [{ role: "user", content: text }] },
        stream_mode: ["custom:ai_sdk", "values"],
        multitask_strategy: "reject",
      }),
    });
    creationResponseReceived = true;
    if (!createdResponse.ok) {
      await createdResponse.body?.cancel();
      throw new Error(
        `Runkite run creation failed (${createdResponse.status})`,
      );
    }
    created = true;
    const run = JSON.parse(
      await readBody(createdResponse.body, MAX_FRAME_BYTES),
    );
    if (run.run_id !== runId || run.thread_id !== threadId)
      throw new Error("Unexpected managed run identity");
    if (signal.aborted) {
      await cancelRun();
      checkAbort(signal);
    }
    const connecting = new AbortController();
    const setupTimer = setTimeout(
      () => connecting.abort(new Error("Runkite stream setup timed out")),
      timeoutMs,
    );
    try {
      response = await fetcher(`${runUrl}/stream`, {
        headers,
        redirect: "error",
        signal: AbortSignal.any([signal, connecting.signal]),
      });
    } finally {
      clearTimeout(setupTimer);
    }
    if (
      !response.ok ||
      !response.headers.get("content-type")?.startsWith("text/event-stream") ||
      !response.body
    ) {
      await response.body?.cancel();
      throw new Error("Runkite did not return an event stream");
    }
  } catch (error) {
    // A lost HTTP response does not prove the run was never created. Cancel
    // the application-allocated ID best-effort and preserve the original error.
    if (creationStarted && !creationResponseReceived) created = true;
    cleanup();
    await cancelRun().catch(() => {});
    throw error;
  }

  async function* chunks(): AsyncGenerator<UIMessageChunk> {
    const seen = new Set<string>();
    let generation: number | undefined;
    let finish: Extract<UIMessageChunk, { type: "finish" }> | undefined;
    let nativeError: Extract<UIMessageChunk, { type: "error" }> | undefined;
    let nativeAbort: Extract<UIMessageChunk, { type: "abort" }> | undefined;
    const validate = asSchema(uiMessageChunkSchema).validate!;
    try {
      for await (const frame of frames(response.body!, signal)) {
        if (frame.id) {
          if (Buffer.byteLength(frame.id) > 512)
            throw new Error("Runkite event ID is too large");
          if (seen.has(frame.id)) continue;
          if (seen.size >= MAX_EVENTS)
            throw new Error("Runkite stream exceeds the example's event limit");
          seen.add(frame.id);
        }
        if (frame.event === "custom:ai_sdk") {
          if (!frame.id)
            throw new Error("AI SDK events require a Runkite event ID");
          const attempt = Number(frame.id.match(/:g(\d+)$/)?.[1] ?? "1");
          if (!Number.isSafeInteger(attempt) || attempt < 1)
            throw new Error("Invalid runner attempt");
          if (generation !== undefined && attempt < generation) continue;
          if (generation !== undefined && attempt !== generation) {
            throw new Error(
              "The managed run changed attempts; reload its durable result",
            );
          }
          generation = attempt;
          const parsed = await validate(JSON.parse(frame.data));
          if (!parsed.success)
            throw new Error("Invalid AI SDK UI stream event");
          const chunk = parsed.value;
          if (
            chunk.type === "start" &&
            chunk.messageId !== `${runId}:g${generation}:assistant`
          ) {
            throw new Error("Unexpected managed assistant message identity");
          }
          if (chunk.type === "finish") finish = chunk;
          else if (chunk.type === "error") nativeError = chunk;
          else if (chunk.type === "abort") nativeAbort = chunk;
          else yield chunk;
        } else if (frame.event === "end" || frame.event === "error") {
          // Raw SSE is advisory during reclaim. Confirm the durable result before
          // letting useChat report a successful completion.
          const durable = await fetcher(`${runUrl}/wait`, {
            headers,
            redirect: "error",
            signal: AbortSignal.any([signal, AbortSignal.timeout(timeoutMs)]),
          });
          if (!durable.ok) {
            await durable.body?.cancel();
            throw new Error("Managed run completion could not be confirmed");
          }
          const result = JSON.parse(
            await readBody(durable.body, MAX_FRAME_BYTES),
          );
          if (
            result.run?.run_id !== runId ||
            result.run?.thread_id !== threadId
          )
            throw new Error("Unexpected completion identity");
          if (
            !["success", "error", "timeout", "interrupted"].includes(
              result.run.status,
            )
          ) {
            throw new Error("The managed run is not durably complete");
          }
          settled = true;
          if (result.run.status === "interrupted") {
            yield nativeAbort ?? {
              type: "abort",
              reason: "Managed run interrupted",
            };
            return;
          }
          if (result.run.status !== "success") {
            yield nativeError ?? {
              type: "error",
              errorText: "Managed run failed",
            };
            return;
          }
          if (
            !finish ||
            nativeAbort ||
            nativeError ||
            generation === undefined ||
            result.values?.ai_sdk?.generation !== generation ||
            result.values?.ai_sdk?.message_id !==
              `${runId}:g${generation}:assistant`
          ) {
            throw new Error(
              "The UI stream does not match the durable managed result",
            );
          }
          yield finish;
          return;
        }
      }
      throw new Error("The Runkite stream ended without confirmed completion");
    } finally {
      cleanup();
    }
  }

  const iterator = chunks();
  let closed = false;
  const stream = new ReadableStream<UIMessageChunk>({
    async pull(output) {
      try {
        const next = await iterator.next();
        if (closed) return;
        if (next.done) {
          closed = true;
          output.close();
        } else output.enqueue(next.value);
      } catch {
        await cancelRun().catch(() => {});
        if (closed) return;
        if (signal.aborted)
          output.enqueue({
            type: "abort",
            reason: "Application request cancelled",
          });
        else
          output.enqueue({
            type: "error",
            errorText: "Managed chat delivery failed; inspect the Runkite run",
          });
        output.close();
        closed = true;
        cleanup();
      }
    },
    async cancel() {
      closed = true;
      controller.abort();
      cleanup();
      await cancelRun();
      await iterator.return(undefined);
    },
  });
  return createUIMessageStreamResponse({
    stream,
    headers: { "X-Runkite-Run-Id": runId },
  });
}
