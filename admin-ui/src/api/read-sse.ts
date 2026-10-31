export interface SseEvent {
  event: string;
  data: string;
}

const MAX_FRAME_CHARACTERS = 4 * 1024 * 1024;

/** Read SSE line endings and fields without stripping application whitespace. */
export async function readSseBody(
  body: ReadableStream<Uint8Array>,
  onEvent: (event: SseEvent) => void,
): Promise<void> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let eventName = "message";
  let dataLines: string[] = [];
  let frameCharacters = 0;

  function line(value: string) {
    if (value === "") {
      if (dataLines.length) onEvent({ event: eventName || "message", data: dataLines.join("\n") });
      eventName = "message";
      dataLines = [];
      frameCharacters = 0;
      return;
    }
    frameCharacters += value.length;
    if (frameCharacters > MAX_FRAME_CHARACTERS) throw new Error("Admin event stream frame is too large");
    const colon = value.indexOf(":");
    const field = colon < 0 ? value : value.slice(0, colon);
    let content = colon < 0 ? "" : value.slice(colon + 1);
    if (content.startsWith(" ")) content = content.slice(1);
    if (field === "event") eventName = content;
    else if (field === "data") dataLines.push(content);
  }

  function drain(final = false) {
    let start = 0;
    for (let index = 0; index < buffer.length; index++) {
      const character = buffer[index];
      if (character !== "\n" && character !== "\r") continue;
      // A CRLF may be divided across network chunks.
      if (character === "\r" && index + 1 === buffer.length && !final) break;
      line(buffer.slice(start, index));
      if (character === "\r" && buffer[index + 1] === "\n") index++;
      start = index + 1;
    }
    buffer = buffer.slice(start);
    if (buffer.length + frameCharacters > MAX_FRAME_CHARACTERS)
      throw new Error("Admin event stream frame is too large");
  }

  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      drain();
    }
    buffer += decoder.decode();
    drain(true);
    // SSE dispatch requires a blank line. An unfinished final event is discarded.
  } finally {
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}
