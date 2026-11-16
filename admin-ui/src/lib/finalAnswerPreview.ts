/** Conservative presentation of durable output, separate from streaming Try text. */
export const MAX_FINAL_ANSWER_PREVIEW_CHARS = 12_000;

export interface FinalAnswerPreview {
  text: string;
  truncated: boolean;
}

type RecordValue = Record<string, unknown>;
const INVALID = Symbol("unreadable field");

function record(value: unknown): RecordValue | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) return null;
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null ? value as RecordValue : null;
}

function field(value: RecordValue, key: string): unknown {
  const descriptor = Object.getOwnPropertyDescriptor(value, key);
  return descriptor ? ("value" in descriptor ? descriptor.value : INVALID) : undefined;
}

function assistant(value: unknown): boolean {
  return typeof value === "string" && ["assistant", "ai", "aimessage"].includes(value.toLowerCase());
}

function unsafeMetadata(value: RecordValue): boolean {
  const channel = field(value, "channel");
  if (channel !== undefined && channel !== null && channel !== "final") return true;
  for (const key of ["finish_reason", "finishReason", "stop_reason"]) {
    const reason = field(value, key);
    if (reason !== undefined && reason !== null && !["stop", "end_turn", "stop_sequence"].includes(reason as string)) return true;
  }
  for (const key of ["tool_calls", "invalid_tool_calls", "toolCalls"]) {
    const calls = field(value, key);
    if (calls !== undefined && calls !== null && (!Array.isArray(calls) || calls.length !== 0)) return true;
  }
  for (const key of ["function_call", "functionCall", "tool_call_id", "reasoning_content", "thinking", "reasoning"]) {
    const content = field(value, key);
    if (content !== undefined && content !== null) return true;
  }
  return false;
}

function textContent(value: unknown): string | null {
  if (typeof value === "string") return value;
  if (!Array.isArray(value) || value.length === 0) return null;
  const parts: string[] = [];
  for (const valuePart of value) {
    const part = record(valuePart);
    if (!part || field(part, "type") !== "text" || unsafeMetadata(part)) return null;
    const text = field(part, "text");
    if (typeof text !== "string") return null;
    parts.push(text);
  }
  return parts.join("");
}

/**
 * Recognized runner output is {messages:[..., terminal assistant message]}.
 * Python/native profiles use flat role/type + content; LangGraph.js can put
 * content under data. Text-only content blocks are supported. We never scan
 * backwards or infer answers from arbitrary result/answer/text properties.
 * Structured results, tool/reasoning content and ambiguous shapes stay raw.
 */
export function finalAnswerPreview(status: string, output: unknown): FinalAnswerPreview | null {
  if (status !== "success") return null;
  try {
    const root = record(output);
    if (!root) return null;
    const messages = field(root, "messages");
    if (!Array.isArray(messages) || messages.length === 0) return null;
    const message = record(messages[messages.length - 1]);
    if (!message) return null;
    const rawData = field(message, "data");
    const data = rawData == null ? null : record(rawData);
    if (rawData != null && !data) return null;
    const containers = data ? [message, data] : [message];
    let roleFound = false;
    for (const container of containers) {
      for (const key of ["role", "type"]) {
        const role = field(container, key);
        if (role === undefined) continue;
        if (!assistant(role)) return null;
        roleFound = true;
      }
      if (unsafeMetadata(container)) return null;
      for (const key of ["additional_kwargs", "response_metadata"]) {
        const metadata = field(container, key);
        if (metadata == null) continue;
        const metadataRecord = record(metadata);
        if (!metadataRecord || unsafeMetadata(metadataRecord)) return null;
      }
    }
    if (!roleFound) return null;
    const content = field(message, "content");
    const nestedContent = data ? field(data, "content") : undefined;
    if (content !== undefined && nestedContent !== undefined) return null;
    const text = textContent(content === undefined ? nestedContent : content);
    if (text === null || !text.trim()) return null;
    const result = field(root, "result");
    if (result !== undefined && result !== text) return null;
    let preview = text.slice(0, MAX_FINAL_ANSWER_PREVIEW_CHARS);
    // Avoid splitting a surrogate pair when bounding the visible preview.
    const last = preview.charCodeAt(preview.length - 1);
    if (text.length > preview.length && last >= 0xd800 && last <= 0xdbff) preview = preview.slice(0, -1);
    return { text: preview, truncated: preview.length < text.length };
  } catch {
    return null;
  }
}
