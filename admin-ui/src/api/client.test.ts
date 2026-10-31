import assert from "node:assert/strict";
import { test } from "node:test";
import { api, ApiError, createSession, destroySession, getCSRFToken, setCSRFToken } from "./client.ts";

test("failed logout preserves the CSRF token until the server confirms session deletion", async () => {
  const original = globalThis.fetch;
  const seen: RequestInit[] = [];
  let status = 403;
  globalThis.fetch = async (_input, init) => {
    seen.push(init!);
    return new Response(null, { status });
  };
  try {
    setCSRFToken("active-session-csrf");
    await assert.rejects(destroySession(), (error: unknown) => error instanceof ApiError && error.status === 403);
    assert.equal(getCSRFToken(), "active-session-csrf");
    assert.equal(new Headers(seen[0].headers).get("X-CSRF-Token"), "active-session-csrf");
    status = 204;
    await destroySession();
    assert.equal(getCSRFToken(), null);
    assert.equal(seen[0].credentials, "include");
    assert.equal(seen[0].method, "DELETE");
    assert.equal(seen[0].redirect, "error");
  } finally {
    globalThis.fetch = original;
    setCSRFToken(null);
  }
});

test("network failure cannot claim that the server session ended", async () => {
  const original = globalThis.fetch;
  globalThis.fetch = async () => { throw new Error("network unavailable"); };
  try {
    setCSRFToken("active-session-csrf");
    await assert.rejects(destroySession(), /network unavailable/);
    assert.equal(getCSRFToken(), "active-session-csrf");
  } finally {
    globalThis.fetch = original;
    setCSRFToken(null);
  }
});

test("Admin GET forwards navigation cancellation while retaining cookie authentication", async () => {
  const original = globalThis.fetch;
  const controller = new AbortController();
  let seen: RequestInit | undefined;
  globalThis.fetch = async (_input, init) => {
    seen = init;
    return Response.json({ status: "success" }, { headers: { "X-Next-Cursor": "next" } });
  };
  try {
    const result = await api.getWithMeta("/runs", controller.signal);
    assert.equal(seen?.signal, controller.signal);
    assert.equal(seen?.credentials, "include");
    assert.equal(seen?.redirect, "error");
    assert.equal(result.nextCursor, "next");
  } finally {
    globalThis.fetch = original;
  }
});

test("login refuses redirects while sending its one-time credential", async () => {
  const original = globalThis.fetch;
  let seen: RequestInit | undefined;
  globalThis.fetch = async (_input, init) => {
    seen = init;
    return Response.json({ authenticated: true, auth_required: true });
  };
  try {
    await createSession("synthetic-test-credential");
    assert.equal(seen?.redirect, "error");
    assert.deepEqual(JSON.parse(seen!.body as string), { credential: "synthetic-test-credential" });
  } finally {
    globalThis.fetch = original;
  }
});

test("malformed structured API errors retain a string status message", async () => {
  const original = globalThis.fetch;
  globalThis.fetch = async () => Response.json({ message: { unexpected: true } }, { status: 503 });
  try {
    await assert.rejects(api.get("/runs"), (error: unknown) =>
      error instanceof ApiError && error.status === 503 && typeof error.message === "string" && error.message.includes("503"));
  } finally {
    globalThis.fetch = original;
  }
});
