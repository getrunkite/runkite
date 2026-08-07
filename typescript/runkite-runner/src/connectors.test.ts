/**
 * Self-check for connectors.ts header assembly (no live control plane).
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { ConnectorError, getConnectorSession } from "./connectors.js";
import { HEADER_GENERATION, HEADER_RUN_ID } from "./tenantCtx.js";

test("getConnectorSession rejects config without run_id", async () => {
  await assert.rejects(() => getConnectorSession({ configurable: {} }, "sf"), /run_id/);
});

test("ConnectorError is an Error subclass", () => {
  assert.ok(new ConnectorError("x") instanceof Error);
});

test("run-bound headers include generation from configurable", async () => {
  // Intercept fetch to capture headers without a real control plane.
  const original = globalThis.fetch;
  let seen: HeadersInit | undefined;
  globalThis.fetch = (async (_url: RequestInfo | URL, init?: RequestInit) => {
    seen = init?.headers;
    return new Response(JSON.stringify({ ok: true }), { status: 200 });
  }) as typeof fetch;
  try {
    await getConnectorSession({ configurable: { run_id: "run-1", generation: 4 } }, "sf", {
      controlPlaneUrl: "http://example.invalid",
    });
    const headers = seen as Record<string, string>;
    assert.equal(headers[HEADER_RUN_ID], "run-1");
    assert.equal(headers[HEADER_GENERATION], "4");
  } finally {
    globalThis.fetch = original;
  }
});
