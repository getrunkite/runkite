import { getCSRFToken } from "./client";
import { readSseBody, type SseEvent } from "./read-sse";
export type { SseEvent } from "./read-sse";

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
    redirect: "error",
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
    redirect: "error",
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
