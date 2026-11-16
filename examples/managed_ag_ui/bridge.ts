/** App-side AG-UI text transport over the existing managed-run bridge. */
import { EventType, PROTOCOL_VERSION, type Event } from "@ag-ui/core";
import { RunAgentInputSchema } from "@ag-ui/core/schemas";
import { EventEncoder } from "@ag-ui/encoder";
import {
  attachManagedChatChunks,
  createManagedChatChunks,
  type ManagedChatAttachOptions,
  type ManagedChatOptions,
  type ManagedChatChunks,
} from "../managed_ai_sdk/bridge.ts";

export interface AGUIIdentity {
  /** These AG-UI application IDs must come from an authenticated stored mapping. */
  aguiThreadId: string;
  aguiRunId: string;
}

export type ManagedAGUIOptions = Omit<
  ManagedChatOptions,
  "disconnectPolicy" | "runId"
> &
  AGUIIdentity & { runId: string };
export type ManagedAGUIAttachOptions = Omit<
  ManagedChatAttachOptions,
  "disconnectPolicy"
> &
  AGUIIdentity;

export class AGUIInputError extends Error {}

function validText(value: string): boolean {
  for (const character of value) {
    const code = character.codePointAt(0)!;
    if (code >= 0xd800 && code <= 0xdfff) return false;
  }
  return true;
}

function identity(options: AGUIIdentity): AGUIIdentity {
  for (const value of [options.aguiThreadId, options.aguiRunId]) {
    if (
      typeof value !== "string" ||
      !value ||
      value !== value.trim() ||
      Buffer.byteLength(value) > 256 ||
      !validText(value) ||
      Array.from(value).some((character) => {
        const code = character.codePointAt(0)!;
        return code <= 0x1f || code === 0x7f;
      })
    )
      throw new Error("Invalid stored AG-UI identity");
  }
  return { aguiThreadId: options.aguiThreadId, aguiRunId: options.aguiRunId };
}

function acceptsSSE(request: Request): boolean {
  const accept = request.headers.get("accept");
  if (!accept) return true;
  const ranges = accept.split(",").map((part) => {
    const [media, ...parameters] = part.trim().toLowerCase().split(";");
    const quality = parameters.find((parameter) =>
      /^q\s*=/u.test(parameter.trim()),
    );
    const value =
      quality === undefined
        ? 1
        : Number(quality.slice(quality.indexOf("=") + 1).trim());
    return {
      media: media.trim(),
      acceptable: Number.isFinite(value) && value > 0 && value <= 1,
    };
  });
  // An explicit exclusion wins over less specific wildcards.
  for (const media of ["text/event-stream", "text/*", "*/*"]) {
    const matching = ranges.filter((range) => range.media === media);
    if (matching.length) return matching.some((range) => range.acceptable);
  }
  return false;
}

export function requireAGUISSE(request: Request): void {
  if (!acceptsSSE(request))
    throw new AGUIInputError("This adapter supports AG-UI SSE only");
}

function emptyObject(value: unknown): boolean {
  return (
    typeof value === "object" &&
    value !== null &&
    !Array.isArray(value) &&
    Object.keys(value).length === 0
  );
}

async function readInput(
  request: Request,
  timeoutMs: number,
): Promise<unknown> {
  if (!request.body) throw new AGUIInputError("A text run input is required");
  const signal = AbortSignal.any([
    request.signal,
    AbortSignal.timeout(timeoutMs),
  ]);
  const reader = request.body.getReader();
  const decoder = new TextDecoder("utf-8", { fatal: true });
  let bytes = 0;
  let text = "";
  const abort = () => {
    void reader.cancel().catch(() => {});
  };
  signal.addEventListener("abort", abort, { once: true });
  try {
    for (;;) {
      if (signal.aborted) throw signal.reason;
      const next = await reader.read();
      if (signal.aborted) throw signal.reason;
      if (next.done) break;
      bytes += next.value.byteLength;
      if (bytes > 64 * 1024)
        throw new AGUIInputError("AG-UI input exceeds 64 KiB");
      text += decoder.decode(next.value, { stream: true });
    }
    text += decoder.decode();
    return JSON.parse(text);
  } catch (error) {
    if (signal.aborted) throw signal.reason;
    if (error instanceof AGUIInputError) throw error;
    throw new AGUIInputError("Invalid AG-UI JSON input");
  } finally {
    signal.removeEventListener("abort", abort);
    void reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

/** Only the final new user text enters Runkite. Client history is not native
 * checkpoint state, and no supplied tool, context, resume or state is executed.
 */
export async function createManagedAGUIResponse(
  request: Request,
  options: ManagedAGUIOptions,
): Promise<Response> {
  if (request.method !== "POST")
    throw new AGUIInputError("AG-UI creation requires POST");
  requireAGUISSE(request);
  const bound = identity(options);
  const parsed = RunAgentInputSchema.strict().safeParse(
    await readInput(request, options.timeoutMs ?? 10_000),
  );
  if (!parsed.success) throw new AGUIInputError("Unsupported AG-UI run input");
  const input = parsed.data;
  if (input.threadId !== bound.aguiThreadId || input.runId !== bound.aguiRunId)
    throw new AGUIInputError("AG-UI input does not match the owned turn");
  if (
    (input.protocolVersion !== undefined &&
      input.protocolVersion !== PROTOCOL_VERSION) ||
    input.parentRunId !== undefined ||
    input.tools.length ||
    input.context.length ||
    (input.resume !== undefined && input.resume.length) ||
    (input.state !== undefined && !emptyObject(input.state)) ||
    (input.forwardedProps !== undefined && !emptyObject(input.forwardedProps))
  )
    throw new AGUIInputError(
      "This adapter accepts new text turns without tools, state or resume",
    );
  const message = input.messages.at(-1);
  if (
    message?.role !== "user" ||
    typeof message.content !== "string" ||
    !message.content.trim() ||
    !validText(message.content) ||
    Object.keys(message).some((key) => !["id", "role", "content"].includes(key))
  )
    throw new AGUIInputError("The latest message must be plain user text");
  const managed = new Request(request.url, {
    method: "POST",
    signal: request.signal,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      messages: [
        { role: "user", parts: [{ type: "text", text: message.content }] },
      ],
    }),
  });
  return aguiResponse(
    await createManagedChatChunks(managed, {
      ...options,
      disconnectPolicy: "detach",
    }),
    bound,
  );
}

/** Full retained replay, not a cursor-based append. Reconstruct one owned
 * assistant response separately and replace it only after confirmed success.
 */
export async function attachManagedAGUIResponse(
  request: Request,
  options: ManagedAGUIAttachOptions,
): Promise<Response> {
  if (request.method !== "GET")
    throw new AGUIInputError("AG-UI attachment requires GET");
  requireAGUISSE(request);
  const bound = identity(options);
  return aguiResponse(
    await attachManagedChatChunks(request, {
      ...options,
      disconnectPolicy: "detach",
    }),
    bound,
  );
}

function aguiResponse(
  source: ManagedChatChunks,
  bound: AGUIIdentity,
): Response {
  const encoder = new EventEncoder({ accept: "text/event-stream" });
  const reader = source.stream.getReader();
  let ended = false;
  let cancelled = false;
  let releasing: Promise<void> | undefined;
  const release = () =>
    (releasing ??= (async () => {
      await reader.cancel().catch(() => {});
      reader.releaseLock();
    })());
  let started = false;
  let messageId: string | undefined;
  let partId: string | undefined;
  let textClosed = false;
  const run = { threadId: bound.aguiThreadId, runId: bound.aguiRunId };
  const errorEvent = (code: string, message: string): Event => ({
    type: EventType.RUN_ERROR,
    code,
    message,
  });
  const stream = new ReadableStream<Uint8Array>({
    async pull(output) {
      const emit = (event: Event) =>
        output.enqueue(encoder.encodeBinary(event));
      try {
        while (!ended) {
          const next = await reader.read();
          if (ended) return;
          if (next.done) throw new Error("incomplete");
          const chunk = next.value;
          if (chunk.type === "error" || chunk.type === "abort") {
            emit(
              errorEvent(
                chunk.type === "abort"
                  ? "RUNKITE_DELIVERY_ABORTED"
                  : "RUNKITE_DELIVERY_FAILED",
                chunk.type === "abort"
                  ? "Managed delivery ended; inspect the owned run because execution may continue"
                  : "Managed delivery failed; inspect the owned Runkite run",
              ),
            );
            ended = true;
          } else if (chunk.type === "start") {
            if (started || !chunk.messageId) throw new Error("unsupported");
            started = true;
            messageId = chunk.messageId;
            emit({
              type: EventType.RUN_STARTED,
              ...run,
              protocolVersion: PROTOCOL_VERSION,
            });
            return;
          } else if (!started) throw new Error("unsupported");
          else if (chunk.type === "start-step" || chunk.type === "finish-step")
            continue;
          else if (chunk.type === "text-start") {
            if (partId || textClosed) throw new Error("unsupported");
            partId = chunk.id;
            emit({
              type: EventType.TEXT_MESSAGE_START,
              messageId: messageId!,
              role: "assistant",
            });
            return;
          } else if (chunk.type === "text-delta") {
            if (!partId || partId !== chunk.id || textClosed)
              throw new Error("unsupported");
            emit({
              type: EventType.TEXT_MESSAGE_CONTENT,
              messageId: messageId!,
              delta: chunk.delta,
            });
            return;
          } else if (chunk.type === "text-end") {
            if (!partId || partId !== chunk.id || textClosed)
              throw new Error("unsupported");
            textClosed = true;
            emit({ type: EventType.TEXT_MESSAGE_END, messageId: messageId! });
            return;
          } else if (chunk.type === "finish") {
            if (!partId || !textClosed) throw new Error("unsupported");
            // The shared bridge withholds finish until durable completion and
            // matching attempt identity. A native text-end alone is insufficient.
            emit({
              type: EventType.RUN_FINISHED,
              ...run,
              outcome: { type: "success" },
            });
            ended = true;
          } else throw new Error("unsupported");
        }
      } catch {
        if (!ended)
          emit(
            errorEvent(
              "RUNKITE_UNSUPPORTED_OR_INCOMPLETE_STREAM",
              "Unsupported or incomplete managed text stream; reload the durable result",
            ),
          );
        ended = true;
      }
      if (ended) {
        await release();
        if (!cancelled) output.close();
      }
    },
    async cancel() {
      ended = true;
      cancelled = true;
      await release();
    },
  });
  return new Response(stream, {
    headers: {
      "Content-Type": encoder.getContentType(),
      "Cache-Control": "no-store",
      "X-Accel-Buffering": "no",
    },
  });
}
