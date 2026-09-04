import assert from "node:assert/strict";
import { test } from "node:test";
import { accumulateUsage, usageOrUnmetered, usagePayload } from "./usage.js";

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

test("accumulateUsage captures gateway-reported cost (OpenRouter-style usage.cost)", () => {
  const totals = {};
  accumulateUsage(totals, {
    messages: [
      {
        type: "ai",
        usage_metadata: { input_tokens: 194, output_tokens: 2, cost: 0.00095 },
        response_metadata: { model_name: "openai/gpt-4o-mini" },
      },
    ],
  });
  const u = usagePayload(totals)!;
  assert.equal(u.prompt_tokens, 194);
  assert.ok(Math.abs((u.cost_usd ?? 0) - 0.00095) < 1e-9);
});

test("accumulateUsage sums gateway cost across turns", () => {
  const totals = {};
  accumulateUsage(totals, { messages: [{ type: "ai", usage_metadata: { input_tokens: 1, cost: 0.001 } }] });
  accumulateUsage(totals, { messages: [{ type: "ai", usage_metadata: { input_tokens: 1, cost: 0.002 } }] });
  const u = usagePayload(totals)!;
  assert.ok(Math.abs((u.cost_usd ?? 0) - 0.003) < 1e-9);
});

test("accumulateUsage descends into updates-mode {nodeName: {...}} wrapper", () => {
  // LangGraph.js "updates" mode has no top-level "messages" key -- it is
  // nested one level down under the node name that produced the update.
  // Regression: this used to silently find nothing for stream_modes
  // configured as ["updates"] alone, on both success and interrupted runs.
  const totals = {};
  accumulateUsage(totals, {
    agent: {
      messages: [{ type: "ai", usage_metadata: { input_tokens: 77, output_tokens: 33, total_tokens: 110 } }],
    },
  });
  assert.deepEqual(usagePayload(totals), { prompt_tokens: 77, completion_tokens: 33, total_tokens: 110 });
});

test("no cost field means no cost_usd key (direct provider, no gateway)", () => {
  const totals = {};
  accumulateUsage(totals, { messages: [{ type: "ai", usage_metadata: { input_tokens: 10, output_tokens: 5 } }] });
  const u = usagePayload(totals)!;
  assert.equal("cost_usd" in u, false);
});

test("plain dict reply with no type or usage_metadata is not flagged unmetered", () => {
  // The common shape for a hand-built, non-LLM deterministic reply must
  // not trip the unmetered-AI-message detector -- it never even
  // qualifies as an AI-shaped message under walkMessages's own test.
  const totals = {};
  accumulateUsage(totals, { messages: [{ role: "ai", content: "hardcoded, no LLM involved" }] });
  assert.equal(usagePayload(totals), null);
});

test("AI message with no usage_metadata is flagged unmetered", () => {
  // Simulates a future/unrecognized provider integration: a real
  // AIMessage-shaped reply exists, but nothing in it is extractable in
  // any currently-known shape.
  const totals = {};
  accumulateUsage(totals, { messages: [{ type: "ai", content: "reply from an unseen provider" }] });
  assert.deepEqual(usagePayload(totals), {
    prompt_tokens: 0,
    completion_tokens: 0,
    total_tokens: 0,
    unmetered: true,
  });
});

test("normal usage never carries the unmetered marker", () => {
  const totals = {};
  accumulateUsage(totals, { messages: [{ type: "ai", usage_metadata: { input_tokens: 10, output_tokens: 5 } }] });
  const u = usagePayload(totals)!;
  assert.equal("unmetered" in u, false);
});

test("usageOrUnmetered passes through real usage", () => {
  const real = { prompt_tokens: 5, completion_tokens: 3, total_tokens: 8 };
  assert.deepEqual(usageOrUnmetered(real, true), real);
});

test("usageOrUnmetered flags a real reply with no usage", () => {
  assert.deepEqual(usageOrUnmetered(null, true), {
    prompt_tokens: 0,
    completion_tokens: 0,
    total_tokens: 0,
    unmetered: true,
  });
});

test("usageOrUnmetered stays null when nothing was produced", () => {
  assert.equal(usageOrUnmetered(null, false), null);
});
