import assert from "node:assert/strict";
import { test } from "node:test";
import { matchRoutes, type RouteObject } from "react-router";
import { adminRegistryEntryHref } from "./adminPaths.ts";
import { emptyForm, formFromEntry, registryEntryFormKey, registryEntrySaveRequest } from "./registryEntryForm.ts";
import { registryEntryRoutes, type RegistryEntryMode } from "./registryEntryRoutes.ts";

function matchEntry(location: string) {
  // Use the same route definitions App renders, including React Router's
  // static/parameter ranking and decoding rather than a hand-written matcher.
  const routes: RouteObject[] = [{
    path: "/admin",
    children: registryEntryRoutes.map(({ path, mode }) => ({ path, handle: { mode } })),
  }];
  const matches = matchRoutes(routes, location);
  const match = matches?.at(-1);
  assert.ok(match);
  return { mode: match.route.handle.mode as RegistryEntryMode, name: match.params.name };
}

test("explicit edit routes disambiguate an existing new entry from the legacy creation URL", () => {
  assert.deepEqual(matchEntry("/admin/registry/new"), { mode: "create", name: undefined });
  assert.deepEqual(matchEntry("/admin/registry/new/edit?tenant_id=tenant"), { mode: "edit", name: "new" });
  assert.deepEqual(matchEntry("/admin/registry/existing?tenant_id=tenant"), { mode: "edit", name: "existing" });
  assert.deepEqual(matchEntry("/admin/registry/existing/edit?tenant_id=tenant"), { mode: "edit", name: "existing" });
});

test("list and after-save hrefs always reach edit mode with the exact name and tenant", () => {
  for (const name of ["new", " entry ", "group/entry?view=new#fragment"]) {
    const href = adminRegistryEntryHref(name, " tenant ");
    const url = new URL(href, "https://plane.example");
    assert.equal(url.searchParams.get("tenant_id"), " tenant ");
    assert.deepEqual(matchEntry(href), { mode: "edit", name });
  }
});

test("publishing a new entry named new returns to its unambiguous existing-entry route", () => {
  const create = registryEntrySaveRequest({ ...emptyForm(), name: "new", source_ref: "{}" }, "create", undefined);
  assert.equal(create.path, "/registry/new?tenant_id=default");
  assert.deepEqual(matchEntry(adminRegistryEntryHref(create.name, "default")), { mode: "edit", name: "new" });
});

test("edit mode keeps loaded new metadata and cannot be reused as a creation form", () => {
  const raw = '{"id":9007199254740993,"decimal":1.0000000000000001}';
  const loaded = formFromEntry({
    tenant_id: "tenant", name: "new", source_type: "inline", source_ref: "{}",
    metadata_json: raw, version: 2, created_at: "", updated_at: "",
  });
  const request = registryEntrySaveRequest(loaded, "edit", "new", "tenant");
  assert.equal(request.path, "/registry/new?tenant_id=tenant");
  assert.ok(request.body.endsWith(`,"metadata":${raw}}`));
  assert.throws(() => registryEntrySaveRequest(loaded, "create", undefined, "tenant"), /form mode changed/);
  assert.throws(() => registryEntrySaveRequest({ ...emptyForm(), name: "new" }, "create", "new"), /form mode changed/);
  assert.throws(() => registryEntrySaveRequest({ ...emptyForm(), name: "new" }, "edit", "new"), /selected record changed/);
  assert.notEqual(registryEntryFormKey("create", "new", "tenant"), registryEntryFormKey("edit", "new", "tenant"));
});
