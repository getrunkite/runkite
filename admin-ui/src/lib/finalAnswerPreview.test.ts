import assert from "node:assert/strict";
import { test } from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { FinalAnswerText } from "../components/final-answer-text.ts";
import { finalAnswerPreview, MAX_FINAL_ANSWER_PREVIEW_CHARS } from "./finalAnswerPreview.ts";

function output(message: unknown) {
  return { messages: [{ role: "user", content: "Current question" }, message] };
}

test("previews durable Python, LangGraph.js and managed SDK terminal assistant text", () => {
  const cases = [
    { type: "ai", content: "Python answer", tool_calls: [], invalid_tool_calls: [], response_metadata: { finish_reason: "stop" } },
    { type: "ai", data: { content: "LangGraph.js answer", additional_kwargs: {}, response_metadata: { finish_reason: "stop" } } },
    { role: "assistant", content: "Managed Python answer" },
    { role: "ai", content: [{ type: "text", text: "AI SDK " }, { type: "text", text: "answer", providerOptions: {} }] },
    { type: "AIMessage", content: [{ type: "text", text: "Text blocks" }] },
  ];
  for (const message of cases) {
    const result = finalAnswerPreview("success", output(message));
    assert.ok(result);
    assert.match(result.text, /answer|Text blocks/);
    assert.equal(result.truncated, false);
  }
});

test("never turns pending, streaming, failed or interrupted output into a completed answer", () => {
  for (const status of ["pending", "running", "error", "interrupted", "timeout", "future-status", ""]) {
    assert.equal(finalAnswerPreview(status, output({ role: "assistant", content: "Partial text" })), null);
  }
});

test("never searches old history when the last entry is a user, tool or unknown message", () => {
  for (const tail of [
    { role: "user", content: "Current question" },
    { role: "tool", content: "Tool observation" },
    { type: "tool", data: { content: "Nested tool observation" } },
    { role: "system", content: "Instruction" },
    { content: "Unknown role" },
    null,
    "Unattributed text",
  ]) {
    assert.equal(finalAnswerPreview("success", { messages: [
      { role: "user", content: "Prior question" },
      { role: "assistant", content: "Stale answer" },
      tail,
    ] }), null);
  }
});

test("tool-call intent is never presented as a final answer, including hidden metadata", () => {
  for (const extra of [
    { tool_calls: [{ name: "transfer", args: {} }] },
    { invalid_tool_calls: [{ name: "transfer" }] },
    { toolCalls: [{ toolName: "transfer" }] },
    { function_call: { name: "transfer" } },
    { tool_call_id: "previous-tool" },
    { additional_kwargs: { tool_calls: [{ name: "transfer" }] } },
    { response_metadata: { finish_reason: "tool_calls" } },
    { tool_calls: "malformed calls" },
  ]) {
    assert.equal(finalAnswerPreview("success", output({ role: "assistant", content: "I will call a tool", ...extra })), null);
    assert.equal(finalAnswerPreview("success", output({ type: "ai", data: { content: "Nested intent", ...extra } })), null);
  }
});

test("reasoning, incomplete stops, mixed blocks and ambiguous shapes keep raw output only", () => {
  const cases = [
    { role: "assistant", content: "Private thought", channel: "analysis" },
    { role: "assistant", content: "Partial", response_metadata: { finish_reason: "length" } },
    { role: "assistant", content: "Partial", finishReason: "error" },
    { role: "assistant", content: "Partial", response_metadata: { stop_reason: "max_tokens" } },
    { role: "assistant", content: "Future", response_metadata: { finish_reason: "future-reason" } },
    { role: "assistant", content: "Unclear", additional_kwargs: { reasoning_content: "Thought" } },
    { role: "assistant", content: [{ type: "reasoning", text: "Thought" }] },
    { role: "assistant", content: [{ type: "text", text: "Maybe final" }, { type: "tool-call", toolName: "write" }] },
    { role: "assistant", content: [{ type: "text", text: "Maybe final" }, { type: "reasoning", text: "Thought" }] },
    { role: "assistant", content: [{ text: "Untyped block" }] },
    { role: "assistant", content: [{ type: "text", text: 42 }] },
    { role: "assistant", content: { text: "Not a supported content object" } },
    { role: "assistant", type: "tool", content: "Conflicting role" },
    { role: "assistant", content: "Flat", data: { content: "Nested" } },
    { type: "ai", data: { type: "human", content: "Conflicting nested role" } },
    { role: "assistant", content: " ", channel: "final" },
  ];
  for (const message of cases) assert.equal(finalAnswerPreview("success", output(message)), null);
  assert.equal(finalAnswerPreview("success", output({ role: "assistant", content: [
    { type: "text", text: "x".repeat(MAX_FINAL_ANSWER_PREVIEW_CHARS + 10) },
    { type: "reasoning", text: "A later unsupported block must still suppress the preview" },
  ] })), null);
});

test("structured results and arbitrary fields are not guessed into a text answer", () => {
  const value = output({ role: "assistant", content: "An explanation" });
  for (const result of [{ count: 7 }, ["answer"], 7, true, null, "Different typed result"]) {
    assert.equal(finalAnswerPreview("success", { ...value, result }), null);
  }
  assert.deepEqual(finalAnswerPreview("success", { ...value, result: "An explanation" }), { text: "An explanation", truncated: false });
  for (const unknown of [null, false, 7, "Unattributed output", [], {}, { answer: "Guess" }, { result: "Guess" }, { text: "Guess" }, { messages: [] }]) {
    assert.equal(finalAnswerPreview("success", unknown), null);
  }
});

test("malformed objects fail closed without invoking accessors or coercing values", () => {
  let invoked = false;
  const getter = Object.defineProperty({}, "messages", { get() { invoked = true; return [{ role: "assistant", content: "Getter" }]; } });
  assert.equal(finalAnswerPreview("success", getter), null);
  assert.equal(invoked, false);
  assert.equal(finalAnswerPreview("success", Object.create({ messages: [{ role: "assistant", content: "Inherited" }] })), null);
  const broken = new Proxy({}, { getOwnPropertyDescriptor() { throw new Error("Unreadable"); } });
  assert.equal(finalAnswerPreview("success", broken), null);
});

test("preview bounds text without splitting a Unicode surrogate pair", () => {
  const exact = "x".repeat(MAX_FINAL_ANSWER_PREVIEW_CHARS);
  assert.deepEqual(finalAnswerPreview("success", output({ role: "assistant", content: exact })), { text: exact, truncated: false });
  const prefix = "x".repeat(MAX_FINAL_ANSWER_PREVIEW_CHARS - 1);
  const bounded = finalAnswerPreview("success", output({ role: "assistant", content: prefix + "💡tail" }));
  assert.deepEqual(bounded, { text: prefix, truncated: true });
  const markup = renderToStaticMarkup(createElement(FinalAnswerText, { preview: bounded! }));
  assert.match(markup, /Preview truncated/);
  assert.match(markup, /complete stored output.*raw JSON/);
});

test("the actual preview renderer escapes markup and never interprets Markdown links", () => {
  const text = '<script>alert("x")</script><img src=x onerror=alert(1)> [click](javascript:alert(2)) & plain';
  const preview = finalAnswerPreview("success", output({ role: "assistant", content: text }))!;
  assert.equal(preview.text, text);
  const markup = renderToStaticMarkup(createElement(FinalAnswerText, { preview }));
  assert.match(markup, /&lt;script&gt;/);
  assert.match(markup, /&lt;img /);
  assert.match(markup, /&amp; plain/);
  assert.doesNotMatch(markup, /<script|<img|<a\b|href=|Preview truncated/);
});
