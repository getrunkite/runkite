import assert from "node:assert/strict";
import test from "node:test";
import { createManagedChatRoutes, type ChatOperation } from "./routes.ts";

const threadId = "owned-thread";
const runId = "owned-run";
const messageId = `${runId}:g1:assistant`;
const json = (value: unknown) => Response.json(value);

function fixture() {
  const calls: {
    path: string;
    method: string;
    body?: unknown;
    key: string | null;
  }[] = [];
  const operations: ChatOperation[] = [];
  const bound: string[] = [];
  const fetcher: typeof fetch = async (input, init) => {
    const request = new Request(input, init);
    const path = new URL(request.url).pathname;
    calls.push({
      path,
      method: request.method,
      body:
        request.method === "POST" && path.endsWith("/runs")
          ? await request.json()
          : undefined,
      key: request.headers.get("Authorization"),
    });
    if (path.endsWith("/cancel")) return new Response(null, { status: 204 });
    if (path.endsWith("/wait"))
      return json({
        run: { run_id: runId, thread_id: threadId, status: "success" },
        values: { ai_sdk: { generation: 1, message_id: messageId } },
      });
    if (path.endsWith("/stream")) {
      const chunks = [
        { type: "start", messageId },
        { type: "text-start", id: "text" },
        { type: "text-delta", id: "text", delta: "Answer" },
        { type: "text-end", id: "text" },
        { type: "finish", finishReason: "stop" },
      ];
      const body =
        chunks
          .map(
            (chunk, index) =>
              `id: ${runId}_evt_${index + 1}\nevent: custom:ai_sdk\ndata: ${JSON.stringify(chunk)}\n\n`,
          )
          .join("") +
        `id: ${runId}_evt_6\nevent: end\ndata: {"status":"success"}\n\n`;
      return new Response(body, {
        headers: { "Content-Type": "text/event-stream" },
      });
    }
    return json({ run_id: runId, thread_id: threadId, status: "running" });
  };
  const options = {
    baseUrl: "https://plane.example",
    apiKey: "server-test-key",
    fetch: fetcher,
    resolveTurn: async (_request: Request, operation: ChatOperation) => {
      operations.push(operation);
      return {
        threadId,
        runId,
        agentId: "owned-agent",
        expectedMessageId: messageId,
        onMessageStart: async (id: string) => {
          bound.push(id);
        },
      };
    },
  };
  return {
    calls,
    operations,
    bound,
    options,
    routes: createManagedChatRoutes(options),
  };
}

function turnRequest(): Request {
  return new Request("https://app.example/chat", {
    method: "POST",
    body: JSON.stringify({
      id: "browser-chat",
      thread_id: "browser-thread",
      run_id: "browser-run",
      agent_id: "browser-agent",
      profile: "google-adk",
      messages: [
        {
          id: "user",
          role: "user",
          parts: [{ type: "text", text: "Question" }],
        },
      ],
    }),
  });
}

test("route creation uses the owned mapping and keeps its key off the UI stream", async () => {
  const env = fixture();
  const response = await env.routes.create(turnRequest());
  const body = await response.text();
  assert.deepEqual(env.operations, ["create"]);
  assert.deepEqual(env.bound, [messageId]);
  const creation = env.calls.find(
    (call) => call.method === "POST" && call.path.endsWith("/runs"),
  )!;
  assert.equal(creation.path, `/threads/${threadId}/runs`);
  assert.deepEqual(creation.body, {
    run_id: runId,
    agent_id: "owned-agent",
    input: { messages: [{ role: "user", content: "Question" }] },
    stream_mode: ["custom:ai_sdk", "values"],
    multitask_strategy: "reject",
  });
  assert.ok(body.includes("Answer"));
  assert.ok(!body.includes("server-test-key"));
  assert.ok(env.calls.every((call) => call.key === "Bearer server-test-key"));
});

test("attach resolves ownership again and never creates a new run", async () => {
  const env = fixture();
  const response = await env.routes.attach(
    new Request("https://app.example/stream?run_id=browser-run"),
  );
  assert.ok((await response.text()).includes("Answer"));
  assert.deepEqual(env.operations, ["attach"]);
  assert.ok(
    env.calls.every(
      (call) => call.method === "GET" && call.path.includes(runId),
    ),
  );
  assert.deepEqual(env.bound, [messageId]);
});

test("Stop is POST-only and cancels the owned run", async () => {
  const env = fixture();
  const wrong = await env.routes.stop(new Request("https://app.example/stop"));
  assert.equal(wrong.status, 405);
  assert.equal(wrong.headers.get("Allow"), "POST");
  assert.equal(env.operations.length, 0);
  const response = await env.routes.stop(
    new Request("https://app.example/stop?run_id=browser-run", {
      method: "POST",
    }),
  );
  assert.equal(response.status, 204);
  assert.deepEqual(env.operations, ["stop"]);
  assert.deepEqual(
    env.calls.map((call) => [call.method, call.path]),
    [
      ["GET", `/threads/${threadId}/runs/${runId}`],
      ["POST", `/threads/${threadId}/runs/${runId}/cancel`],
    ],
  );
});

test("an application denial or no-active-run response never calls the plane", async () => {
  const env = fixture();
  for (const status of [204, 401, 403, 404]) {
    const routes = createManagedChatRoutes({
      ...env.options,
      resolveTurn: async () => new Response(null, { status }),
    });
    const response = await routes.attach(
      new Request("https://app.example/stream"),
    );
    assert.equal(response.status, status);
  }
  assert.equal(env.calls.length, 0);
});

test("route settings are captured before application resolution awaits", async () => {
  const env = fixture();
  env.options.baseUrl = "https://replacement.invalid";
  env.options.apiKey = "replacement-key";
  const response = await env.routes.create(turnRequest());
  await response.text();
  assert.ok(env.calls.every((call) => call.key === "Bearer server-test-key"));
});
