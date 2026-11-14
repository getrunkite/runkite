import test from "node:test";
import assert from "node:assert/strict";
import { identityEvidenceRows } from "./identityEvidence.ts";

test("legacy action does not fabricate identity evidence", () => assert.deepEqual(identityEvidenceRows({}), []));
test("identity evidence keeps authentication, actor assertions and operator intent distinct", () => {
  const rows = identityEvidenceRows({ identity_provenance: { issuer: "https://issuer.invalid", subject: "alice", audience: "runkite", expires_at: "2026-11-14T10:00:00Z", issuer_asserted_actors: [{ subject: "service" }] }, workload_identity: { uri: "spiffe://example.invalid/worker", key_sha256: "a".repeat(64), not_after: "2026-11-14T11:00:00Z" }, delegation_contract: { scopes: ["files.read"], audiences: ["files-api"], configured_actor: "registered-service", max_ttl_seconds: 300 } });
  assert.ok(rows.some((r) => r.label === "Issuer-asserted actors"));
  assert.ok(rows.some((r) => r.label === "Operator-configured actor"));
  assert.ok(rows.some((r) => r.label === "Dispatch public-key SHA256"));
  assert.ok(!rows.some((r) => /approved|executed|currently verified/i.test(r.label)));
});
test("arbitrary extra credential fields never become display rows", () => {
  const input = { identity_provenance: { issuer: "issuer", subject: "alice", audience: "runkite", expires_at: "expiry", access_token: "must-not-render" }, access_token: "must-not-render" };
  assert.ok(!JSON.stringify(identityEvidenceRows(input)).includes("must-not-render"));
});
