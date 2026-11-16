import { test } from "node:test";
import assert from "node:assert/strict";
import {
  adminAgentPath,
  adminAgentSchemasPath,
  adminAgentVersionsPath,
  adminRegistryPath,
  adminRegistryVersionsPath,
  adminRunPath,
  adminThreadPath,
  adminThreadRunsPath,
  adminTenantQuery,
  adminPendingActionHref,
  adminRetainedRunHref,
  adminCronRowId,
} from "./adminPaths.ts";
import { connectorActionRunReferenceMatches } from "./connectorActions.ts";

// Regression: every sub-resource path must place its extra path segment
// (/schemas, /versions) BEFORE the ?tenant_id= query string. Building it by
// string-concatenating a suffix onto an already-queried "base" path lands
// the segment inside the query value instead (e.g.
// "/agents/x?tenant_id=default/schemas"), which the router reads as request
// path "/agents/x" with tenant_id "default/schemas" -- a tenant that does
// not exist, so the backend 404s with "agent not found" even though the
// base agent page loads fine.

test("adminAgentSchemasPath puts /schemas before the query string", () => {
  const p = adminAgentSchemasPath("gemini_langchain", "default");
  assert.equal(p, "/agents/gemini_langchain/schemas?tenant_id=default");
});

test("adminAgentVersionsPath puts /versions before the query string", () => {
  const p = adminAgentVersionsPath("gemini_langchain", "default");
  assert.equal(p, "/agents/gemini_langchain/versions?tenant_id=default");
});

test("adminAgentPath has no query when tenantId is omitted", () => {
  assert.equal(adminAgentPath("echo_agent"), "/agents/echo_agent");
  assert.equal(adminAgentSchemasPath("echo_agent"), "/agents/echo_agent/schemas");
});

test("adminRegistryPath and adminRegistryVersionsPath place /versions before the query too", () => {
  assert.equal(adminRegistryPath("sales-qualifier", "acme"), "/registry/sales-qualifier?tenant_id=acme");
  assert.equal(
    adminRegistryVersionsPath("sales-qualifier", "acme"),
    "/registry/sales-qualifier/versions?tenant_id=acme",
  );
});

test("path segments are URL-encoded", () => {
  assert.equal(adminAgentPath("weird id/slash", "ten ant"), "/agents/weird%20id%2Fslash?tenant_id=ten%20ant");
});

test("tenant selection preserves exact IDs and distinguishes omitted from empty default scope", () => {
  assert.equal(adminTenantQuery(undefined), "");
  assert.equal(adminTenantQuery(""), "?tenant_id=");
  assert.equal(adminTenantQuery("  "), "?tenant_id=%20%20");
  assert.equal(adminTenantQuery(" tenant "), "?tenant_id=%20tenant%20");
  assert.equal(adminTenantQuery("default"), "?tenant_id=default");
  for (const path of [adminAgentPath(" shared ", " tenant "), adminRegistryPath(" entry ", " tenant ")]) {
    const url = new URL(`/admin-api${path}`, "https://plane.example");
    assert.equal(url.searchParams.get("tenant_id"), " tenant ");
    assert.ok(decodeURIComponent(url.pathname).includes(" "));
  }
});

test("run/thread operations keep caller-supplied identifiers inside one URL segment", () => {
  const id = "../other?tenant_id=foreign#fragment%";
  for (const path of [adminRunPath(id), adminThreadPath(id)]) {
    const url = new URL(`/admin-api${path}/cancel`, "https://plane.example");
    assert.equal(url.search, "");
    assert.equal(url.hash, "");
    const parts = url.pathname.split("/");
    assert.equal(parts.length, 5);
    assert.equal(decodeURIComponent(parts[3]), id);
    assert.equal(parts[4], "cancel");
  }
});

test("exact approval links cannot turn opaque IDs into another run, tenant or route", () => {
  const id = "pending/../雪?run_id=reused&tenant_id=foreign#fragment%";
  const url = new URL(adminPendingActionHref(id), "https://plane.example");
  assert.equal(url.pathname.split("/").length, 4);
  assert.equal(url.pathname.split("/")[2], "pending");
  assert.equal(decodeURIComponent(url.pathname.split("/")[3]), id);
  assert.equal(url.search, "");
  assert.equal(url.hash, "");
  assert.equal(adminPendingActionHref("original-approval"), "/admin/pending/original-approval");
});

test("cron thread navigation preserves exact tenant scope on detail, history and deletion paths", () => {
  const thread = "cron:daily/report?tenant_id=foreign#fragment";
  const selectedTenant = " tenant/a&tenant_id=foreign ";
  for (const path of [adminThreadPath(thread, selectedTenant), adminThreadRunsPath(thread, selectedTenant)]) {
    const url = new URL(`/admin-api${path}`, "https://plane.example");
    assert.equal(decodeURIComponent(url.pathname.split("/")[3]), thread);
    assert.equal(url.searchParams.get("tenant_id"), selectedTenant);
    assert.deepEqual([...url.searchParams.keys()], ["tenant_id"]);
    assert.equal(url.hash, "");
  }
  assert.equal(adminThreadRunsPath("cron:daily", "default"), "/threads/cron%3Adaily/runs?tenant_id=default");
  assert.equal(adminThreadPath("cron:daily", ""), "/threads/cron%3Adaily?tenant_id=");
  assert.equal(adminThreadPath("cron:daily"), "/threads/cron%3Adaily");
});

test("Cron and thread history links reject a reused run ID at the actual Run Detail guard", () => {
  const original = { tenant_id: " tenant/A ", run_id: "reused/run?tenant=other", incarnation_id: "stored-original" };
  const href = adminRetainedRunHref(original);
  assert.ok(href);
  const url = new URL(href, "https://plane.example");
  const id = decodeURIComponent(url.pathname.split("/")[3]);
  const accepts = (run: typeof original) => connectorActionRunReferenceMatches(
    run, id, url.searchParams.get("expected_tenant_id"), url.searchParams.get("expected_incarnation_id"),
  );
  assert.equal(accepts(original), true);
  assert.equal(accepts({ ...original, tenant_id: "other-tenant", incarnation_id: "replacement" }), false);
  assert.equal(accepts({ ...original, incarnation_id: "replacement" }), false);
  assert.equal(accepts({ ...original, run_id: "other-run" }), false);
  assert.equal(adminRetainedRunHref({ ...original, incarnation_id: undefined }), null);
  assert.equal(adminRetainedRunHref({ ...original, incarnation_id: "" }), null);
});

test("Cron row identity cannot collide across tenant/name pairs containing colons", () => {
  assert.notEqual(adminCronRowId({ tenant_id: "a:b", name: "c" }), adminCronRowId({ tenant_id: "a", name: "b:c" }));
  assert.equal(adminCronRowId({ name: "c" }), adminCronRowId({ tenant_id: "default", name: "c" }));
  assert.notEqual(adminCronRowId({ tenant_id: " tenant ", name: "c" }), adminCronRowId({ tenant_id: "tenant", name: "c" }));
});

test("retained reference views forward exact read guards and cannot dispatch live or cancel operations", async () => {
  const { adminRunViewPaths } = await import("./adminPaths.ts");
  for (const [tenant, incarnation] of [[" tenant ", "original"], ["tenant", null], [null, "original"], ["", "original"]] as const) {
    const paths = adminRunViewPaths("reused/id", tenant, incarnation);
    assert.equal(paths.retained, true);
    assert.equal(paths.stream, null);
    assert.equal(paths.cancel, null);
    const detail = new URL(paths.detail, "https://plane.example");
    assert.equal(detail.searchParams.get("expected_tenant_id"), tenant);
    assert.equal(detail.searchParams.get("expected_incarnation_id"), incarnation);
  }
  assert.deepEqual(adminRunViewPaths("ordinary", null, null), {
    retained: false, detail: "/runs/ordinary", stream: "/runs/ordinary/stream", cancel: "/runs/ordinary/cancel",
  });
});
