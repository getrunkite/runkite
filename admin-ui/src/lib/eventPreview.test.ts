import assert from "node:assert/strict";
import { test } from "node:test";
import { eventPreview } from "./eventPreview.ts";

test("ordinary debugging events remain unchanged", () => {
  const data = '{"id":9007199254740993,"text":"hello"}';
  assert.equal(eventPreview(data), data);
});

test("large event previews stay bounded and disclose truncation without changing the source", () => {
  const source = "x".repeat(3 * 1024 * 1024);
  const preview = eventPreview(source);
  assert.ok(preview.length < 17_000);
  assert.ok(preview.endsWith("[Admin preview truncated; use the run stream API for the complete event]"));
  assert.equal(source.length, 3 * 1024 * 1024);
});

test("preview truncation does not split a Unicode code point", () => {
  const preview = eventPreview("abc\u{1f600}def", 4);
  assert.ok(preview.startsWith("abc\n[Admin preview truncated"));
  assert.ok(!preview.includes("\ufffd"));
});
