import { test } from "node:test";
import assert from "node:assert/strict";
import type { AdminRegistryEntry } from "../api/types.ts";
import { api, setCSRFToken } from "../api/client.ts";
import { jsonBodyFromForm, emptyForm, formFromEntry, registryEntryFormKey, registryEntrySaveRequest } from "./registryEntryForm.ts";

const entry: AdminRegistryEntry = {
  tenant_id: "acme",
  name: "sales-qualifier",
  display_name: "Sales Qualifier",
  description: "Qualifies inbound leads",
  author: "alice",
  tags: ["sales", "lead-gen"],
  source_type: "git",
  source_ref: "https://github.com/example/sales-qualifier@main",
  metadata_json: "null",
  version: 7,
  created_at: "2026-11-01T10:00:00Z",
  updated_at: "2026-11-16T10:00:00Z",
};

const rawMetadata = `{
  "unknown.vendor/contract": {
    "large": 9007199254740993,
    "decimal": 1.0000000000000001,
    "nested": [9007199254740993, {"decimal": 2.0000000000000001}, null, false, ""]
  },
  "__proto__": {"opaque": true},
  "empty": {}
}`;

test("visible registry edits send the exact loaded metadata fragment through the real fetch wrapper", async () => {
  const original = globalThis.fetch;
  let seen: RequestInit | undefined;
  let url: unknown;
  globalThis.fetch = async (input, init) => {
    url = input;
    seen = init;
    return Response.json(entry);
  };
  try {
    setCSRFToken("registry-csrf");
    // This is the normal response.json path. Parsed metadata has already lost
    // precision before formFromEntry is called; only metadata_json is safe.
    const response = Response.json({ ...entry, metadata: JSON.parse(rawMetadata), metadata_json: rawMetadata });
    const loaded = await response.json() as AdminRegistryEntry;
    assert.equal((loaded.metadata!["unknown.vendor/contract"] as Record<string, unknown>).decimal, 1);
    const form = formFromEntry(loaded);
    form.description = "Updated description";
    form.tags = "sales, qualified";
    await api.putRawJSON("/registry/sales-qualifier?tenant_id=acme", jsonBodyFromForm(form));

    assert.equal(url, "/admin-api/registry/sales-qualifier?tenant_id=acme");
    assert.equal(seen?.method, "PUT");
    assert.equal(seen?.credentials, "include");
    assert.equal(seen?.redirect, "error");
    assert.equal(new Headers(seen?.headers).get("Content-Type"), "application/json");
    assert.equal(new Headers(seen?.headers).get("X-CSRF-Token"), "registry-csrf");
    assert.equal((seen!.body as string).slice((seen!.body as string).indexOf(',"metadata":') + 12, -1), rawMetadata);
    const submitted = JSON.parse(seen!.body as string);
    assert.equal(submitted.description, "Updated description");
    assert.deepEqual(submitted.tags, ["sales", "qualified"]);
    assert.equal(Object.hasOwn(submitted.metadata, "__proto__"), true);
    assert.equal(Object.hasOwn(submitted, "metadata_json"), false);
  } finally {
    globalThis.fetch = original;
    setCSRFToken(null);
  }
});

test("unchanged writable fields and an explicitly empty metadata object survive", () => {
  const submitted = JSON.parse(jsonBodyFromForm(formFromEntry({ ...entry, metadata: {}, metadata_json: "{}" })));
  assert.deepEqual(submitted, {
    name: entry.name,
    display_name: entry.display_name,
    description: entry.description,
    author: entry.author,
    tags: entry.tags,
    source_type: entry.source_type,
    source_ref: entry.source_ref,
    metadata: {},
  });
});

test("new entries omit metadata while loaded null metadata remains null", () => {
  const form = emptyForm();
  form.name = "new-specialist";
  form.source_ref = "./specialist";
  assert.equal(Object.hasOwn(JSON.parse(jsonBodyFromForm(form)), "metadata"), false);
  assert.equal(JSON.parse(jsonBodyFromForm(formFromEntry(entry))).metadata, null);
});

test("an older replica cannot trigger a save using rounded parsed metadata", () => {
  const { metadata_json: _omitted, ...oldResponse } = entry;
  assert.throws(() => jsonBodyFromForm(formFromEntry({ ...oldResponse, metadata: JSON.parse(rawMetadata) })), /Reload.*updated server/);
  assert.throws(() => jsonBodyFromForm(formFromEntry(oldResponse)), /Reload.*updated server/);
});

test("raw metadata accepts only one JSON object or null and cannot escape its property", () => {
  for (const metadata_json of ['{} , "source_ref":"changed"', '{} {}', '[]', '1', '"text"', '{']) {
    assert.throws(() => jsonBodyFromForm(formFromEntry({ ...entry, metadata_json })), /Reload/);
  }
  const form = formFromEntry({ ...entry, metadata_json: '{"x":1e400}' });
  assert.ok(jsonBodyFromForm(form).endsWith(',"metadata":{"x":1e400}}'));
});

test("the explicit raw write rejects invalid JSON before fetching", () => {
  assert.throws(() => api.putRawJSON("/registry/example", '{} {}'), SyntaxError);
});

test("existing saves keep exact padded record identity on the actual shared-client wire", async () => {
  const original = globalThis.fetch;
  let seenURL: string | undefined;
  let seenBody: string | undefined;
  globalThis.fetch = async (url, init) => {
    seenURL = String(url);
    seenBody = init?.body as string;
    return Response.json(entry);
  };
  try {
    for (const [name, tenant_id] of [[" entry ", "tenant"], ["entry", " tenant "], [" entry ", " tenant "], ["entry", "  "], ["new", " tenant "]]) {
      const form = formFromEntry({ ...entry, name, tenant_id, metadata_json: rawMetadata });
      form.description = "visible edit";
      const request = registryEntrySaveRequest(form, "edit", name, tenant_id);
      await api.putRawJSON(request.path, request.body);
      const wire = new URL(seenURL!, "https://plane.example");
      assert.equal(decodeURIComponent(wire.pathname.split("/").at(-1)!), name);
      assert.equal(wire.searchParams.get("tenant_id"), tenant_id);
      assert.equal(JSON.parse(seenBody!).name, name);
      assert.equal(JSON.parse(seenBody!).description, "visible edit");
      assert.ok(seenBody!.endsWith(`,"metadata":${rawMetadata}}`));
    }
  } finally {
    globalThis.fetch = original;
  }
});

test("a stale or unhydrated existing form cannot overwrite a newly selected record", () => {
  const loaded = formFromEntry(entry);
  assert.throws(() => registryEntrySaveRequest(loaded, "edit", "another-entry", entry.tenant_id), /selected record changed/);
  assert.throws(() => registryEntrySaveRequest(loaded, "edit", entry.name, "another-tenant"), /selected record changed/);
  assert.throws(() => registryEntrySaveRequest(loaded, "edit", entry.name, ""), /selected record changed/);
  assert.throws(() => registryEntrySaveRequest({ ...loaded, metadata_json: undefined }, "edit", entry.name, entry.tenant_id), /selected record changed/);
  const selectedDefault = formFromEntry({ ...entry, tenant_id: "default" });
  assert.ok(registryEntrySaveRequest(selectedDefault, "edit", entry.name, "").path.endsWith("?tenant_id=default"));
});

test("new entry creation retains its existing slug cleanup and exact tenant selection", () => {
  const form = { ...emptyForm(), name: " new-entry ", tenant_id: " tenant ", source_ref: "{}" };
  const request = registryEntrySaveRequest(form, "create", undefined, " tenant ");
  assert.equal(request.name, "new-entry");
  assert.equal(request.path, "/registry/new-entry?tenant_id=%20tenant%20");
  assert.equal(JSON.parse(request.body).name, "new-entry");
});

test("route and exact tenant navigation use distinct form remount keys", () => {
  const selections: Array<[string, string | undefined]> = [
    ["entry", undefined], ["entry", ""], ["entry", "default"], ["entry", "tenant"],
    ["entry", " tenant "], [" entry ", "tenant"], ["a:b", "c"], ["a", "b:c"],
  ];
  const keys = selections.map(([name, tenant]) => registryEntryFormKey("edit", name, tenant));
  assert.equal(new Set(keys).size, selections.length);
});
