import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { NoteStore } from "./state.ts";

test("approved continuation is idempotent across concurrency and restart", async () => {
  const directory = await mkdtemp(join(tmpdir(), "runkite-example-"));
  try {
    const path = join(directory, "state.json");
    const store = new NoteStore(path);
    const args = { title: "A draft", body: "Original approved content" };
    await store.addDecision(
      {
        id: "job",
        turnId: "turn",
        args,
        actionId: "action",
        status: "pending",
      },
      false,
    );
    await Promise.all([
      store.settle("job", "approved"),
      store.settle("job", "approved"),
    ]);
    const restarted = new NoteStore(path);
    assert.deepEqual(await restarted.settle("job", "approved"), {
      id: "job",
      turnId: "turn",
      args,
      actionId: "action",
      status: "done",
      result: { noteId: "job" },
    });
    assert.deepEqual((await restarted.read()).notes, { job: args });
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test("rejected and consumed approvals never create notes", async () => {
  const directory = await mkdtemp(join(tmpdir(), "runkite-example-"));
  try {
    const store = new NoteStore(join(directory, "state.json"));
    for (const status of ["rejected", "consumed"] as const) {
      await store.addDecision(
        {
          id: status,
          turnId: "turn",
          args: { title: "x", body: "x" },
          status: "pending",
        },
        false,
      );
      assert.equal((await store.settle(status, status)).status, "denied");
    }
    assert.deepEqual((await store.read()).notes, {});
    assert.equal(await store.claimTurn("turn", "same prompt"), true);
    assert.equal(await store.claimTurn("turn", "same prompt"), false);
    await assert.rejects(store.claimTurn("turn", "different prompt"));
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
