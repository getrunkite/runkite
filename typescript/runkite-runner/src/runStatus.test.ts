import assert from "node:assert/strict";
import test from "node:test";
import { shouldSkipRun } from "./runStatus.js";
import { runWithBinding } from "./tenantCtx.js";

test("shouldSkipRun returns true for interrupted", async () => {
  const orig = globalThis.fetch;
  globalThis.fetch = (async () =>
    new Response(JSON.stringify({ status: "interrupted" }), { status: 200 })) as typeof fetch;
  try {
    assert.equal(await shouldSkipRun("http://cp:2026", "run-1"), true);
  } finally {
    globalThis.fetch = orig;
  }
});

test("shouldSkipRun returns false for pending", async () => {
  const orig = globalThis.fetch;
  globalThis.fetch = (async () => new Response(JSON.stringify({ status: "pending" }), { status: 200 })) as typeof fetch;
  try {
    assert.equal(await shouldSkipRun("http://cp:2026", "run-1"), false);
  } finally {
    globalThis.fetch = orig;
  }
});

test("shouldSkipRun fail-open on HTTP 503", async () => {
  const orig = globalThis.fetch;
  globalThis.fetch = (async () => new Response("busy", { status: 503 })) as typeof fetch;
  try {
    assert.equal(await shouldSkipRun("http://cp:2026", "run-1"), false);
  } finally {
    globalThis.fetch = orig;
  }
});

test("shouldSkipRun returns true for 404", async () => {
  const orig = globalThis.fetch;
  globalThis.fetch = (async () => new Response("gone", { status: 404 })) as typeof fetch;
  try {
    assert.equal(await shouldSkipRun("http://cp:2026", "run-1"), true);
  } finally {
    globalThis.fetch = orig;
  }
});

test("shouldSkipRun preserves the assignment tenant instead of querying default", async () => {
  const original = globalThis.fetch;
  let tenant: string | null = null;
  globalThis.fetch = (async (_input, init) => {
    const headers = new Headers(init?.headers);
    tenant = headers.get("X-Runkite-Tenant-Id");
    assert.equal(headers.get("X-Runner-Kind"), "typescript-ai-sdk");
    assert.equal(headers.get("X-Runner-Token"), "runner-token");
    return tenant === "acme"
      ? new Response(JSON.stringify({ status: "running" }), { status: 200 })
      : new Response("not found in default", { status: 404 });
  }) as typeof fetch;
  try {
    const skip = await runWithBinding({ tenantId: "acme", runId: "run-acme", generation: 2 }, () =>
      shouldSkipRun("http://cp:2026", "run-acme", {
        runnerKind: "typescript-ai-sdk",
        runnerToken: "runner-token",
      }),
    );
    assert.equal(skip, false, "an executable non-default run must not be discarded");
    assert.equal(tenant, "acme");
  } finally {
    globalThis.fetch = original;
  }
});

test("shouldSkipRun keeps a custom run ID inside one URL path component", async () => {
  const original = globalThis.fetch;
  let requested = "";
  globalThis.fetch = (async (input) => {
    requested = String(input);
    return new Response(JSON.stringify({ status: "running" }), { status: 200 });
  }) as typeof fetch;
  try {
    const id = "run/child?status=success#fragment%tail";
    assert.equal(await shouldSkipRun("http://cp:2026/", id), false);
    assert.equal(requested, `http://cp:2026/internal/runs/${encodeURIComponent(id)}/status`);
    const url = new URL(requested);
    assert.equal(url.search, "");
    assert.equal(url.hash, "");
    assert.equal(decodeURIComponent(url.pathname.split("/")[3]), id);
  } finally {
    globalThis.fetch = original;
  }
});
