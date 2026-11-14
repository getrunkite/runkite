import assert from "node:assert/strict";
import test from "node:test";
import { poolRunnerKind } from "./executionPool.js";

test("pool selection changes the queue identity and preserves trusted defaults", () => {
  assert.equal(poolRunnerKind("typescript-ai-sdk", "finance"), "pool-finance");
  assert.equal(poolRunnerKind("typescript-ai-sdk"), "typescript-ai-sdk");
  for (const name of ["Finance", "*", "a/b", "x".repeat(32)]) {
    assert.throws(() => poolRunnerKind("typescript-ai-sdk", name));
  }
  assert.throws(() => poolRunnerKind("pool-other", "finance"));
});

test("pool worker refuses direct control-plane backend configuration", () => {
  const original = process.env.POSTGRES_DSN;
  process.env.POSTGRES_DSN = "fixture-presence-only";
  try {
    assert.throws(() => poolRunnerKind("typescript-ai-sdk", "finance"));
    assert.equal(poolRunnerKind("typescript-ai-sdk"), "typescript-ai-sdk");
  } finally {
    if (original === undefined) delete process.env.POSTGRES_DSN;
    else process.env.POSTGRES_DSN = original;
  }
});
