/** Normalize Agent Protocol / runner SSE message shapes for the Try panel.
 * Python runners usually emit flat {role|type, content}; LangGraph.js often
 * emits {type, data: {content}}. Prefer the last assistant turn — never the
 * last message blindly (that echoes the user's own text as if the AI spoke).
 */

export function messageContent(msg: unknown): string {
  if (msg == null) return "";
  if (typeof msg === "string") return msg;
  if (typeof msg !== "object") return String(msg);
  const m = msg as Record<string, unknown>;
  const raw = m.content ?? (m.data as Record<string, unknown> | undefined)?.content;
  if (typeof raw === "string") return raw;
  if (Array.isArray(raw)) {
    return raw
      .map((part) => {
        if (typeof part === "string") return part;
        if (part && typeof part === "object" && "text" in part) return String((part as { text: unknown }).text ?? "");
        return "";
      })
      .join("");
  }
  return "";
}

export function messageRole(msg: unknown): "user" | "assistant" | "other" {
  if (!msg || typeof msg !== "object") return "other";
  const m = msg as Record<string, unknown>;
  const data = m.data as Record<string, unknown> | undefined;
  let raw = String(m.role ?? m.type ?? data?.type ?? data?.role ?? "").toLowerCase();
  if (raw === "human" || raw === "user" || raw === "humanmessage") return "user";
  if (raw === "ai" || raw === "assistant" || raw === "aimessage") return "assistant";
  return "other";
}

/** Prefer last assistant message text from a values payload. */
export function extractAssistantText(data: unknown): string {
  if (!data || typeof data !== "object") return "";
  const d = data as Record<string, unknown>;
  const messages = d.messages;
  if (!Array.isArray(messages) || messages.length === 0) {
    return messageContent(d);
  }
  for (let i = messages.length - 1; i >= 0; i--) {
    const msg = messages[i];
    if (messageRole(msg) === "assistant") {
      const text = messageContent(msg);
      if (text.trim()) return text;
    }
  }
  return "";
}

export function extractUsage(data: unknown): Record<string, unknown> | null {
  if (!data || typeof data !== "object") return null;
  const u = (data as Record<string, unknown>).usage;
  if (u && typeof u === "object") return u as Record<string, unknown>;
  return null;
}

export function parseSseMethod(eventName: string, payload: Record<string, unknown>): string {
  return String(payload.event ?? payload.method ?? eventName ?? "");
}

export function parseSseData(payload: Record<string, unknown>): unknown {
  return payload.data !== undefined ? payload.data : payload;
}
