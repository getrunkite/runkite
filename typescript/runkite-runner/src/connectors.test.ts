/**
 * Self-check for connectors.ts header assembly (no live control plane).
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { ConnectorError, getConnectorSession, HEADER_CONNECTOR_SESSION, proxyConnectorMcp } from "./connectors.js";
import { HEADER_GENERATION, HEADER_RUN_ID, HEADER_RUN_ID_B64, runIdHeaders, runWithBinding } from "./tenantCtx.js";

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

test("explicit connector config replaces inherited raw and encoded run headers", async () => {
  const original = globalThis.fetch;
  let seen: Record<string, string> = {};
  globalThis.fetch = (async (_url, init) => {
    seen = init?.headers as Record<string, string>;
    return new Response(JSON.stringify({ session_token: "token" }));
  }) as typeof fetch;
  try {
    for (const [bound, explicit] of [
      ["old", "ré 雪"],
      ["old ", "new"],
    ]) {
      await runWithBinding({ runId: bound, generation: 1 }, () =>
        getConnectorSession({ run_id: explicit, generation: 3 }, "workspace"),
      );
      const actual = Object.fromEntries(
        Object.entries(seen).filter(([key]) => key === HEADER_RUN_ID || key === HEADER_RUN_ID_B64),
      );
      assert.deepEqual(actual, runIdHeaders(explicit));
      assert.equal(seen[HEADER_GENERATION], "3");
    }
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
      return new Response(JSON.stringify({ reason_code: "connector_session_invalid" }), { status: 401 });
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
    return new Response(JSON.stringify({ reason_code: "connector_session_invalid" }), { status: 401 });
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

test("unmarked 401 cannot replay an operation that may have reached upstream", async () => {
  const original = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = (async (url) => {
    calls++;
    return String(url).endsWith("/session")
      ? new Response(JSON.stringify({ session_token: "token" }))
      : new Response("upstream unauthorized after operation", { status: 401 });
  }) as typeof fetch;
  try {
    await assert.rejects(
      () => proxyConnectorMcp({ run_id: "r", generation: 1 }, "sf", { method: "tools/call" }),
      /HTTP 401/,
    );
    assert.equal(calls, 2);
  } finally {
    globalThis.fetch = original;
  }
});

test("MCP operation uses one total deadline across mint and capability renewal", async () => {
  const original = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = (async (url, init) => {
    calls++;
    await new Promise<void>((resolve, reject) => {
      const signal = init?.signal;
      const timer = setTimeout(resolve, 25);
      signal?.addEventListener(
        "abort",
        () => {
          clearTimeout(timer);
          reject(signal.reason);
        },
        { once: true },
      );
    });
    return String(url).endsWith("/session")
      ? new Response(JSON.stringify({ session_token: "token" }))
      : new Response(JSON.stringify({ reason_code: "connector_session_invalid" }), { status: 401 });
  }) as typeof fetch;
  try {
    await assert.rejects(() => proxyConnectorMcp({ run_id: "r" }, "sf", { method: "tools/call" }, { timeoutMs: 60 }), {
      name: "TimeoutError",
    });
    assert.ok(calls < 4, "The retry cannot obtain a fresh operation budget");
  } finally {
    globalThis.fetch = original;
  }
});

test("caller cancellation aborts an in-flight MCP call without reminting", async () => {
  const original = globalThis.fetch;
  const controller = new AbortController();
  let calls = 0;
  globalThis.fetch = (async (_url, init) => {
    calls++;
    return new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener("abort", () => reject(init.signal?.reason), { once: true });
    });
  }) as typeof fetch;
  try {
    const reason = new Error("caller stopped");
    const pending = proxyConnectorMcp(
      { run_id: "r" },
      "sf",
      { method: "tools/call" },
      {
        sessionToken: "token",
        signal: controller.signal,
        timeoutMs: 1000,
      },
    );
    controller.abort(reason);
    await assert.rejects(pending, (error) => error === reason);
    assert.equal(calls, 1);
  } finally {
    globalThis.fetch = original;
  }
});

test("response body consumption stays inside the session operation deadline", async () => {
  const original = globalThis.fetch;
  globalThis.fetch = (async () => {
    const response = new Response("{}");
    response.json = async () => {
      await new Promise((resolve) => setTimeout(resolve, 80));
      return {};
    };
    return response;
  }) as typeof fetch;
  try {
    await assert.rejects(() => getConnectorSession({ run_id: "r" }, "sf", { timeoutMs: 20 }), { name: "TimeoutError" });
  } finally {
    globalThis.fetch = original;
  }
});

test("pre-aborted calls and invalid deadlines never make a request", async () => {
  const original = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = (async () => {
    calls++;
    return new Response("{}");
  }) as typeof fetch;
  try {
    const controller = new AbortController();
    controller.abort(new Error("already stopped"));
    await assert.rejects(
      () => getConnectorSession({ run_id: "r" }, "sf", { signal: controller.signal }),
      /already stopped/,
    );
    for (const timeoutMs of [0, -1, NaN, Infinity, 1.2, 2 ** 32])
      await assert.rejects(() => getConnectorSession({ run_id: "r" }, "sf", { timeoutMs }), /timeoutMs/);
    for (const name of ["", ".", ".."])
      await assert.rejects(() => getConnectorSession({ run_id: "r" }, name), /connector name/);
    assert.equal(calls, 0);
  } finally {
    globalThis.fetch = original;
  }
});

test("run binding and MCP body are captured before session mint awaits", async () => {
  const original = globalThis.fetch;
  const config = { run_id: "original", generation: 3 };
  const request = { method: "tools/list" };
  let seen: RequestInit | undefined;
  globalThis.fetch = (async (url, init) => {
    if (String(url).endsWith("/session")) {
      config.run_id = "replacement";
      config.generation = 4;
      request.method = "tools/call";
      return new Response(JSON.stringify({ session_token: "token" }));
    }
    seen = init;
    return new Response(JSON.stringify({ result: {} }));
  }) as typeof fetch;
  try {
    await proxyConnectorMcp(config, "sf", request);
    assert.ok(seen);
    const headers = new Headers(seen.headers);
    assert.equal(headers.get(HEADER_RUN_ID), "original");
    assert.equal(headers.get(HEADER_GENERATION), "3");
    assert.equal(JSON.parse(String(seen.body)).method, "tools/list");
  } finally {
    globalThis.fetch = original;
  }
});

test("session minting has a finite default deadline", async () => {
  const original = globalThis.fetch;
  let signal: AbortSignal | null | undefined;
  globalThis.fetch = (async (_url, init) => {
    signal = init?.signal;
    return new Response("{}");
  }) as typeof fetch;
  try {
    await getConnectorSession({ run_id: "r" }, "sf");
    assert.ok(signal instanceof AbortSignal);
    assert.equal(signal.aborted, false);
  } finally {
    globalThis.fetch = original;
  }
});
