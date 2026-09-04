import { test } from "node:test";
import assert from "node:assert/strict";
import {
  adminAgentPath,
  adminAgentSchemasPath,
  adminAgentVersionsPath,
  adminRegistryPath,
  adminRegistryVersionsPath,
  adminTenantQuery,
} from "./adminPaths.ts";

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

test("adminTenantQuery is empty for missing or blank tenant, otherwise a leading-? param", () => {
  assert.equal(adminTenantQuery(undefined), "");
  assert.equal(adminTenantQuery(""), "");
  assert.equal(adminTenantQuery("  "), "");
  assert.equal(adminTenantQuery("default"), "?tenant_id=default");
});
