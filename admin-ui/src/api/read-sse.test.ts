import assert from "node:assert/strict";
import { test } from "node:test";
import { readSseBody, type SseEvent } from "./read-sse.ts";

function stream(value: string, chunkBytes = 1) {
  const bytes = new TextEncoder().encode(value);
  let offset = 0;
  return new ReadableStream<Uint8Array>({
    pull(controller) {
      if (offset === bytes.length) { controller.close(); return; }
      controller.enqueue(bytes.slice(offset, offset + chunkBytes));
      offset += chunkBytes;
    },
  });
}

for (const ending of ["\n", "\r\n", "\r"]) {
  test(`SSE preserves data whitespace and split UTF-8 with ${JSON.stringify(ending)} endings`, async () => {
    const events: SseEvent[] = [];
    const body = stream([": keepalive", "", "event: values", "data:  café  ", "data:", "data: tail  ", "", ""].join(ending));
    await readSseBody(body, (event) => events.push(event));
    assert.deepEqual(events, [{ event: "values", data: " café  \n\ntail  " }]);
    assert.equal(body.locked, false);
  });
}

test("SSE handles empty data/default event fields and discards an unfinished final frame", async () => {
  const events: SseEvent[] = [];
  await readSseBody(stream("event:\ndata\n\ndata: unfinished\n"), (event) => events.push(event));
  assert.deepEqual(events, [{ event: "message", data: "" }]);
});

test("SSE releases and cancels the reader when an event consumer fails", async () => {
  let canceled = false;
  const body = new ReadableStream<Uint8Array>({
    start(controller) { controller.enqueue(new TextEncoder().encode("data: value\n\n")); },
    cancel() { canceled = true; },
  });
  await assert.rejects(readSseBody(body, () => { throw new Error("consumer failed"); }), /consumer failed/);
  assert.equal(canceled, true);
  assert.equal(body.locked, false);
});

test("SSE rejects an unbounded frame and closes its reader", async () => {
  const body = stream(`data: ${"x".repeat(4 * 1024 * 1024 + 1)}`, 64 * 1024);
  await assert.rejects(readSseBody(body, () => assert.fail("Oversized frame was dispatched")), /too large/);
  assert.equal(body.locked, false);
});
