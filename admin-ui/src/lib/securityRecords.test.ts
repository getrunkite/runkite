import assert from "node:assert/strict";
import test from "node:test";
import type { CredentialLease } from "../api/types.ts";
import { actionDeliveriesPath, actionDeliveryRetryPath, credentialLeaseHref, credentialLeasePath, leaseControls, leaseStateLabel, leaseTime, reviewCredentialLeasePath } from "./securityRecords.ts";

const ref = `cred-${"a".repeat(56)}`;
test("credential paths require explicit tenant and keep operations outside the query", () => {
  const url = new URL(credentialLeasePath("tenant&x=1", "record/雪?", "revoke"), "https://example.test");
  assert.equal(url.pathname, `/credential-leases/${encodeURIComponent("record/雪?")}/revoke`);
  assert.equal(url.searchParams.get("tenant_id"), "tenant&x=1");
  assert.equal(url.searchParams.get("x"), null);
  for (const value of ["", " "]) assert.throws(() => credentialLeasePath(value, ref), /explicit tenant/);
  assert.throws(() => credentialLeasePath("tenant", undefined, "renew"), /identity/);
  assert.throws(() => credentialLeasePath("tenant", ".."), /identity/);
});
test("only known lease references link, and Review never receives URL tenant selection", () => {
  assert.equal(credentialLeaseHref(ref, "tenant"), `/admin/credential-leases/${ref}?tenant_id=tenant`);
  assert.equal(credentialLeaseHref(ref, "tenant", true), `/admin/review/credential-leases/${ref}`);
  assert.equal(reviewCredentialLeasePath(ref), `/credential-leases/${ref}`);
  for (const bad of [undefined, null, "vault:private/provider-handle", `cred-${"A".repeat(56)}`, `${ref}\n`, `cred-${"a".repeat(55)}`]) assert.equal(credentialLeaseHref(bad, "tenant"), null);
  assert.equal(credentialLeaseHref(ref, ""), null);
});
test("delivery filters and retry preserve tenant and exact IDs without tool execution routes", () => {
  const url = new URL(actionDeliveriesPath("t?a", "dead_letter", "sink&one", 100, 50), "https://example.test");
  assert.equal(url.searchParams.get("tenant_id"), "t?a"); assert.equal(url.searchParams.get("sink_id"), "sink&one"); assert.equal(url.searchParams.get("status"), "dead_letter"); assert.equal(url.searchParams.get("offset"), "100");
  const retry = new URL(actionDeliveryRetryPath("tenant?two", "delivery/雪?"), "https://example.test");
  assert.equal(retry.pathname, `/action-deliveries/${encodeURIComponent("delivery/雪?")}/retry`); assert.equal(retry.searchParams.get("tenant_id"), "tenant?two");
  for (const offset of [-1, 10001, 1.5, Infinity]) assert.throws(() => actionDeliveriesPath("t", "", "", offset));
  assert.throws(() => actionDeliveriesPath("t", "unknown")); assert.throws(() => actionDeliveriesPath("t", "", "", 0, 101));
});
test("renewal controls require active renewable unexpired owner authority; revocation is never inferred", () => {
  const active = { state: "active", renewable: true, cleanup_requested: false, expires_at: "2030-01-01T00:00:00Z" } as CredentialLease;
  const now = Date.parse("2026-01-01");
  assert.deepEqual(leaseControls(active, now), { canRenew: true, canRevoke: true });
  for (const state of ["issuance_unknown", "renewal_unknown", "revocation_unknown", "revoked", "not_issued"]) assert.equal(leaseControls({ ...active, state }, now).canRenew, false);
  assert.equal(leaseControls({ ...active, cleanup_requested: true }, now).canRenew, false);
  assert.equal(leaseControls({ ...active, expires_at: "invalid" }, now).canRenew, false);
  assert.equal(leaseControls({ ...active, expires_at: "2025-01-01" }, now).canRenew, false);
  assert.equal(leaseControls({ ...active, state: "revoked" }, now).canRevoke, false);
});

test("unknown lifecycle states and absent deadlines never imply successful issuance", () => {
  assert.equal(leaseStateLabel("issuance_unknown"), "Issuance unknown");
  assert.equal(leaseStateLabel("revocation_unknown"), "Revocation unknown");
  assert.equal(leaseStateLabel("new_unrecognized_state"), "Unrecognized state");
  for (const value of [undefined, "", "invalid", "0001-01-01T00:00:00Z"]) assert.equal(leaseTime(value), "Not established");
});
