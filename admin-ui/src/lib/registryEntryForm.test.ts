import { test } from "node:test";
import assert from "node:assert/strict";
import type { AdminRegistryEntry } from "../api/types.ts";
import { bodyFromForm, emptyForm, formFromEntry } from "./registryEntryForm.ts";

const entry: AdminRegistryEntry = {
  tenant_id: "acme",
  name: "sales-qualifier",
  display_name: "Sales Qualifier",
  description: "Qualifies inbound leads",
  author: "alice",
  tags: ["sales", "lead-gen"],
  source_type: "git",
  source_ref: "https://github.com/example/sales-qualifier@main",
  version: 7,
  created_at: "2026-11-01T10:00:00Z",
  updated_at: "2026-11-16T10:00:00Z",
};

test("editing visible registry fields preserves nested and unknown metadata in the PUT body", () => {
  const metadata = JSON.parse(`{
    "unknown.vendor/contract": {
      "version": 3,
      "nested": [{"enabled": false, "values": [null, 0, "", true]}]
    },
    "__proto__": {"opaque": true},
    "empty": {}
  }`);
  const form = formFromEntry({ ...entry, metadata });
  form.description = "Updated description";
  form.tags = "sales, qualified";

  const submitted = JSON.parse(JSON.stringify(bodyFromForm(form)));
  assert.deepEqual(submitted.metadata, metadata);
  assert.equal(submitted.description, "Updated description");
  assert.deepEqual(submitted.tags, ["sales", "qualified"]);
  assert.equal(submitted.metadata["unknown.vendor/contract"].nested[0].enabled, false);
  assert.equal(Object.hasOwn(submitted.metadata, "__proto__"), true);
});

test("an unchanged registry form roundtrips every writable field including metadata", () => {
  const metadata = { manifest: { revision: "abc123", connectors: ["crm"] } };
  const submitted = JSON.parse(JSON.stringify(bodyFromForm(formFromEntry({ ...entry, metadata }))));

  assert.deepEqual(submitted, {
    name: entry.name,
    display_name: entry.display_name,
    description: entry.description,
    author: entry.author,
    tags: entry.tags,
    source_type: entry.source_type,
    source_ref: entry.source_ref,
    metadata,
  });
});

test("a new registry entry omits metadata when the form has none", () => {
  const form = emptyForm();
  form.name = "new-specialist";
  form.source_ref = "./specialist";

  const body = bodyFromForm(form);
  assert.equal(Object.hasOwn(form, "metadata"), false);
  assert.equal(Object.hasOwn(body, "metadata"), false);
  assert.equal(Object.hasOwn(JSON.parse(JSON.stringify(body)), "metadata"), false);
});

test("an existing entry without metadata does not gain a metadata field", () => {
  const form = formFromEntry(entry);
  assert.equal(Object.hasOwn(form, "metadata"), false);
  assert.equal(Object.hasOwn(bodyFromForm(form), "metadata"), false);
});

test("an explicitly empty metadata object remains present in the PUT body", () => {
  const submitted = bodyFromForm(formFromEntry({ ...entry, metadata: {} }));
  assert.equal(Object.hasOwn(submitted, "metadata"), true);
  assert.deepEqual(submitted.metadata, {});
});
