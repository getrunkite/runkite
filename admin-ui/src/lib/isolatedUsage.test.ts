import test from "node:test";
import assert from "node:assert/strict";
import { isolatedUsageView } from "./isolatedUsage.ts";

const base = { reporting_source: "isolated_workload", reporting_status: "reported", complete: true, sequence: 1, prompt_tokens: 0, completion_tokens: 0 };
test("reported zero remains different from unknown and omitted cost", () => {
  assert.deepEqual(isolatedUsageView({ usage: base }), { status: "complete", tokens: "0 input / 0 output", dollars: "USD not reported", reporter: undefined, sequence: 1 });
  const unknown = isolatedUsageView({ usage: { reporting_source: "isolated_workload", reporting_status: "unknown", complete: false, sequence: 1 } });
  assert.equal(unknown?.status, "unknown"); assert.equal(unknown?.tokens, "Not reported");
  assert.equal(isolatedUsageView({ usage: { ...base, cost_reported: true, cost_usd: 0 } })?.dollars, "$0.0000 reported");
});
test("partial reporting and original reporter remain visible", () => {
  const view = isolatedUsageView({ usage: { ...base, complete: false, reported_workload: { uri: "spiffe://example.invalid/original-worker" } } });
  assert.equal(view?.status, "partial"); assert.equal(view?.reporter, "spiffe://example.invalid/original-worker");
});
test("ordinary output and malformed purported measurements do not gain assurance labels", () => {
  for (const value of [null, {}, { usage: { prompt_tokens: 2 } }, { usage: { ...base, prompt_tokens: -1 } }, { usage: { ...base, sequence: 0 } }, { usage: { ...base, completion_tokens: "0" } }]) assert.equal(isolatedUsageView(value), undefined);
});
