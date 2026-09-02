import assert from "node:assert/strict";
import { test } from "node:test";
import { accumulateUsage, usagePayload } from "./usage.js";

test("accumulateUsage from AIMessage-shaped values", () => {
  const totals = {};
  accumulateUsage(totals, {
    messages: [
      {
        type: "ai",
        usage_metadata: { input_tokens: 10, output_tokens: 5 },
        response_metadata: { model_name: "gpt-4o-mini" },
      },
    ],
  });
  assert.deepEqual(usagePayload(totals), {
    prompt_tokens: 10,
    completion_tokens: 5,
    total_tokens: 15,
    model: "gpt-4o-mini",
  });
});

test("usagePayload returns null when empty", () => {
  assert.equal(usagePayload({}), null);
});
