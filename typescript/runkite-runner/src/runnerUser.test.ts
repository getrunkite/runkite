import { test } from "node:test";
import assert from "node:assert/strict";
import { RunnerUser } from "./runnerUser.js";

test("RunnerUser exposes identity/displayName/isAuthenticated/permissions from a flat wire-shaped dict", () => {
  const user = new RunnerUser({
    identity: "alice",
    display_name: "Alice A.",
    is_authenticated: true,
    permissions: ["read", "write"],
  });
  assert.equal(user.identity, "alice");
  assert.equal(user.displayName, "Alice A.");
  assert.equal(user.isAuthenticated, true);
  assert.deepEqual(user.permissions, ["read", "write"]);
});

test("RunnerUser.displayName falls back to identity when display_name is absent", () => {
  const user = new RunnerUser({ identity: "bob" });
  assert.equal(user.displayName, "bob");
});

test("RunnerUser.isAuthenticated defaults to true unless explicitly false", () => {
  assert.equal(new RunnerUser({ identity: "a" }).isAuthenticated, true);
  assert.equal(new RunnerUser({ identity: "a", is_authenticated: false }).isAuthenticated, false);
});

test("RunnerUser.permissions defaults to an empty array, not undefined", () => {
  assert.deepEqual(new RunnerUser({ identity: "a" }).permissions, []);
});

test("RunnerUser exposes provider-specific Extra fields via get()", () => {
  const user = new RunnerUser({ identity: "alice", email: "alice@example.com", sso_token: "xyz" });
  assert.equal(user.get("email"), "alice@example.com");
  assert.equal(user.get("sso_token"), "xyz");
  assert.equal(user.get("nonexistent", "fallback"), "fallback");
  assert.equal(user.has("email"), true);
  assert.equal(user.has("nonexistent"), false);
});

test("RunnerUser flattens a nested legacy extra bag into top-level fields", () => {
  // Defense for in-flight assignments/hand-built fixtures that still
  // nest provider fields under `extra` -- application code expects
  // top-level access (toDict().sso_token), matching Python's
  // RunnerUser's identical _flatten_user_data behavior.
  const user = new RunnerUser({ identity: "alice", extra: { email: "alice@example.com" } });
  assert.equal(user.get("email"), "alice@example.com");
  assert.deepEqual(user.toDict(), { identity: "alice", email: "alice@example.com" });
});

test("RunnerUser: a top-level field always wins over the same key nested in extra", () => {
  const user = new RunnerUser({ identity: "alice", email: "top-level@example.com", extra: { email: "nested@example.com" } });
  assert.equal(user.get("email"), "top-level@example.com");
});

test("RunnerUser.toDict returns a flat, independent copy", () => {
  const source = { identity: "alice", email: "alice@example.com" };
  const user = new RunnerUser(source);
  const dict = user.toDict();
  assert.deepEqual(dict, source);
  dict.identity = "mutated";
  assert.equal(user.identity, "alice", "mutating the returned dict must not affect the RunnerUser itself");
});

test("RunnerUser supports real attribute-style access for Extra fields (user.email), matching Python's __getattr__", () => {
  const user = new RunnerUser({ identity: "alice", email: "alice@example.com", sso_token: "xyz" });
  assert.equal((user as any).email, "alice@example.com");
  assert.equal((user as any).sso_token, "xyz");
  assert.equal((user as any).nonexistent_field, undefined);
});

test("RunnerUser's Proxy still resolves real class members first (attribute access never shadows identity/get/toDict)", () => {
  const user = new RunnerUser({ identity: "alice", get: "should-not-shadow-the-method" });
  assert.equal(user.identity, "alice");
  assert.equal(typeof user.get, "function", "the real get() method must win over a same-named data field");
  assert.equal(user.get("get"), "should-not-shadow-the-method", "the data field is still reachable via get()");
});

test("RunnerUser instances still pass instanceof checks despite being returned via a Proxy", () => {
  const user = new RunnerUser({ identity: "alice" });
  assert.ok(user instanceof RunnerUser);
});

test("RunnerUser handles a null/undefined data argument gracefully", () => {
  assert.equal(new RunnerUser(undefined).identity, "");
  assert.equal(new RunnerUser(null).identity, "");
  assert.deepEqual(new RunnerUser(null).toDict(), {});
});
