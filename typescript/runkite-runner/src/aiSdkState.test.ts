import assert from "node:assert/strict";
import { test } from "node:test";
import { AiSdkState } from "./aiSdkState.js";
import { CompletionDeliveryError } from "./completion.js";
import { runWithBinding } from "./tenantCtx.js";

const stateHeaders = {
  "X-Runkite-Checkpoint-Framework": "ai-sdk",
  ETag: '"7"',
};

test("AI SDK state preserves native JSON and binds conditional writes to the run", async () => {
  const requests: { url: string; init: RequestInit | undefined }[] = [];
  const value = {
    messages: [{ role: "user", content: "hello" }],
    optional: undefined,
    metadata: { numbers: [1, 1.5, null], enabled: true },
  };
  const request: typeof fetch = async (url, init) => {
    requests.push({ url: String(url), init });
    if (init?.method === "PUT") return new Response(null, { status: 204 });
    return Response.json(value, { headers: stateHeaders });
  };
  const state = new AiSdkState("http://plane/", "typescript-ai-sdk", "test-runner-token", request);
  await runWithBinding({ tenantId: "tenant-one", runId: "run-one", generation: 9 }, async () => {
    const loaded = await state.load<typeof value>("thread/one");
    assert.equal(loaded.etag, '"7"');
    assert.deepEqual(loaded.value, JSON.parse(JSON.stringify(value)));
    await state.save("thread/one", value, loaded.etag);
    await state.save("thread/two", value, null);
  });
  assert.equal(requests.length, 3);
  assert.equal(requests[0].url, "http://plane/internal/checkpoints/thread%2Fone/adapter-state");
  for (const { init } of requests) {
    const headers = new Headers(init?.headers);
    assert.equal(headers.get("X-Runkite-Tenant-Id"), "tenant-one");
    assert.equal(headers.get("X-Runkite-Run-Id"), "run-one");
    assert.equal(headers.get("X-Runkite-Generation"), "9");
    assert.equal(headers.get("X-Runner-Kind"), "typescript-ai-sdk");
    assert.equal(headers.get("X-Runner-Token"), "test-runner-token");
    assert.equal(headers.get("X-Runkite-Checkpoint-Framework"), "ai-sdk");
  }
  assert.equal(new Headers(requests[1].init?.headers).get("If-Match"), '"7"');
  assert.equal(new Headers(requests[1].init?.headers).has("If-None-Match"), false);
  assert.equal(new Headers(requests[2].init?.headers).get("If-None-Match"), "*");
  assert.equal(new Headers(requests[2].init?.headers).has("If-Match"), false);
  assert.deepEqual(JSON.parse(String(requests[1].init?.body)), JSON.parse(JSON.stringify(value)));
});

test("AI SDK state rejects rich or lossy values before making a write", async () => {
  let writes = 0;
  let accessorCalls = 0;
  let serializerCalls = 0;
  const getter = Object.defineProperty({}, "secret", {
    enumerable: true,
    get() {
      accessorCalls++;
      return "must not read";
    },
  });
  const customSerializer = {
    toJSON() {
      serializerCalls++;
      return "must not run";
    },
  };
  const cycle: Record<string, unknown> = {};
  cycle.self = cycle;
  const sparse = new Array(1);
  const decoratedHole = Object.assign(new Array(1), { extra: "not an array element" });
  const bad: unknown[] = [
    { nested: getter },
    { nested: customSerializer },
    { nested: new Date("2026-01-01T00:00:00Z") },
    { nested: new Map([["key", "value"]]) },
    { nested: new Set(["value"]) },
    { nested: Buffer.from([1, 2]) },
    { nested: new Uint8Array([1, 2]) },
    { nested: new ArrayBuffer(2) },
    { nested: 1n },
    { nested: Symbol("value") },
    { nested: () => "value" },
    { nested: NaN },
    { nested: Infinity },
    { nested: -0 },
    { nested: 2 ** 53 },
    { nested: "\ud800" },
    { ["\udfff"]: "value" },
    { nested: [undefined] },
    { nested: sparse },
    { nested: decoratedHole },
    cycle,
  ];
  const request: typeof fetch = async () => {
    writes++;
    return new Response(null, { status: 204 });
  };
  const state = new AiSdkState("http://plane", "typescript-ai-sdk", undefined, request);
  for (const value of bad) {
    await assert.rejects(state.save("thread", value, null), CompletionDeliveryError);
  }
  assert.equal(writes, 0);
  assert.equal(accessorCalls, 0, "Serialization must not invoke getters");
  assert.equal(serializerCalls, 0, "Serialization must not invoke toJSON");
});

test("AI SDK state refuses invalid UTF-8 instead of restoring replacement characters", async () => {
  const bytes = Buffer.concat([Buffer.from('{"text":"'), Buffer.from([0xc3, 0x28]), Buffer.from('"}')]);
  const request: typeof fetch = async () => new Response(bytes, { headers: stateHeaders });
  const state = new AiSdkState("http://plane", "typescript-ai-sdk", undefined, request);
  await assert.rejects(state.load("thread"), CompletionDeliveryError);
});

test("AI SDK state rejects stored JSON values that cannot round trip through the native codec", async () => {
  for (const body of ['{"number":1e400}', '{"number":9007199254740992}', '{"number":-0}', '{"text":"\\ud800"}']) {
    const request: typeof fetch = async () => new Response(body, { headers: stateHeaders });
    const state = new AiSdkState("http://plane", "typescript-ai-sdk", undefined, request);
    await assert.rejects(state.load("thread"), CompletionDeliveryError);
  }
});

test("AI SDK state only restores matching framework state with a strong ETag", async () => {
  const headers = [
    { "X-Runkite-Checkpoint-Framework": "ai-sdk" },
    { ...stateHeaders, ETag: 'W/"7"' },
    { ...stateHeaders, ETag: "7" },
    { ...stateHeaders, ETag: '"not-a-version"' },
    { ...stateHeaders, "X-Runkite-Checkpoint-Framework": "langgraph" },
    { ETag: '"7"' },
  ];
  for (const value of headers) {
    const request: typeof fetch = async () => Response.json({ messages: [] }, { headers: value });
    const state = new AiSdkState("http://plane", "typescript-ai-sdk", undefined, request);
    await assert.rejects(state.load("thread"), CompletionDeliveryError);
  }
});

test("AI SDK state recognizes a missing checkpoint and fails closed on other HTTP errors", async () => {
  for (const status of [403, 412, 500]) {
    let writes = 0;
    const request: typeof fetch = async (_url, init) => {
      if (init?.method === "PUT") writes++;
      return new Response("state rejected", { status });
    };
    const state = new AiSdkState("http://plane", "typescript-ai-sdk", undefined, request);
    await assert.rejects(state.load("thread"), CompletionDeliveryError);
    await assert.rejects(state.save("thread", { messages: [] }, '"7"'), CompletionDeliveryError);
    assert.equal(writes, 1, "A conflict must not retry or fall back to an unconditional write");
  }
  const request: typeof fetch = async () => new Response(null, { status: 404 });
  const state = new AiSdkState("http://plane", "typescript-ai-sdk", undefined, request);
  assert.deepEqual(await state.load("thread"), { value: null, etag: null });
});

test("AI SDK state bounds stored payloads before PUT and during GET", async () => {
  let writes = 0;
  const writeRequest: typeof fetch = async () => {
    writes++;
    return new Response(null, { status: 204 });
  };
  const writeState = new AiSdkState("http://plane", "typescript-ai-sdk", undefined, writeRequest);
  await assert.rejects(writeState.save("thread", { text: "x".repeat(16 << 20) }, null), CompletionDeliveryError);
  assert.equal(writes, 0);

  let cancelled = false;
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(new Uint8Array((16 << 20) + 1));
    },
    cancel() {
      cancelled = true;
    },
  });
  const readRequest: typeof fetch = async () => new Response(stream, { headers: stateHeaders });
  const readState = new AiSdkState("http://plane", "typescript-ai-sdk", undefined, readRequest);
  await assert.rejects(readState.load("thread"), CompletionDeliveryError);
  assert.equal(cancelled, true);
});

test("AI SDK state propagates caller cancellation through a stalled read", { timeout: 2_000 }, async () => {
  const controller = new AbortController();
  let entered!: () => void;
  const started = new Promise<void>((resolve) => {
    entered = resolve;
  });
  let requestSignal: AbortSignal | undefined;
  const request: typeof fetch = async (_url, init) =>
    new Promise<Response>((_resolve, reject) => {
      requestSignal = init?.signal ?? undefined;
      assert.ok(requestSignal);
      const abort = () => reject(requestSignal?.reason);
      requestSignal.addEventListener("abort", abort, { once: true });
      if (requestSignal.aborted) abort();
      entered();
    });
  const state = new AiSdkState("http://plane", "typescript-ai-sdk", undefined, request);
  const load = state.load("thread", controller.signal);
  await started;
  controller.abort(new Error("run superseded"));
  await assert.rejects(load, CompletionDeliveryError);
  assert.equal(requestSignal?.aborted, true);
});
