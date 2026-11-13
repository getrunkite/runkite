import assert from "node:assert/strict";
import { test } from "node:test";
import {
  connectorActionMatchesRun,
  connectorActionPath,
  connectorActionRunHref,
  connectorActionRunReferenceMatches,
  connectorActionScope,
  connectorActionsHref,
  connectorActionsPath,
  connectorActionState,
  connectorAuthority,
  connectorApprovalQueueHref,
  evidenceForPath,
} from "./connectorActions.ts";

test("response evidence and unknown outcomes never claim business success", () => {
  const response = connectorActionState("response_observed");
  assert.equal(response.label, "Response observed");
  assert.notEqual(response.tone, "success");
  assert.match(response.description, /does not prove/);
  const unknown = connectorActionState("outcome_unknown");
  assert.equal(unknown.tone, "warning");
  assert.match(unknown.description, /may have executed/);
  assert.match(unknown.description, /before.*retry/);
  assert.match(connectorActionState("dispatch_intent").description, /does not prove.*received/);
  for (const state of ["future_state", "__proto__", "constructor", "toString"]) {
    assert.equal(connectorActionState(state).label, "Unrecognized state");
  }
});

test("final authority remains distinct from the recorded decision", () => {
  assert.equal(connectorAuthority("approved_capability"), "Approved capability");
  assert.equal(connectorAuthority("break_glass"), "Break-glass");
  assert.equal(connectorAuthority(undefined), "No authority recorded");
  assert.equal(connectorAuthority("future_authority"), "future_authority");
  const approvals = new URL(connectorApprovalQueueHref("tenant&exact", "run?exact"), "https://plane.example");
  assert.equal(approvals.searchParams.get("status"), "all");
  assert.equal(approvals.searchParams.get("tenant_id"), "tenant&exact");
  assert.equal(approvals.searchParams.get("run_id"), "run?exact");
});

test("action paths and filters preserve exact opaque identifiers", () => {
  const id = "action/../雪?run_id=foreign#x%";
  const url = new URL(`/admin-api${connectorActionPath(id)}`, "https://plane.example");
  assert.equal(url.pathname.split("/").length, 4);
  assert.equal(decodeURIComponent(url.pathname.split("/")[3]), id);
  assert.equal(url.search, "");
  assert.equal(url.hash, "");
  const filters = {
    tenant_id: " Tenant 雪 ",
    run_id: "r?x=1&tenant_id=other",
    connector: "../provider",
    state: "outcome_unknown",
  };
  const list = new URL(connectorActionsPath(filters, "cursor+/=", 5), "https://plane.example");
  for (const [key, value] of Object.entries(filters)) assert.equal(list.searchParams.get(key), value);
  assert.equal(list.searchParams.get("limit"), "5");
  assert.equal(list.searchParams.get("cursor"), "cursor+/=");
  assert.equal(
    new URL(connectorActionsHref(filters), "https://plane.example").searchParams.get("run_id"),
    filters.run_id,
  );
  assert.notEqual(
    connectorActionScope({ run_id: "a", tenant_id: "b" }),
    connectorActionScope({ run_id: "b", tenant_id: "a" }),
  );
});

test("retained evidence requires the exact nonempty database run incarnation", () => {
  const run = {
    tenant_id: "A",
    run_id: "reused",
    incarnation_id: "opaque-db-proof-1",
    created_at: "2026-11-13T00:00:00Z",
  };
  const action = {
    tenant_id: "A",
    run_id: "reused",
    run_incarnation_id: run.incarnation_id,
    run_created_at: run.created_at,
  };
  assert.equal(connectorActionMatchesRun(action, run), true);
  assert.equal(connectorActionMatchesRun(action, { ...run, incarnation_id: "opaque-db-proof-2" }), false);
  assert.equal(connectorActionMatchesRun(action, { ...run, incarnation_id: undefined }), false);
  assert.equal(connectorActionMatchesRun({ ...action, run_incarnation_id: "" }, run), false);
  assert.equal(connectorActionMatchesRun({ ...action, tenant_id: "a" }, run), false);
  assert.equal(connectorActionMatchesRun({ ...action, run_id: "other" }, run), false);
  const sameInstanceWithDisplayTimestamp = { ...action, run_created_at: "2026-11-13T00:00:00.000Z" };
  assert.equal(connectorActionMatchesRun(sameInstanceWithDisplayTimestamp, run), true);
});

test("archived evidence cannot link, and verified links carry the immutable incarnation", () => {
  const action = { run_id: "r/../雪?x=1", tenant_id: "tenant A", run_incarnation_id: "opaque-db-proof-1" };
  assert.equal(connectorActionRunHref(action, false), null);
  assert.equal(connectorActionRunHref({ ...action, run_incarnation_id: "" }, true), null);
  assert.equal(connectorActionRunHref(action, "true" as unknown as boolean), null);
  const url = new URL(connectorActionRunHref(action, true)!, "https://plane.example");
  assert.equal(decodeURIComponent(url.pathname.split("/")[3]), action.run_id);
  assert.equal(url.searchParams.get("expected_tenant_id"), action.tenant_id);
  assert.equal(url.searchParams.get("expected_incarnation_id"), action.run_incarnation_id);
  const run = { run_id: action.run_id, tenant_id: action.tenant_id, incarnation_id: action.run_incarnation_id };
  assert.equal(connectorActionRunReferenceMatches(run, action.run_id, null, null), true);
  assert.equal(connectorActionRunReferenceMatches(run, action.run_id, action.tenant_id, null), false);
  assert.equal(
    connectorActionRunReferenceMatches(run, action.run_id, action.tenant_id, action.run_incarnation_id),
    true,
  );
  assert.equal(
    connectorActionRunReferenceMatches(
      { ...run, incarnation_id: "replacement-db-proof" },
      action.run_id,
      action.tenant_id,
      action.run_incarnation_id,
    ),
    false,
  );
  assert.equal(connectorActionRunReferenceMatches(run, "other", action.tenant_id, action.run_incarnation_id), false);
});

test("old query data is hidden immediately on a scope or cursor change", () => {
  const previous = { path: "/connector-actions?tenant_id=a&run_id=one", data: ["old evidence"] };
  assert.deepEqual(evidenceForPath(previous, previous.path), previous.data);
  assert.equal(evidenceForPath(previous, "/connector-actions?tenant_id=b&run_id=two"), null);
  assert.equal(evidenceForPath(previous, "/connector-actions?tenant_id=a&run_id=one&cursor=next"), null);
});
