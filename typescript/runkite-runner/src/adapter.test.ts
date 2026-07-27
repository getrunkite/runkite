import { test } from "node:test";
import assert from "node:assert/strict";
import { splitGraphPath } from "./adapter.js";

test("splitGraphPath splits on the last-relevant colon", () => {
  assert.deepEqual(splitGraphPath("./graph.ts:graph"), ["./graph.ts", "graph"]);
});

test("splitGraphPath handles nested export names", () => {
  assert.deepEqual(splitGraphPath("src/agent.ts:namedExport"), ["src/agent.ts", "namedExport"]);
});

test("splitGraphPath throws on a path with no colon", () => {
  assert.throws(() => splitGraphPath("./graph.ts"), /expected "path:exportName"/);
});
