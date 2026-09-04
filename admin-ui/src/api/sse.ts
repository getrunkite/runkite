import { getCSRFToken } from "./client";

export interface SseEvent {
  event: string;
  data: string;
}

/**
 * Manually parsed SSE reader instead of the browser's native EventSource.
 * EventSource can't send custom headers -- we already used fetch for that
 * when auth was Bearer; now the session cookie is enough (credentials:
 * "include"), and GET streams don't need CSRF.
 *
 * Returns an abort() function; call it on unmount to close the stream.
 */
export function streamSSE(path: string, onEvent: (event: SseEvent) => void, onDone: () => void): () => void {
  const controller = new AbortController();

  fetch(`/admin-api${path}`, {
    credentials: "include",
    signal: controller.signal,
  })
    .then(async (resp) => {
      if (!resp.ok || !resp.body) {
        onDone();
        return;
      }
      await readSseBody(resp.body, onEvent);
      onDone();
    })
    .catch(() => {
      onDone();
    });

  return () => controller.abort();
}

/** POST SSE (create-and-stream). Sends CSRF when present. */
export function streamSSEPost(
  path: string,
  body: unknown,
  onEvent: (event: SseEvent) => void,
  onDone: (err?: string) => void,
): () => void {
  const controller = new AbortController();
  const headers = new Headers({ "Content-Type": "application/json" });
  const csrf = getCSRFToken();
  if (csrf) headers.set("X-CSRF-Token", csrf);

  fetch(`/admin-api${path}`, {
    method: "POST",
    credentials: "include",
    headers,
    body: JSON.stringify(body ?? {}),
    signal: controller.signal,
  })
    .then(async (resp) => {
      if (!resp.ok) {
        let message = `${resp.status} ${resp.statusText}`;
        try {
          const j = await resp.json();
          if (j?.message) message = j.message;
        } catch {
          /* keep status line */
        }
        onDone(message);
        return;
      }
      if (!resp.body) {
        onDone();
        return;
      }
      await readSseBody(resp.body, onEvent);
      onDone();
    })
    .catch((err) => {
      if (err?.name === "AbortError") onDone();
      else onDone(String(err?.message || err));
    });

  return () => controller.abort();
}

async function readSseBody(body: ReadableStream<Uint8Array>, onEvent: (event: SseEvent) => void): Promise<void> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    let frameEnd: number;
    while ((frameEnd = buffer.indexOf("\n\n")) !== -1) {
      const frame = buffer.slice(0, frameEnd);
      buffer = buffer.slice(frameEnd + 2);
      parseFrame(frame, onEvent);
    }
  }
}

function parseFrame(frame: string, onEvent: (event: SseEvent) => void): void {
  let eventName = "message";
  const dataLines: string[] = [];
  for (const line of frame.split("\n")) {
    if (line.startsWith("event:")) eventName = line.slice("event:".length).trim();
    else if (line.startsWith("data:")) dataLines.push(line.slice("data:".length).trim());
  }
  if (dataLines.length > 0) onEvent({ event: eventName, data: dataLines.join("\n") });
}
