/**
 * Self-check for connectors.ts header assembly (no live control plane).
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { ConnectorError, getConnectorSession, HEADER_CONNECTOR_SESSION, proxyConnectorMcp } from "./connectors.js";
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

test("proxyConnectorMcp sends X-Runkite-Connector-Session", async () => {
  const original = globalThis.fetch;
  const calls: { url: string; headers: Record<string, string> }[] = [];
  globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const headers = (init?.headers ?? {}) as Record<string, string>;
    calls.push({ url: u, headers });
    if (u.endsWith("/session")) {
      return new Response(JSON.stringify({ session_token: "tok-abc", mcp: { url: "/x" } }), {
        status: 200,
      });
    }
    return new Response(JSON.stringify({ jsonrpc: "2.0", id: 1, result: {} }), { status: 200 });
  }) as typeof fetch;
  try {
    await proxyConnectorMcp(
      { configurable: { run_id: "run-1", generation: 1 } },
      "sf",
      {
        jsonrpc: "2.0",
        id: 1,
        method: "tools/list",
      },
      { controlPlaneUrl: "http://example.invalid" },
    );
    assert.equal(calls.length, 2);
    assert.match(calls[0].url, /\/session$/);
    assert.match(calls[1].url, /\/mcp$/);
    assert.equal(calls[1].headers[HEADER_CONNECTOR_SESSION], "tok-abc");
  } finally {
    globalThis.fetch = original;
  }
});

test("proxyConnectorMcp re-mints once on 401 when auto-minting", async () => {
  const original = globalThis.fetch;
  const calls: string[] = [];
  let mcpHits = 0;
  let sessionN = 0;
  globalThis.fetch = (async (url: RequestInfo | URL) => {
    const u = String(url);
    calls.push(u);
    if (u.endsWith("/session")) {
      sessionN += 1;
      return new Response(JSON.stringify({ session_token: `tok-${sessionN}` }), { status: 200 });
    }
    mcpHits += 1;
    if (mcpHits === 1) {
      return new Response("expired", { status: 401 });
    }
    return new Response(JSON.stringify({ jsonrpc: "2.0", id: 1, result: {} }), { status: 200 });
  }) as typeof fetch;
  try {
    await proxyConnectorMcp(
      { configurable: { run_id: "run-1", generation: 1 } },
      "sf",
      { jsonrpc: "2.0", id: 1, method: "tools/list" },
      { controlPlaneUrl: "http://example.invalid" },
    );
    // session → mcp-401 → re-mint session → mcp-success
    assert.equal(calls.length, 4);
    assert.match(calls[0], /\/session$/);
    assert.match(calls[1], /\/mcp$/);
    assert.match(calls[2], /\/session$/);
    assert.match(calls[3], /\/mcp$/);
  } finally {
    globalThis.fetch = original;
  }
});

test("proxyConnectorMcp does not retry 401 when caller supplied sessionToken", async () => {
  const original = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = (async () => {
    calls += 1;
    return new Response("expired", { status: 401 });
  }) as typeof fetch;
  try {
    await assert.rejects(
      () =>
        proxyConnectorMcp(
          { configurable: { run_id: "run-1", generation: 1 } },
          "sf",
          { jsonrpc: "2.0", id: 1, method: "tools/list" },
          { controlPlaneUrl: "http://example.invalid", sessionToken: "caller-tok" },
        ),
      /HTTP 401/,
    );
    assert.equal(calls, 1);
  } finally {
    globalThis.fetch = original;
  }
});
