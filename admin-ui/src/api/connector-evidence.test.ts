import assert from "node:assert/strict";
import { test } from "node:test";
import { evidenceForPath } from "../lib/connectorActions.ts";
import { pollRequest } from "./poll-request.ts";

test("scope switch hides previous evidence and ignores a late obsolete response", async () => {
  let resolveOld!: (value: string[]) => void;
  const oldResponse = new Promise<string[]>((resolve) => {
    resolveOld = resolve;
  });
  let state = { path: "old-tenant/old-run", data: ["old evidence"] };
  let oldSignal: AbortSignal | undefined;
  const stopOld = pollRequest(
    (signal) => {
      oldSignal = signal;
      return oldResponse;
    },
    (data) => {
      state = { path: "old-tenant/old-run", data };
    },
    () => {},
    () => {},
  );
  const currentPath = "new-tenant/new-run";
  assert.equal(evidenceForPath(state, currentPath), null);
  stopOld();
  assert.equal(oldSignal?.aborted, true);
  const stopNew = pollRequest(
    async () => ["new evidence"],
    (data) => {
      state = { path: currentPath, data };
    },
    () => {},
    () => {},
  );
  await Promise.resolve();
  resolveOld(["late old evidence"]);
  await Promise.resolve();
  await Promise.resolve();
  assert.deepEqual(evidenceForPath(state, currentPath), ["new evidence"]);
  stopNew();
});
