import { getStoredCredential } from "./client";

export interface SseEvent {
  event: string;
  data: string;
}

/**
 * Manually parsed SSE reader instead of the browser's native EventSource.
 * EventSource can't send a custom Authorization header at all -- a well
 * known platform limitation -- and every /admin-api/* route requires one
 * whenever auth is configured. fetch() has no such restriction, so this
 * reads the same "id: ...\nevent: ...\ndata: ...\n\n" stream
 * streamExistingRun (internal/api/runs.go) writes, by hand.
 *
 * Returns an abort() function; call it on unmount to close the stream.
 */
export function streamSSE(path: string, onEvent: (event: SseEvent) => void, onDone: () => void): () => void {
  const controller = new AbortController();
  const credential = getStoredCredential();
  const headers = new Headers();
  if (credential) headers.set("Authorization", `Bearer ${credential}`);

  fetch(`/admin-api${path}`, { headers, signal: controller.signal })
    .then(async (resp) => {
      if (!resp.ok || !resp.body) {
        onDone();
        return;
      }
      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";

      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });

        // SSE frames are separated by a blank line.
        let frameEnd: number;
        while ((frameEnd = buffer.indexOf("\n\n")) !== -1) {
          const frame = buffer.slice(0, frameEnd);
          buffer = buffer.slice(frameEnd + 2);
          parseFrame(frame, onEvent);
        }
      }
      onDone();
    })
    .catch(() => {
      // AbortError from unmount, or a real network error -- either way
      // there's nothing more to stream.
      onDone();
    });

  return () => controller.abort();
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
