import { test } from "node:test";
import assert from "node:assert/strict";
import { callAgent, A2AError } from "./a2a.js";

/** Mocks the global fetch for the duration of one test, capturing the
 * single request made and returning fn's response. Node's fetch is a
 * plain global (not an import), so reassigning globalThis.fetch is
 * sufficient -- no extra mocking dependency, same "inspect the actual
 * outgoing request without a live control plane" approach as Python's
 * test_a2a.py uses httpx.MockTransport for. */
function mockFetchOnce(status: number, body: unknown): { calls: Array<{ url: string; init: RequestInit }>; restore: () => void } {
  const original = globalThis.fetch;
  const calls: Array<{ url: string; init: RequestInit }> = [];
  globalThis.fetch = (async (url: any, init: any) => {
    calls.push({ url: String(url), init });
    return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  return { calls, restore: () => { globalThis.fetch = original; } };
}

test("callAgent throws when config has no configurable.run_id", async () => {
  await assert.rejects(() => callAgent({ configurable: {} }, "other_agent", { messages: [] }), /configurable\.run_id/);
});

test("callAgent throws when config itself is null/undefined", async () => {
  await assert.rejects(() => callAgent(undefined, "other_agent", {}), /configurable\.run_id/);
});

test("callAgent posts to /internal/a2a/runs with the correct body, headers, and forwards on_behalf_of from langgraph_auth_user.toDict()", async () => {
  const mock = mockFetchOnce(200, { run_id: "child-run", status: "pending" });
  const originalToken = process.env.RUNNER_TOKEN;
  process.env.RUNNER_TOKEN = "test-token";

  try {
    const config = {
      configurable: {
        run_id: "parent-run-123",
        langgraph_auth_user: { toDict: () => ({ identity: "alice", email: "alice@example.com" }) },
      },
    };
    const result = await callAgent(config, "worker_agent", { messages: [{ role: "human", content: "do the thing" }] }, {
      wait: false,
      controlPlaneUrl: "http://fake-control-plane:2026",
    });

    assert.equal(mock.calls.length, 1);
    assert.equal(mock.calls[0].url, "http://fake-control-plane:2026/internal/a2a/runs");
    const body = JSON.parse(mock.calls[0].init.body as string);
    assert.equal(body.agent_id, "worker_agent");
    assert.equal(body.parent_run_id, "parent-run-123");
    assert.equal(body.wait, false);
    assert.deepEqual(body.on_behalf_of, { identity: "alice", email: "alice@example.com" });
    const headers = mock.calls[0].init.headers as Record<string, string>;
    assert.equal(headers["X-Runner-Kind"], "typescript-langgraphjs");
    assert.equal(headers["X-Runner-Token"], "test-token");
    assert.deepEqual(result, { run_id: "child-run", status: "pending" });
  } finally {
    mock.restore();
    if (originalToken === undefined) delete process.env.RUNNER_TOKEN;
    else process.env.RUNNER_TOKEN = originalToken;
  }
});

test("callAgent omits on_behalf_of when config has no authenticated user", async () => {
  const mock = mockFetchOnce(200, { run_id: "child-run" });
  try {
    await callAgent({ configurable: { run_id: "parent-run-456" } }, "worker_agent", {}, { wait: false });
    const body = JSON.parse(mock.calls[0].init.body as string);
    assert.ok(!("on_behalf_of" in body), "expected on_behalf_of to be omitted when no authenticated user in config");
  } finally {
    mock.restore();
  }
});

test("callAgent omits on_behalf_of when langgraph_auth_user has no toDict method", async () => {
  const mock = mockFetchOnce(200, { run_id: "child-run" });
  try {
    await callAgent(
      { configurable: { run_id: "parent-run-456", langgraph_auth_user: { identity: "alice" } } },
      "worker_agent",
      {},
      { wait: false },
    );
    const body = JSON.parse(mock.calls[0].init.body as string);
    assert.ok(!("on_behalf_of" in body));
  } finally {
    mock.restore();
  }
});

test("callAgent throws A2AError on a non-2xx response, with the response body in the message", async () => {
  const mock = mockFetchOnce(400, { message: "a2a delegation depth 11 exceeds max_depth 10" });
  try {
    await assert.rejects(
      () => callAgent({ configurable: { run_id: "parent-run" } }, "worker_agent", {}, { wait: false }),
      (err: unknown) => {
        assert.ok(err instanceof A2AError);
        assert.match((err as Error).message, /depth/);
        return true;
      },
    );
  } finally {
    mock.restore();
  }
});

test("callAgent includes thread_id and run_config in the request body when provided", async () => {
  const mock = mockFetchOnce(200, { run_id: "child-run" });
  try {
    await callAgent({ configurable: { run_id: "parent-run" } }, "worker_agent", {}, {
      wait: false,
      threadId: "existing-thread",
      runConfig: { recursion_limit: 5 },
    });
    const body = JSON.parse(mock.calls[0].init.body as string);
    assert.equal(body.thread_id, "existing-thread");
    assert.deepEqual(body.config, { recursion_limit: 5 });
  } finally {
    mock.restore();
  }
});

test("callAgent defaults wait to true when not specified", async () => {
  const mock = mockFetchOnce(200, { run: {}, values: {} });
  try {
    await callAgent({ configurable: { run_id: "parent-run" } }, "worker_agent", {});
    const body = JSON.parse(mock.calls[0].init.body as string);
    assert.equal(body.wait, true);
  } finally {
    mock.restore();
  }
});
