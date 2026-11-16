import { test } from "node:test";
import assert from "node:assert/strict";
import type { AdminRegistryEntry } from "../api/types.ts";
import { api, setCSRFToken } from "../api/client.ts";
import { jsonBodyFromForm, emptyForm, formFromEntry } from "./registryEntryForm.ts";

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
