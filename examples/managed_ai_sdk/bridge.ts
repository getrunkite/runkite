/** Server-side AI SDK UI bridge. Authentication and chat ownership belong to the app. */
import { randomUUID } from "node:crypto";
import {
  asSchema,
  createUIMessageStreamResponse,
  uiMessageChunkSchema,
  type UIMessageChunk,
} from "ai";

export type ManagedChatDisconnectPolicy = "cancel" | "detach";
export type ManagedChatProfile =
  "ai-sdk" | "agent-framework" | "openai-agents" | "pydantic-ai" | "google-adk";

export interface ManagedRunOptions {
  baseUrl: string;
  apiKey: string;
  /** Resolve both IDs from authenticated application storage. */
  threadId: string;
  runId: string;
  fetch?: typeof globalThis.fetch;
  /** HTTP setup and durable-completion timeout. Does not limit a running stream. */
  timeoutMs?: number;
}

export interface ManagedChatAttachOptions extends ManagedRunOptions {
  /** Select from server-owned agent configuration, never browser input. */
  profile?: ManagedChatProfile;
  /** Attach defaults to detach, so a read does not cancel an existing run. */
  disconnectPolicy?: ManagedChatDisconnectPolicy;
  onCancelError?: () => void;
  /** Persisted assistant identity when resuming an already displayed response. */
  expectedMessageId?: string;
  /** Bind unset-or-equal identity on this stored turn before displaying start. */
  onMessageStart?: (
    messageId: string,
    signal: AbortSignal,
  ) => void | Promise<void>;
}

export interface ManagedChatOptions extends Omit<
  ManagedChatAttachOptions,
  "runId"
> {
  agentId: string;
  /** Required for detach: persist the ID before creating the run. */
  runId?: string;
}

class ReplayUnavailableError extends Error {
  constructor() {
    super("Managed chat replay is unavailable; reload saved messages");
  }
}

class AttemptChangedError extends Error {
  constructor() {
    super("The managed run changed attempts; reload its durable result");
  }
}

type Frame = { event: string; id: string; data: string };
const MAX_FRAME_BYTES = 1024 * 1024;
const MAX_REQUEST_BYTES = 64 * 1024;
const MAX_EVENTS = 20_000;

const PROFILES: Record<ManagedChatProfile, string> = {
  "ai-sdk": "ai_sdk",
  "agent-framework": "agent_framework",
  "openai-agents": "openai_agents",
  "pydantic-ai": "pydantic_ai",
  "google-adk": "google_adk",
};

function record(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function validText(value: unknown): value is string {
  if (typeof value !== "string") return false;
  for (const character of value) {
    const code = character.codePointAt(0)!;
    if (code >= 0xd800 && code <= 0xdfff) return false;
  }
  return true;
}

function pythonDelta(value: unknown, runId: string): string {
  if (
    !Array.isArray(value) ||
    value.length !== 2 ||
    !record(value[0]) ||
    !record(value[1]) ||
    Object.keys(value[1]).length ||
    Object.keys(value[0]).some(
      (key) => !["id", "type", "content"].includes(key),
    ) ||
    value[0].id !== `${runId}:assistant` ||
    value[0].type !== "ai" ||
    !validText(value[0].content)
  )
    throw new Error("Unsupported managed Python message chunk");
  return value[0].content;
}

function validateProfile(
  value: unknown,
  profile: ManagedChatProfile,
  generation: number,
): asserts value is Record<string, unknown> {
  const metadata = record(value) ? value[PROFILES[profile]] : undefined;
  if (
    !record(metadata) ||
    metadata.framework !== profile ||
    metadata.generation !== generation
  )
    throw new Error(
      "The managed result does not match the selected profile and attempt",
    );
}

function currentTurnText(
  value: unknown,
  input: unknown,
  expectedInput?: string,
): string {
  if (
    !record(value) ||
    !Array.isArray(value.messages) ||
    !record(input) ||
    Object.keys(input).some((key) => key !== "messages") ||
    !Array.isArray(input.messages) ||
    input.messages.length !== 1
  )
    throw new Error("Unsupported durable managed turn");
  const incoming = input.messages[0];
  if (
    !record(incoming) ||
    incoming.role !== "user" ||
    !validText(incoming.content) ||
    !incoming.content.trim() ||
    (expectedInput !== undefined && incoming.content !== expectedInput)
  )
    throw new Error("Unexpected durable user input");
  const messages = value.messages;
  const final = messages.at(-1);
  const assistant = record(final) && final.role === "assistant";
  const user = messages.at(assistant ? -2 : -1);
  if (
    !record(user) ||
    user.role !== "user" ||
    user.content !== incoming.content ||
    (assistant && !validText(final.content))
  )
    throw new Error("The managed result has no matching current turn");
  return assistant ? (final.content as string) : "";
}

function canonicalJSON(value: unknown, depth = 0): string {
  if (depth > 64)
    throw new Error("Structured result exceeds the example's depth limit");
  if (value === null || typeof value === "boolean")
    return JSON.stringify(value);
  if (typeof value === "number") {
    if (
      !Number.isFinite(value) ||
      (Number.isInteger(value) && !Number.isSafeInteger(value))
    )
      throw new Error(
        "Structured result numbers must be finite and integers must be safe",
      );
    return JSON.stringify(value);
  }
  if (validText(value)) return JSON.stringify(value);
  if (Array.isArray(value))
    return `[${value.map((item) => canonicalJSON(item, depth + 1)).join(",")}]`;
  if (record(value))
    return `{${Object.keys(value)
      .sort()
      .map((key) => {
        if (!validText(key))
          throw new Error("Structured result has invalid Unicode");
        return `${JSON.stringify(key)}:${canonicalJSON(value[key], depth + 1)}`;
      })
      .join(",")}}`;
  throw new Error("Structured result must be finite JSON");
}

function identifier(value: string): string {
  if (
    typeof value !== "string" ||
    !value ||
    value === "." ||
    value === ".." ||
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

async function abortable<T>(
  operation: () => T | Promise<T>,
  signal: AbortSignal,
): Promise<T> {
  checkAbort(signal);
  let abort!: () => void;
  const interrupted = new Promise<never>((_, reject) => {
    abort = () =>
      reject(signal.reason ?? new DOMException("Aborted", "AbortError"));
    signal.addEventListener("abort", abort, { once: true });
  });
  try {
    return await Promise.race([Promise.resolve().then(operation), interrupted]);
  } finally {
    signal.removeEventListener("abort", abort);
  }
}

async function readBody(
  body: ReadableStream<Uint8Array> | null,
  limit: number,
  signal?: AbortSignal,
): Promise<string> {
  if (!body) throw new Error("Missing HTTP response body");
  const reader = body.getReader();
  const parts: Uint8Array[] = [];
  let size = 0;
  const abort = () => {
    void reader.cancel(signal?.reason).catch(() => {});
  };
  signal?.addEventListener("abort", abort, { once: true });
  try {
    for (;;) {
      if (signal) checkAbort(signal);
      const next = await reader.read();
      if (signal) checkAbort(signal);
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
    signal?.removeEventListener("abort", abort);
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

function managedConnection(
  options: ManagedRunOptions &
    Pick<
      ManagedChatAttachOptions,
      | "profile"
      | "disconnectPolicy"
      | "onCancelError"
      | "expectedMessageId"
      | "onMessageStart"
    >,
  defaultPolicy: ManagedChatDisconnectPolicy,
) {
  const threadId = identifier(options.threadId);
  const runId = identifier(options.runId);
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
  const disconnectPolicy = options.disconnectPolicy ?? defaultPolicy;
  if (disconnectPolicy !== "cancel" && disconnectPolicy !== "detach")
    throw new Error("Invalid managed chat disconnect policy");
  const profile = options.profile ?? "ai-sdk";
  if (typeof profile !== "string" || !Object.hasOwn(PROFILES, profile))
    throw new Error("Invalid server-side managed chat profile");
  const expectedMessageId = options.expectedMessageId;
  if (expectedMessageId !== undefined) {
    const prefix = `${runId}:g`;
    const generation =
      typeof expectedMessageId === "string" &&
      expectedMessageId.startsWith(prefix)
        ? /^([1-9]\d*):assistant$/.exec(
            expectedMessageId.slice(prefix.length),
          )?.[1]
        : undefined;
    if (!generation || !Number.isSafeInteger(Number(generation)))
      throw new Error(
        "Expected assistant identity must belong to the server-owned run",
      );
  }
  return {
    threadId,
    runId,
    runUrl,
    root,
    timeoutMs,
    fetcher,
    headers,
    disconnectPolicy,
    profile,
    onCancelError: options.onCancelError,
    expectedMessageId,
    onMessageStart: options.onMessageStart,
  };
}

type Connection = ReturnType<typeof managedConnection>;

async function existingRun(connection: Connection, signal?: AbortSignal) {
  const bounded = signal
    ? AbortSignal.any([signal, AbortSignal.timeout(connection.timeoutMs)])
    : AbortSignal.timeout(connection.timeoutMs);
  const response = await connection.fetcher(connection.runUrl, {
    method: "GET",
    headers: connection.headers,
    redirect: "error",
    signal: bounded,
  });
  if (!response.ok) {
    await response.body?.cancel();
    throw new Error(`Managed run lookup failed (${response.status})`);
  }
  const run = JSON.parse(
    await readBody(response.body, MAX_FRAME_BYTES, bounded),
  );
  if (run.run_id !== connection.runId || run.thread_id !== connection.threadId)
    throw new Error("Unexpected managed run identity");
  if (
    ![
      "pending",
      "running",
      "success",
      "error",
      "timeout",
      "interrupted",
    ].includes(run.status)
  )
    throw new Error("Invalid managed run status");
  return run as { run_id: string; thread_id: string; status: string };
}

async function requestCancellation(
  connection: Connection,
  timeoutMs: number,
): Promise<void> {
  const response = await connection.fetcher(`${connection.runUrl}/cancel`, {
    method: "POST",
    headers: connection.headers,
    redirect: "error",
    signal: AbortSignal.timeout(timeoutMs),
  });
  await response.body?.cancel();
  if (!response.ok) throw new Error("Runkite cancellation was not confirmed");
}

/** Call from an authenticated app POST route after checking chat/run ownership.
 * Completion may already have won; this requests cancellation, not an undo.
 */
export async function cancelManagedChatRun(
  options: ManagedRunOptions,
): Promise<void> {
  const connection = managedConnection(options, "detach");
  const run = await existingRun(connection);
  if (["success", "error", "timeout", "interrupted"].includes(run.status))
    return;
  await requestCancellation(connection, connection.timeoutMs);
}

/** Read-only attachment by default. Never creates or dispatches another run. */
export async function attachManagedChatResponse(
  request: Request,
  options: ManagedChatAttachOptions,
): Promise<Response> {
  if (request.method !== "GET")
    throw new Error("Managed chat attach requires GET");
  checkAbort(request.signal);
  return managedChatResponse(request, managedConnection(options, "detach"));
}

/** New user turn. Opt into detach only after persisting its server-owned run ID. */
export async function createManagedChatResponse(
  request: Request,
  options: ManagedChatOptions,
): Promise<Response> {
  if (request.method !== "POST")
    throw new Error("Managed chat creation requires POST");
  if (options.disconnectPolicy === "detach" && !options.runId)
    throw new Error(
      "Detached managed chats require a persisted server-owned run ID",
    );
  const connection = managedConnection(
    { ...options, runId: options.runId ?? randomUUID() },
    "cancel",
  );
  const agentId = identifier(options.agentId);
  checkAbort(request.signal);
  const input = JSON.parse(
    await readBody(request.body, MAX_REQUEST_BYTES, request.signal),
  );
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

  return managedChatResponse(request, connection, { agentId, text });
}

async function managedChatResponse(
  request: Request,
  connection: Connection,
  newTurn?: { agentId: string; text: string },
): Promise<Response> {
  const {
    threadId,
    runId,
    runUrl,
    root,
    timeoutMs,
    fetcher,
    headers,
    disconnectPolicy,
    profile,
    onCancelError,
    expectedMessageId,
    onMessageStart,
  } = connection;
  const cancelOnDisconnect = disconnectPolicy === "cancel";
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
      await requestCancellation(connection, 5_000);
    })().catch((error) => {
      try {
        if (onCancelError) onCancelError();
        else
          console.warn(
            "Runkite cancellation was not confirmed; inspect the managed run",
          );
      } catch {
        // A recovery callback cannot replace the cancellation failure.
      }
      throw error;
    });
    return cancellation;
  };
  const abort = () => {
    if (cancelOnDisconnect) void cancelRun().catch(() => {});
  };
  request.signal.addEventListener("abort", abort, { once: true });
  const cleanup = () => request.signal.removeEventListener("abort", abort);
  let response: Response;
  try {
    // Keep creation bounded but independent of browser disconnect. Once the
    // creation response arrives, a cancelled request explicitly cancels its run.
    if (newTurn) {
      const creationSignal = AbortSignal.timeout(timeoutMs);
      creationStarted = true;
      const createdResponse = await fetcher(`${root}/runs`, {
        method: "POST",
        headers,
        redirect: "error",
        signal: creationSignal,
        body: JSON.stringify({
          run_id: runId,
          agent_id: newTurn.agentId,
          input: { messages: [{ role: "user", content: newTurn.text }] },
          stream_mode:
            profile === "ai-sdk"
              ? ["custom:ai_sdk", "values"]
              : ["messages", "values"],
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
        await readBody(createdResponse.body, MAX_FRAME_BYTES, creationSignal),
      );
      if (run.run_id !== runId || run.thread_id !== threadId)
        throw new Error("Unexpected managed run identity");
    } else {
      await existingRun(connection, signal);
      created = true;
    }
    if (signal.aborted) {
      if (cancelOnDisconnect) await cancelRun();
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
    if (cancelOnDisconnect) await cancelRun().catch(() => {});
    throw error;
  }

  async function* chunks(): AsyncGenerator<UIMessageChunk> {
    const seen = new Set<string>();
    const sequence = new Map<number, number>();
    let generation: number | undefined;
    let started = false;
    let textStarted = false;
    let text = "";
    let textBytes = 0;
    let finalValues: Record<string, unknown> | undefined;
    const textId = "managed-text";
    async function startMessage(messageId: string) {
      if (expectedMessageId && messageId !== expectedMessageId)
        throw new AttemptChangedError();
      if (onMessageStart) {
        const persistenceSignal = AbortSignal.any([
          signal,
          AbortSignal.timeout(timeoutMs),
        ]);
        await abortable(
          () => onMessageStart(messageId, persistenceSignal),
          persistenceSignal,
        );
        checkAbort(signal);
      }
    }
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
        const prefix = `${runId}_evt_`;
        const runnerId = frame.id.startsWith(prefix)
          ? /^(\d+)(?::g(\d+))?$/.exec(frame.id.slice(prefix.length))
          : null;
        if (profile !== "ai-sdk" && frame.id.startsWith(prefix) && !runnerId)
          throw new Error("Invalid runner event identity");
        if (runnerId) {
          const seq = Number(runnerId[1]);
          const attempt = Number(runnerId[2] ?? "1");
          if (
            !Number.isSafeInteger(seq) ||
            seq < 1 ||
            !Number.isSafeInteger(attempt) ||
            attempt < 1
          )
            throw new Error("Invalid runner event identity");
          if (generation !== undefined && attempt < generation) continue;
          if (generation !== undefined && attempt !== generation)
            throw new AttemptChangedError();
          const previous = sequence.get(attempt);
          // Count every runner event, including mirrored messages and lifecycle.
          // A trimmed prefix or a missing middle frame cannot be a full replay.
          if (
            (previous === undefined && seq > 2) ||
            (previous !== undefined && seq !== previous + 1)
          )
            throw new ReplayUnavailableError();
          if (
            profile !== "ai-sdk" &&
            (runnerId[1] !== String(seq) ||
              (runnerId[2] !== undefined && runnerId[2] !== String(attempt)))
          )
            throw new Error("Invalid runner event identity");
          sequence.set(attempt, seq);
          if (
            profile !== "ai-sdk" &&
            !started &&
            !["end", "error"].includes(frame.event)
          ) {
            if (
              seq !== 1 ||
              frame.event !== "lifecycle" ||
              JSON.parse(frame.data)?.event !== "running"
            )
              throw new ReplayUnavailableError();
            generation = attempt;
            const messageId = `${runId}:g${generation}:assistant`;
            await startMessage(messageId);
            checkAbort(signal);
            started = true;
            yield { type: "start", messageId };
          }
        }
        if (profile !== "ai-sdk" && frame.event === "messages") {
          if (!runnerId || !started) throw new ReplayUnavailableError();
          const delta = pythonDelta(JSON.parse(frame.data), runId);
          textBytes += Buffer.byteLength(delta);
          if (textBytes > MAX_FRAME_BYTES)
            throw new Error("Managed text exceeds the example's size limit");
          text += delta;
          if (delta) {
            if (!textStarted) {
              textStarted = true;
              yield { type: "text-start", id: textId };
            }
            yield { type: "text-delta", id: textId, delta };
          }
        } else if (profile !== "ai-sdk" && frame.event === "values") {
          if (!runnerId || !started) throw new ReplayUnavailableError();
          const value = JSON.parse(frame.data);
          validateProfile(value, profile, generation!);
          finalValues = value;
        } else if (frame.event === "custom:ai_sdk") {
          if (profile !== "ai-sdk")
            throw new Error("Unexpected native UI stream for Python profile");
          if (!frame.id || !runnerId)
            throw new Error("AI SDK events require a Runkite event ID");
          const attempt = Number(runnerId[2] ?? "1");
          if (!Number.isSafeInteger(attempt) || attempt < 1)
            throw new Error("Invalid runner attempt");
          if (generation !== undefined && attempt < generation) continue;
          if (generation !== undefined && attempt !== generation) {
            throw new AttemptChangedError();
          }
          generation = attempt;
          const parsed = await validate(JSON.parse(frame.data));
          if (!parsed.success)
            throw new Error("Invalid AI SDK UI stream event");
          const chunk = parsed.value;
          if (chunk.type === "start") {
            if (started) throw new ReplayUnavailableError();
            started = true;
          } else if (!started) {
            throw new ReplayUnavailableError();
          }
          if (
            chunk.type === "start" &&
            chunk.messageId !== `${runId}:g${generation}:assistant`
          ) {
            throw new Error("Unexpected managed assistant message identity");
          }
          if (chunk.type === "start") {
            await startMessage(chunk.messageId!);
            checkAbort(signal);
          }
          if (chunk.type === "finish") finish = chunk;
          else if (chunk.type === "error") nativeError = chunk;
          else if (chunk.type === "abort") nativeAbort = chunk;
          else yield chunk;
        } else if (frame.event === "end" || frame.event === "error") {
          // Raw SSE is advisory during reclaim. Confirm the durable result before
          // letting useChat report a successful completion.
          const completionSignal = AbortSignal.any([
            signal,
            AbortSignal.timeout(timeoutMs),
          ]);
          const durable = await fetcher(`${runUrl}/wait`, {
            headers,
            redirect: "error",
            signal: completionSignal,
          });
          if (!durable.ok) {
            await durable.body?.cancel();
            throw new Error("Managed run completion could not be confirmed");
          }
          const result = JSON.parse(
            await readBody(durable.body, MAX_FRAME_BYTES, completionSignal),
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
          if (
            profile !== "ai-sdk" &&
            generation !== undefined &&
            result.run.metadata?._runkite_completion_generation !== generation
          )
            throw new AttemptChangedError();
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
          if (profile !== "ai-sdk") {
            if (!started || generation === undefined || !finalValues)
              throw new ReplayUnavailableError();
            validateProfile(result.values, profile, generation);
            const currentText = currentTurnText(
              result.values,
              result.run.input,
              newTurn?.text,
            );
            if (
              currentText !== text ||
              currentTurnText(finalValues, result.run.input, newTurn?.text) !==
                text
            )
              throw new Error(
                "The UI text does not match the durable current turn",
              );
            const hasResult = Object.hasOwn(result.values, "result");
            if (
              Object.hasOwn(finalValues, "result") !== hasResult ||
              (hasResult &&
                canonicalJSON(result.values.result) !==
                  canonicalJSON(finalValues.result))
            )
              throw new Error("The UI result does not match durable output");
            if (hasResult && profile !== "pydantic-ai")
              throw new Error(
                "This profile does not support structured results",
              );
            if (textStarted) yield { type: "text-end", id: textId };
            if (hasResult)
              yield {
                type: "data-result",
                id: `${runId}:g${generation}:assistant:result`,
                data: result.values.result,
              };
            yield { type: "finish", finishReason: "stop" };
            return;
          }
          if (!started || !finish) throw new ReplayUnavailableError();
          if (
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
      } catch (error) {
        if (cancelOnDisconnect) await cancelRun().catch(() => {});
        if (closed) return;
        if (signal.aborted)
          output.enqueue({
            type: "abort",
            reason: "Application request cancelled",
          });
        else
          output.enqueue({
            type: "error",
            errorText:
              error instanceof ReplayUnavailableError ||
              error instanceof AttemptChangedError
                ? error.message
                : "Managed chat delivery failed; inspect the Runkite run",
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
      try {
        if (cancelOnDisconnect) await cancelRun();
      } finally {
        await iterator.return(undefined);
      }
    },
  });
  return createUIMessageStreamResponse({
    stream,
    headers: { "X-Runkite-Run-Id": runId },
  });
}
