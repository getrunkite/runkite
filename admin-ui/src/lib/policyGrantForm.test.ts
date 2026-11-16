import { test } from "node:test";
import assert from "node:assert/strict";
import type { AdminPolicyGrant } from "../api/types.ts";
import { bodyFromForm, emptyForm, formFromGrant } from "./policyGrantForm.ts";

const grant: AdminPolicyGrant = {
  id: " grant/id ",
  tenant_id: " tenant ",
  agent_id: " agent ",
  connector: " connector ",
  tools: {
    allow: ["read", " read ", "comma,name", "two words"],
    deny: [" delete ", "another,tool"],
  },
};

test("editing a grant preserves exact scope, row ID and wire tool names", () => {
  const submitted = JSON.parse(JSON.stringify(bodyFromForm(formFromGrant(grant))));
  assert.deepEqual(submitted, grant);
  assert.equal(`/policy-grants/${encodeURIComponent(submitted.id)}`, "/policy-grants/%20grant%2Fid%20");
});

test("a new positive grant does not retarget padded identities or split tool names", () => {
  const form = { ...emptyForm(), id: grant.id, tenant_id: grant.tenant_id, agent_id: grant.agent_id, connector: grant.connector,
    allow: '[" read ", "comma,name", "two words"]' };
  const submitted = bodyFromForm(form);
  assert.equal(submitted.tenant_id, grant.tenant_id);
  assert.equal(submitted.agent_id, grant.agent_id);
  assert.equal(submitted.connector, grant.connector);
  assert.deepEqual(submitted.tools?.allow, [" read ", "comma,name", "two words"]);
});

test("ambiguous legacy tool text is refused rather than silently granting split names", () => {
  for (const allow of ["read,write", "read write", '["read", 7]', '[""]', '{"allow":["read"]}'])
    assert.throws(() => bodyFromForm({ ...emptyForm(), allow }), /JSON array of nonempty strings/);
});

test("empty tool lists keep the existing unrestricted-filter meaning", () => {
  for (const raw of ["", "  ", "[]"])
    assert.equal(bodyFromForm({ ...emptyForm(), allow: raw, deny: raw }).tools, undefined);
});
