import assert from "node:assert/strict";
import { test } from "node:test";
import { createGovernanceClient, GovernanceError } from "./index.ts";

const call = { agentId: "a", connector: "c", tool: "t", runId: "turn" };
const untilAbort = (signal: AbortSignal) =>
  new Promise<never>((_, reject) => {
    signal.addEventListener("abort", () => reject(signal.reason), {
      once: true,
    });
    if (signal.aborted) reject(signal.reason);
  });

test("deadline interrupts blocked response headers", async () => {
  const client = createGovernanceClient({
    baseUrl: "http://plane",
    timeoutMs: 25,
    fetch: ((_url, init) => untilAbort(init!.signal!)) as typeof fetch,
  });
  await assert.rejects(
    client.resolve(call),
    (err: unknown) => err instanceof GovernanceError && err.status === 408,
  );
});

test("deadline covers a blocked response body", async () => {
  const client = createGovernanceClient({
    baseUrl: "http://plane",
    timeoutMs: 25,
    fetch: (async (_url, init) => ({
      ok: true,
      text: () => untilAbort(init!.signal!),
    })) as typeof fetch,
  });
  await assert.rejects(
    client.action("pending"),
    (err: unknown) => err instanceof GovernanceError && err.status === 408,
  );
});

test("caller abort interrupts pending action HTTP", async () => {
  const controller = new AbortController();
  let pending = false;
  const client = createGovernanceClient({
    baseUrl: "http://plane",
    timeoutMs: 1000,
    fetch: (async (_url, init) => {
      if (!pending) {
        pending = true;
        return new Response(
          JSON.stringify({ effect: "pending", action_id: "a" }),
        );
      }
      setTimeout(() => controller.abort(), 10);
      return untilAbort(init!.signal!);
    }) as typeof fetch,
  });
  await assert.rejects(
    client.resolve(call, { signal: controller.signal }),
    (err: unknown) => err instanceof DOMException && err.name === "AbortError",
  );
});
