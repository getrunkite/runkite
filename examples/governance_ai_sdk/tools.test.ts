import assert from "node:assert/strict";
import { test } from "node:test";
import { mkdtemp, rm } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { generateText } from "ai";
import { MockLanguageModelV4 } from "ai/test";
import {
  createGovernanceClient,
  type GovernanceDecision,
} from "runkite-governance";
import { NoteStore, type NoteJob } from "./state.ts";
import { createNoteTool } from "./tools.ts";

const note = { title: "A draft", body: "The original proposed content" };

function model() {
  return new MockLanguageModelV4({
    doGenerate: {
      content: [
        {
          type: "tool-call",
          toolCallId: "call-one",
          toolName: "createNote",
          input: JSON.stringify(note),
        },
      ],
      finishReason: { unified: "tool-calls", raw: undefined },
      usage: {
        inputTokens: {
          total: 1,
          noCache: 1,
          cacheRead: 0,
          cacheWrite: 0,
        },
        outputTokens: { total: 1, text: 1, reasoning: 0 },
      },
      warnings: [],
    },
  });
}

for (const effect of ["allow", "deny", "pending"] as const) {
  test(`AI SDK executes its native tool with a Runkite ${effect} decision`, async (t) => {
    const directory = await mkdtemp(join(tmpdir(), "runkite-ai-sdk-"));
    t.after(() => rm(directory, { recursive: true, force: true }));
    const path = join(directory, "state.json");
    const store = new NoteStore(path);
    const requests: { path: string; body?: unknown }[] = [];
    const decision: GovernanceDecision = {
      effect,
      ...(effect === "pending" ? { actionId: "approval-one" } : {}),
    };
    const client = createGovernanceClient({
      baseUrl: "http://plane",
      fetch: async (url, init) => {
        const path = new URL(String(url)).pathname;
        requests.push({
          path,
          body: init?.body ? JSON.parse(String(init.body)) : undefined,
        });
        return Response.json(
          path === "/governance/decide"
            ? {
                effect: decision.effect,
                action_id: decision.actionId,
              }
            : { action_id: "approval-one", status: "approved" },
        );
      },
    });
    const jobs: NoteJob[] = [];
    const tools = {
      createNote: createNoteTool({
        client,
        store,
        turnId: "turn-one",
        onJob: (job) => jobs.push(job),
      }),
    };
    const result = await generateText({
      model: model(),
      prompt: "Create the note",
      tools,
      maxRetries: 0,
    });

    assert.equal(result.toolResults.length, 1);
    assert.equal(jobs.length, 1);
    assert.deepEqual(result.toolResults[0].output, jobs[0]);
    assert.equal(requests.length, 1, "the guard must not poll or create a run");
    assert.equal(requests[0].path, "/governance/decide");
    assert.deepEqual(requests[0].body, {
      agent_id: "notes-app",
      connector: "workspace",
      tool: "createNote",
      args: note,
      run_id: jobs[0].id,
    });
    const state = await store.read();
    if (effect === "allow") {
      assert.equal(jobs[0].status, "done");
      assert.deepEqual(state.notes, { [jobs[0].id]: note });
      // The app transaction provides deduplication. The guard itself does
      // not claim that a correlation ID is an execution-idempotency key.
      await generateText({ model: model(), prompt: "Create the note", tools });
      assert.equal(requests.length, 2);
      assert.deepEqual((await store.read()).notes, state.notes);
    } else {
      assert.deepEqual(state.notes, {});
      assert.equal(jobs[0].status, effect === "deny" ? "denied" : "pending");
      if (effect === "pending") {
        assert.equal(state.jobs[jobs[0].id].actionId, "approval-one");
        assert.deepEqual(state.jobs[jobs[0].id].args, note);
        // Model turns and HTTP requests may end before operator approval.
        // Continue only the persisted operation on a later application call.
        const action = await client.action(state.jobs[jobs[0].id].actionId!);
        assert.equal(action.status, "approved");
        const restarted = new NoteStore(path);
        const settled = await restarted.settle(jobs[0].id, action.status);
        assert.equal(settled.status, "done");
        assert.deepEqual((await restarted.read()).notes, {
          [jobs[0].id]: note,
        });
      }
    }
  });
}

test("AI SDK reports a governance outage as a tool error without executing", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "runkite-ai-sdk-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const store = new NoteStore(join(directory, "state.json"));
  let decisions = 0;
  const client = createGovernanceClient({
    baseUrl: "http://plane",
    fetch: async () => {
      decisions++;
      return Response.json({ message: "plane unavailable" }, { status: 503 });
    },
  });
  const result = await generateText({
    model: model(),
    prompt: "Create the note",
    tools: {
      createNote: createNoteTool({ client, store, turnId: "turn-one" }),
    },
    maxRetries: 0,
  });
  assert.equal(decisions, 1);
  assert.equal(result.toolResults.length, 0);
  assert.equal(
    result.content.filter((part) => part.type === "tool-error").length,
    1,
  );
  assert.deepEqual((await store.read()).notes, {});
  assert.deepEqual((await store.read()).jobs, {});
});

test("AI SDK cancellation aborts the decision and never creates the note", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "runkite-ai-sdk-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const store = new NoteStore(join(directory, "state.json"));
  const controller = new AbortController();
  let entered!: () => void;
  const deciding = new Promise<void>((resolve) => (entered = resolve));
  let decisionAborted = false;
  const client = createGovernanceClient({
    baseUrl: "http://plane",
    fetch: async (_url, init) =>
      new Promise<Response>((_resolve, reject) => {
        const signal = init!.signal!;
        const abort = () => {
          decisionAborted = true;
          reject(signal.reason);
        };
        signal.addEventListener("abort", abort, { once: true });
        if (signal.aborted) abort();
        entered();
      }),
  });
  const result = generateText({
    model: model(),
    prompt: "Create the note",
    abortSignal: controller.signal,
    tools: {
      createNote: createNoteTool({ client, store, turnId: "turn-one" }),
    },
    maxRetries: 0,
  });
  await deciding;
  controller.abort(new Error("application turn cancelled"));
  const completed = await result;
  // The SDK exposes the interrupted tool as a tool-error result. The guard
  // still aborts its HTTP request and never invokes application execution.
  assert.equal(completed.toolResults.length, 0);
  assert.equal(
    completed.content.filter((part) => part.type === "tool-error").length,
    1,
  );
  assert.equal(decisionAborted, true);
  assert.deepEqual((await store.read()).notes, {});
  assert.deepEqual((await store.read()).jobs, {});
});
