import assert from "node:assert/strict";
import test from "node:test";
import { shouldSkipRun } from "./runStatus.js";

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
  globalThis.fetch = (async () =>
    new Response(JSON.stringify({ status: "pending" }), { status: 200 })) as typeof fetch;
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
