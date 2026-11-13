import assert from "node:assert/strict";
import { test } from "node:test";
import { createReviewClient, ReviewError } from "./client.ts";
import { getCSRFToken, setCSRFToken } from "../api/client.ts";
import type { ReviewSessionStatus, ReviewPendingAction } from "./contracts.ts";

function status(tenant = "tenant-a", approve = false): ReviewSessionStatus {
  return { authenticated: true, auth_required: true, configured: true, identity: "reviewer", tenant_id: tenant, can_read: true, can_approve: approve, csrf_token: "review-csrf", expires_at: new Date(Date.now() + 15 * 60_000).toISOString() };
}
const pending = { id: "pending-v2-exact/雪?", tenant_id: "tenant-a", status: "pending", args_digest: "bound", run_incarnation_id: "db-proof", target_revision: `target-v1-${"a".repeat(64)}` } as ReviewPendingAction;

test("Review uses its own API, sends a credential once and never borrows Admin CSRF", async () => {
  const old = globalThis.fetch;
  const seen: { url: string; init: RequestInit }[] = [];
  globalThis.fetch = async (url, init) => { seen.push({ url: String(url), init: init! }); return Response.json(seen.length === 1 ? status("tenant-a", true) : pending); };
  try {
    setCSRFToken("admin-csrf");
    const client = createReviewClient();
    await client.login("synthetic-personal-review-key");
    await client.resolve(pending, "approve");
    assert.deepEqual(seen.map((item) => item.url), ["/review-api/session", "/review-api/pending-actions/pending-v2-exact%2F%E9%9B%AA%3F/approve"]);
    assert.deepEqual(JSON.parse(seen[0].init.body as string), { credential: "synthetic-personal-review-key" });
    assert.equal(seen[1].init.body, undefined);
    assert.equal(new Headers(seen[1].init.headers).get("X-CSRF-Token"), "review-csrf");
    assert.equal(new Headers(seen[1].init.headers).get("Authorization"), null);
    assert.equal(getCSRFToken(), "admin-csrf");
    for (const item of seen) { assert.equal(item.init.credentials, "include"); assert.equal(item.init.redirect, "error"); assert.ok(item.init.signal); }
  } finally { globalThis.fetch = old; setCSRFToken(null); }
});

test("unconfigured Review stays closed and missing verified tenant cannot authenticate", async () => {
  const old = globalThis.fetch;
  const client = createReviewClient();
  globalThis.fetch = async () => Response.json({ ...status(), authenticated: false, configured: false, csrf_token: "", expires_at: null });
  try {
    await client.status();
    assert.equal(client.snapshot().configured, false);
    assert.equal(client.snapshot().identity, null);
    await assert.rejects(client.getWithMeta("/connector-actions"), (err: unknown) => err instanceof ReviewError && err.status === 401);
    globalThis.fetch = async () => Response.json({ ...status(), tenant_id: "" });
    await assert.rejects(client.login("test"), /explicit tenant/);
    assert.equal(client.snapshot().identity, null);
  } finally { globalThis.fetch = old; }
});

test("read-only role cannot issue approve or deny even when calling the helper directly", async () => {
  const old = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = async () => { calls++; return Response.json(status()); };
  try {
    const client = createReviewClient(); await client.login("test");
    await assert.rejects(client.resolve(pending, "approve"), /read-only/);
    await assert.rejects(client.resolve(pending, "deny"), /read-only/);
    await assert.rejects(client.getWithMeta("/../admin-api/runs"), /Unsupported Review endpoint/);
    assert.equal(calls, 1);
  } finally { globalThis.fetch = old; }
});

test("foreign approval and legacy managed authority are rejected before mutation", async () => {
  const old = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = async () => { calls++; return Response.json(status("tenant-a", true)); };
  try {
    const client = createReviewClient(); await client.login("test");
    await assert.rejects(client.resolve({ ...pending, tenant_id: "tenant-b" }, "deny"), /outside your Review tenant/);
    await assert.rejects(client.resolve({ ...pending, run_incarnation_id: undefined }, "approve"), /fresh review/);
    await assert.rejects(client.resolve({ ...pending, target_revision: undefined }, "approve"), /connector target revision/);
    await assert.rejects(client.resolve({ ...pending, target_revision: "malformed" }, "approve"), /connector target revision/);
    await assert.rejects(client.resolve({ ...pending, expires_at: "2020-01-01T00:00:00Z" }, "approve"), /expired/);
    assert.equal(calls, 1);
  } finally { globalThis.fetch = old; }
});

test("stored target binding cannot predict server drift and a 409 is never retried", async () => {
  const old = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = async () => { calls++; return calls === 1 ? Response.json(status("tenant-a", true)) : Response.json({ message: "connector target changed; resubmit for fresh review" }, { status: 409 }); };
  try {
    const client = createReviewClient(); await client.login("test");
    await assert.rejects(client.resolve(pending, "approve"), (err: unknown) => err instanceof ReviewError && err.status === 409 && /target changed/.test(err.message));
    assert.equal(calls, 2);
    assert.ok(client.snapshot().identity);
  } finally { globalThis.fetch = old; }
});

test("401 expiry clears scoped identity while 403 leaves the session distinct from denied data", async () => {
  const old = globalThis.fetch;
  const client = createReviewClient();
  globalThis.fetch = async () => Response.json(status());
  try {
    await client.login("test");
    globalThis.fetch = async () => Response.json({ message: "not allowed" }, { status: 403 });
    await assert.rejects(client.getWithMeta("/pending-actions"), (err: unknown) => err instanceof ReviewError && err.status === 403);
    assert.ok(client.snapshot().identity);
    globalThis.fetch = async () => Response.json({ message: "expired" }, { status: 401 });
    await assert.rejects(client.getWithMeta("/pending-actions"), (err: unknown) => err instanceof ReviewError && err.status === 401);
    assert.equal(client.snapshot().identity, null);
  } finally { globalThis.fetch = old; }
});

test("a response crossing absolute session expiry cannot release scoped data", async () => {
  const old = globalThis.fetch;
  const now = Date.now;
  const login = status();
  const client = createReviewClient();
  let complete!: (response: Response) => void;
  globalThis.fetch = async () => Response.json(login);
  try {
    await client.login("test");
    globalThis.fetch = () => new Promise((resolve) => { complete = resolve; });
    const result = client.getWithMeta("/connector-actions");
    Date.now = () => Date.parse(login.expires_at!) + 1;
    complete(Response.json([{ tenant_id: "tenant-a" }]));
    await assert.rejects(result, /expired Review response/);
    assert.equal(client.snapshot().identity, null);
  } finally { globalThis.fetch = old; Date.now = now; }
});

test("late data from an old tenant is discarded after another session signs in", async () => {
  const old = globalThis.fetch;
  const client = createReviewClient();
  globalThis.fetch = async () => Response.json(status());
  let complete!: (response: Response) => void;
  try {
    await client.login("test-a");
    globalThis.fetch = () => new Promise((resolve) => { complete = resolve; });
    const stale = client.getWithMeta("/connector-actions");
    globalThis.fetch = async () => Response.json(status("tenant-b"));
    await client.login("test-b");
    complete(Response.json([{ tenant_id: "tenant-a" }]));
    await assert.rejects(stale);
    assert.equal(client.snapshot().identity?.tenant_id, "tenant-b");
  } finally { globalThis.fetch = old; }
});

test("request timeout aborts a late response and tenant mismatch never reaches a view", async () => {
  const old = globalThis.fetch;
  const originalTimeout = AbortSignal.timeout;
  const timeout = new AbortController();
  const client = createReviewClient();
  globalThis.fetch = async () => Response.json(status());
  try {
    await client.login("test");
    let complete!: (response: Response) => void;
    AbortSignal.timeout = () => timeout.signal;
    globalThis.fetch = () => new Promise((resolve) => { complete = resolve; });
    const late = client.getWithMeta("/connector-actions");
    timeout.abort(new DOMException("timed out", "TimeoutError"));
    complete(Response.json([{ tenant_id: "tenant-a" }]));
    await assert.rejects(late, (err: unknown) => err instanceof DOMException && err.name === "TimeoutError");
    AbortSignal.timeout = originalTimeout;
    globalThis.fetch = async () => Response.json([{ tenant_id: "tenant-b" }]);
    await assert.rejects(client.getWithMeta("/connector-actions"), /unexpected tenant scope/);
  } finally { globalThis.fetch = old; AbortSignal.timeout = originalTimeout; }
});

test("logout preserves a usable session on failure and clears only Review on success", async () => {
  const old = globalThis.fetch;
  const client = createReviewClient();
  globalThis.fetch = async () => Response.json(status());
  try {
    setCSRFToken("still-admin"); await client.login("test");
    globalThis.fetch = async () => new Response(null, { status: 503 });
    await assert.rejects(client.logout()); assert.ok(client.snapshot().identity);
    globalThis.fetch = async () => new Response(null, { status: 204 });
    await client.logout(); assert.equal(client.snapshot().identity, null);
    assert.equal(getCSRFToken(), "still-admin");
  } finally { globalThis.fetch = old; setCSRFToken(null); }
});
