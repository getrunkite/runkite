import { test } from "node:test";
import assert from "node:assert/strict";
import { extractAssistantText, extractUsage, messageContent, messageRole } from "./agentMessages.ts";

// Real shapes captured live from the actual runners (not guessed) --
// Python/LangGraph and LangGraph.js disagree on where `content` lives, and a
// naive "grab the last message" renderer silently echoed the user's own
// text as if the AI had said it. Both bugs shipped to the dogfood UI and
// went unnoticed for days because nothing exercised these shapes together.

test("extractAssistantText: python flat shape, array content blocks (Gemini)", () => {
  const values = {
    messages: [
      { type: "human", content: "Say hi" },
      { type: "ai", content: [{ type: "text", text: "Hi there! How can I help you today?" }], usage_metadata: {} },
    ],
  };
  assert.equal(extractAssistantText(values), "Hi there! How can I help you today?");
});

test("extractAssistantText: LangGraph.js nested data.content shape", () => {
  const values = {
    messages: [
      { data: { content: "Say hi" }, type: "human" },
      { data: { content: "Hello, how can I assist you today?" }, type: "ai" },
    ],
  };
  assert.equal(extractAssistantText(values), "Hello, how can I assist you today?");
});

test("extractAssistantText: does not echo the user's own message as an assistant reply", () => {
  // Regression for the exact bug seen on the HITL dogfood page: a "values"
  // snapshot that fires before the AI turn lands has the user's message
  // last. A renderer that blindly takes messages[messages.length - 1] shows
  // it as an "ai" bubble.
  const values = { messages: [{ role: "user", content: "Hi" }] };
  assert.equal(extractAssistantText(values), "");
});

test("extractAssistantText: picks the last assistant turn, not the first, across multiple turns", () => {
  const values = {
    messages: [
      { role: "user", content: "turn 1" },
      { role: "ai", content: "reply 1" },
      { role: "user", content: "turn 2" },
      { role: "ai", content: "reply 2" },
    ],
  };
  assert.equal(extractAssistantText(values), "reply 2");
});

test("messageRole normalizes every shape variant seen across runners", () => {
  assert.equal(messageRole({ type: "human" }), "user");
  assert.equal(messageRole({ role: "user" }), "user");
  assert.equal(messageRole({ type: "HumanMessage" }), "user");
  assert.equal(messageRole({ type: "ai" }), "assistant");
  assert.equal(messageRole({ role: "assistant" }), "assistant");
  assert.equal(messageRole({ data: { type: "ai" } }), "assistant");
  assert.equal(messageRole({ data: { role: "assistant" } }), "assistant");
  assert.equal(messageRole({}), "other");
  assert.equal(messageRole(null), "other");
});

test("messageContent handles flat string, nested data.content, and array-of-blocks", () => {
  assert.equal(messageContent({ content: "plain" }), "plain");
  assert.equal(messageContent({ data: { content: "nested" } }), "nested");
  assert.equal(
    messageContent({ content: [{ type: "text", text: "a" }, { type: "text", text: "b" }] }),
    "ab",
  );
  assert.equal(messageContent(null), "");
  assert.equal(messageContent("bare string"), "bare string");
});

test("extractUsage reads a top-level usage object, or null when absent", () => {
  assert.deepEqual(extractUsage({ usage: { prompt_tokens: 5 } }), { prompt_tokens: 5 });
  assert.equal(extractUsage({ messages: [] }), null);
  assert.equal(extractUsage(null), null);
});
