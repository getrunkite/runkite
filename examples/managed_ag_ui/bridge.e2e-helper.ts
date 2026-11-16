/** Called by the existing runner qualification with a real public Runkite API. */
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { HttpAgent, EventType, type Event, type Message } from "@ag-ui/client";
import { EventSchema } from "@ag-ui/core/schemas";
import { createManagedAGUIRoutes } from "./routes.ts";
import { reconstructManagedAGUI } from "./replay.ts";

type LiveOptions = {
  baseUrl: string;
  threadId: string;
  agentId: string;
  apiKey: string;
};

function ownedApplication(options: LiveOptions) {
  // A qualification stand-in for an app's persisted authenticated turn record.
  // The IDs are allocated before the create route can dispatch anything.
  const runId = randomUUID();
  const appThreadId = randomUUID();
  const appRunId = randomUUID();
  let assistantMessageId: string | undefined;
  const calls: Array<{ path: string; method: string }> = [];
  const routes = createManagedAGUIRoutes({
    baseUrl: options.baseUrl,
    apiKey: options.apiKey,
    timeoutMs: 10_000,
    fetch: async (url, init) => {
      const request = new Request(url, init);
      calls.push({
        path: new URL(request.url).pathname,
        method: request.method,
      });
      return fetch(request);
    },
    async resolveTurn(request) {
      if (request.headers.get("Cookie") !== "app-session=owned")
        return new Response(null, { status: 401 });
      return {
        threadId: options.threadId,
        runId,
        agentId: options.agentId,
        profile: "ai-sdk",
        aguiThreadId: appThreadId,
        aguiRunId: appRunId,
        expectedMessageId: assistantMessageId,
        async onMessageStart(messageId) {
          assert.equal(messageId, `${runId}:g1:assistant`);
          if (assistantMessageId !== undefined)
            assert.equal(messageId, assistantMessageId);
          await Promise.resolve();
          assistantMessageId = messageId;
        },
      };
    },
  });
  const appFetch = async (url: string, init: RequestInit) => {
    const request = new Request(url, init);
    const response =
      request.method === "GET"
        ? await routes.attach(request)
        : await routes.create(request);
    assert.equal(
      JSON.stringify([...response.headers]).includes(options.apiKey),
      false,
    );
    return response;
  };
  const initialMessages: Message[] = [
    { id: randomUUID(), role: "user", content: "One owned AG-UI text turn" },
  ];
  const client = (attach = false) =>
    new HttpAgent({
      url: "http://application.example/api/agui",
      threadId: appThreadId,
      initialMessages,
      headers: { Cookie: "app-session=owned" },
      fetch: (url, init) =>
        appFetch(
          url,
          attach ? { ...init, method: "GET", body: undefined } : init,
        ),
    });
  return {
    runId,
    appThreadId,
    appRunId,
    calls,
    routes,
    appFetch,
    client,
    get assistantMessageId() {
      return assistantMessageId;
    },
  };
}

export async function verifyManagedAGUI(options: LiveOptions): Promise<string> {
  const app = ownedApplication(options);
  const agent = app.client();
  const events: Event[] = [];
  await agent.runAgent(
    { runId: app.appRunId },
    {
      onEvent({ event }) {
        events.push(EventSchema.parse(event));
      },
    },
  );
  const expectedText = "AG-UI text 🌍 e\u0301 भारत";
  const messageId = `${app.runId}:g1:assistant`;
  assert.equal(app.assistantMessageId, messageId);
  assert.equal(
    agent.messages.find((message) => message.id === messageId)?.content,
    expectedText,
  );
  assert.equal(
    events.filter((event) => event.type === EventType.RUN_FINISHED).length,
    1,
  );
  assert.equal(
    events.some(
      (event) =>
        event.type === EventType.RUN_ERROR || event.type.startsWith("TOOL_"),
    ),
    false,
  );
  assert.equal(events.at(-1)?.type, EventType.RUN_FINISHED);
  const saved: Message[] = [
    ...agent.messages.filter((message) => message.id !== messageId),
    { id: messageId, role: "assistant", content: "partial" },
  ];
  const beforeReplay = app.calls.length;
  const replayed = await reconstructManagedAGUI({
    attachUrl: "http://application.example/api/agui/attach",
    threadId: app.appThreadId,
    runId: app.appRunId,
    assistantMessageId: messageId,
    messages: saved,
    fetch: app.appFetch,
    headers: { Cookie: "app-session=owned" },
  });
  assert.equal(
    replayed.filter((message) => message.id === messageId).length,
    1,
  );
  assert.equal(
    replayed.find((message) => message.id === messageId)?.content,
    expectedText,
  );
  assert.equal(saved.at(-1)?.content, "partial");
  assert.ok(
    app.calls.slice(beforeReplay).every((call) => call.method === "GET"),
  );
  assert.equal(
    app.calls.filter(
      (call) => call.method === "POST" && call.path.endsWith("/runs"),
    ).length,
    1,
  );
  const durable = await fetch(
    `${options.baseUrl}/threads/${options.threadId}/runs/${app.runId}`,
    {
      headers: { Authorization: `Bearer ${options.apiKey}` },
      signal: AbortSignal.timeout(10_000),
    },
  );
  assert.equal(durable.ok, true);
  const run = await durable.json();
  assert.equal(run.status, "success");
  assert.equal(run.output.ai_sdk.message_id, messageId);
  assert.equal(run.output.messages.at(-1).content[0].text, expectedText);
  return app.runId;
}

export async function verifyManagedAGUIDetachAndStop(
  options: LiveOptions & {
    assertDetached: (runId: string) => Promise<void>;
  },
): Promise<string> {
  const app = ownedApplication(options);
  for (const attach of [false, true]) {
    const agent = app.client(attach);
    let observedText = false;
    await agent
      .runAgent(
        { runId: app.appRunId },
        {
          onTextMessageContentEvent() {
            observedText = true;
            agent.abortRun();
          },
        },
      )
      .catch((error: unknown) => {
        if (!observedText) throw error;
      });
    assert.equal(observedText, true);
    assert.equal(app.assistantMessageId, `${app.runId}:g1:assistant`);
    assert.equal(
      app.calls.some((call) => call.path.endsWith("/cancel")),
      false,
    );
    await options.assertDetached(app.runId);
  }
  const stopped = await app.routes.stop(
    new Request("http://application.example/api/agui/stop", {
      method: "POST",
      headers: { Cookie: "app-session=owned" },
    }),
  );
  assert.equal(stopped.status, 204);
  assert.equal(
    app.calls.filter((call) => call.path.endsWith("/cancel")).length,
    1,
  );
  assert.equal(
    app.calls.filter(
      (call) => call.method === "POST" && call.path.endsWith("/runs"),
    ).length,
    1,
  );
  return app.runId;
}
