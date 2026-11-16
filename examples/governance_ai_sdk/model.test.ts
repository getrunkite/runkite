import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { createOpenAI } from "@ai-sdk/openai";
import { generateText } from "ai";
import { createGovernanceClient } from "runkite-governance";
import { createApplicationModel, SCRIPTED_NOTE } from "./model.ts";
import { NoteStore, type NoteJob } from "./state.ts";
import { createNoteTool } from "./tools.ts";

const refuseProvider: typeof createOpenAI = () => {
  throw new Error("The scripted path must not construct a model provider");
};

test("scripted mode is explicit and does not require provider configuration", async () => {
  const configured = await createApplicationModel(
    { RUNKITE_DEMO_MODEL: "scripted" },
    refuseProvider,
  );
  assert.equal(configured.mode, "scripted");
  assert.notEqual(typeof configured.model, "string");
  if (typeof configured.model !== "string") {
    assert.equal(configured.model.modelId, "scripted-note-proposal");
  }
});

test("provider mode stays the default and missing configuration never falls back to a mock", async () => {
  await assert.rejects(
    createApplicationModel({}, refuseProvider),
    /OPENAI_API_KEY is required/,
  );
  await assert.rejects(
    createApplicationModel({ OPENAI_API_KEY: "unused" }, refuseProvider),
    /MODEL is required/,
  );
});

test("configured provider mode keeps the application's selected model", async () => {
  let constructed = 0;
  const configured = await createApplicationModel(
    { OPENAI_API_KEY: "unused", MODEL: "application-selected-model" },
    (settings) => {
      constructed++;
      assert.equal(settings?.apiKey, "unused");
      return createOpenAI(settings);
    },
  );
  assert.equal(constructed, 1);
  assert.equal(configured.mode, "provider");
  assert.notEqual(typeof configured.model, "string");
  if (typeof configured.model !== "string") {
    assert.equal(configured.model.modelId, "application-selected-model");
  }
});

test("an unknown demo mode fails rather than invoking a provider", async () => {
  await assert.rejects(
    createApplicationModel({ RUNKITE_DEMO_MODEL: "typo" }, refuseProvider),
    /must be provider or scripted/,
  );
});

for (const effect of ["allow", "deny", "pending", "outage"] as const) {
  test(`scripted model retains the native guarded ${effect} path without a provider`, async (t) => {
    const directory = await mkdtemp(join(tmpdir(), "runkite-scripted-note-"));
    t.after(() => rm(directory, { recursive: true, force: true }));
    const store = new NoteStore(join(directory, "state.json"));
    const requests: { path: string; body: unknown }[] = [];
    const client = createGovernanceClient({
      baseUrl: "http://plane",
      fetch: async (url, init) => {
        requests.push({
          path: new URL(String(url)).pathname,
          body: JSON.parse(String(init?.body)),
        });
        return effect === "outage"
          ? Response.json({ message: "plane unavailable" }, { status: 503 })
          : Response.json({
              effect,
              ...(effect === "pending" ? { action_id: "scripted-approval" } : {}),
            });
      },
    });
    const jobs: NoteJob[] = [];
    const { model } = await createApplicationModel(
      { RUNKITE_DEMO_MODEL: "scripted", OPENAI_API_KEY: "unused", MODEL: "unused" },
      refuseProvider,
    );
    const result = await generateText({
      model,
      prompt: "This prompt is deliberately not interpreted by a model provider.",
      tools: {
        createNote: createNoteTool({
          client,
          store,
          turnId: "scripted-turn",
          onJob: (job) => jobs.push(job),
        }),
      },
      maxRetries: 0,
    });
    assert.match(result.text, /Scripted local demo/);
    assert.equal(requests.length, 1);
    assert.equal(requests[0].path, "/governance/decide");
    const { run_id: operationId, ...proposal } = requests[0].body as {
      run_id: string;
      [key: string]: unknown;
    };
    assert.match(operationId, /^[a-f0-9]{64}$/);
    assert.deepEqual(
      proposal,
      {
        agent_id: "notes-app",
        connector: "workspace",
        tool: "createNote",
        args: SCRIPTED_NOTE,
      },
    );
    const state = await store.read();
    if (effect === "outage") {
      assert.equal(jobs.length, 0);
      assert.equal(result.toolResults.length, 0);
      assert.equal(result.content.filter((part) => part.type === "tool-error").length, 1);
      assert.deepEqual(state.jobs, {});
      assert.deepEqual(state.notes, {});
    } else {
      assert.equal(jobs.length, 1);
      assert.equal(operationId, jobs[0].id);
      assert.equal(jobs[0].status, effect === "allow" ? "done" : effect === "deny" ? "denied" : "pending");
      assert.deepEqual(jobs[0].args, SCRIPTED_NOTE);
      assert.deepEqual(state.notes, effect === "allow" ? { [jobs[0].id]: SCRIPTED_NOTE } : {});
      if (effect === "pending") {
        assert.equal(jobs[0].actionId, "scripted-approval");
        const settled = await store.settle(jobs[0].id, "approved");
        const repeated = await store.settle(jobs[0].id, "approved");
        assert.deepEqual(repeated, settled);
        assert.equal(Object.keys((await store.read()).notes).length, 1);
      }
    }
  });
}
