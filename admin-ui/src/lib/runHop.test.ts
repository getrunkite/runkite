import { test } from "node:test";
import assert from "node:assert/strict";
import { hopPhase, hopToolLabel, parseToolAuth, pendingPageHref } from "./runHop.ts";

test("hopPhase: pending wins over busy", () => {
  assert.equal(hopPhase({ busy: true, connectorPending: true, effect: null }), "held");
  assert.equal(hopPhase({ busy: false, connectorPending: false, effect: "pending" }), "held");
});

test("hopPhase: deny, running, idle", () => {
  assert.equal(hopPhase({ busy: false, connectorPending: false, effect: "deny" }), "denied");
  assert.equal(hopPhase({ busy: true, connectorPending: false, effect: null }), "running");
  assert.equal(hopPhase({ busy: false, connectorPending: false, effect: null }), "idle");
});

test("hopToolLabel uses last tool_auth, else pending_agent default", () => {
  assert.equal(hopToolLabel({ agentId: "echo_agent", connector: "sf", tool: "update" }), "sf.update");
  assert.equal(hopToolLabel({ agentId: "pending_agent", connector: null, tool: null }), "bank.refund");
  assert.equal(hopToolLabel({ agentId: "echo_agent", connector: null, tool: null }), "tool");
});

test("pendingPageHref encodes run and tenant", () => {
  assert.equal(pendingPageHref({}), "/admin/pending");
  assert.equal(
    pendingPageHref({ runId: "abc", tenantId: "default" }),
    "/admin/pending?tenant_id=default&run_id=abc",
  );
});

test("parseToolAuth reads plane payload and JSON string bodies", () => {
  const live = parseToolAuth({
    effect: "pending",
    connector: "bank",
    tool: "refund",
    action_id: "pending-1",
  });
  assert.deepEqual(live, {
    effect: "pending",
    connector: "bank",
    tool: "refund",
    actionId: "pending-1",
  });
  const encoded = parseToolAuth(
    JSON.stringify({ effect: "deny", connector: "sf", tool: "updateRecord" }),
  );
  assert.equal(encoded?.effect, "deny");
  assert.equal(encoded?.connector, "sf");
  assert.equal(parseToolAuth(null), null);
  assert.equal(parseToolAuth({}), null);
});
