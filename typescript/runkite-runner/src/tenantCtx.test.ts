/**
 * Self-check for tenantCtx.ts's per-run AsyncLocalStorage binding.
 * TypeScript mirror of python/tests/test_tenant_ctx.py.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  checkpointThreadId,
  currentTenant,
  HEADER_GENERATION,
  HEADER_RUN_ID,
  HEADER_TENANT_ID,
  runWithBinding,
  runWithTenant,
  tenantHeaders,
} from "./tenantCtx.js";

test("default tenant is 'default'", () => {
  assert.equal(currentTenant(), "default");
  assert.equal(tenantHeaders()[HEADER_TENANT_ID], "default");
});

test("runWithTenant binds for the duration of the callback", async () => {
  await runWithTenant("acme", async () => {
    assert.equal(currentTenant(), "acme");
    assert.equal(tenantHeaders()[HEADER_TENANT_ID], "acme");
  });
  assert.equal(currentTenant(), "default");
});

test("empty / whitespace binds as 'default'", async () => {
  await runWithTenant("  ", async () => {
    assert.equal(currentTenant(), "default");
  });
});

test("concurrent jobs keep distinct tenants", async () => {
  const seen: string[] = [];
  await Promise.all(
    ["a", "b", "c"].map((tid) =>
      runWithTenant(tid, async () => {
        await new Promise((r) => setTimeout(r, 10));
        seen.push(currentTenant());
      }),
    ),
  );
  assert.deepEqual(seen.sort(), ["a", "b", "c"]);
});

test("checkpointThreadId encodes non-default tenants", () => {
  assert.equal(checkpointThreadId(undefined, "t1"), "t1");
  assert.equal(checkpointThreadId("default", "t1"), "t1");
  assert.equal(checkpointThreadId("  ", "t1"), "t1");
  assert.equal(checkpointThreadId("acme", "t1"), "acme:t1");
});

test("runWithBinding adds run id + generation headers", async () => {
  assert.equal(tenantHeaders()[HEADER_RUN_ID], undefined);
  await runWithBinding({ tenantId: "acme", runId: "run-1", generation: 3 }, async () => {
    const h = tenantHeaders();
    assert.equal(h[HEADER_TENANT_ID], "acme");
    assert.equal(h[HEADER_RUN_ID], "run-1");
    assert.equal(h[HEADER_GENERATION], "3");
  });
  assert.equal(tenantHeaders()[HEADER_RUN_ID], undefined);
});
