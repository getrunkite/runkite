import { test } from "node:test";
import assert from "node:assert/strict";
import { nsToString, stringToNs, nsPrefixPattern } from "./store.js";

test("nsToString/stringToNs round-trip", () => {
  const cases = [[], ["a"], ["a", "b", "c"], ["team-a"], ["seg/with/slash"]];
  for (const ns of cases) {
    assert.deepEqual(stringToNs(nsToString(ns)), ns);
  }
});

test("nsToString wraps with leading and trailing delimiters", () => {
  assert.equal(nsToString(["a", "b"]), "\x1fa\x1fb\x1f");
});

test("stringToNs of empty-ish input returns empty array", () => {
  assert.deepEqual(stringToNs("\x1f\x1f"), []);
  assert.deepEqual(stringToNs(""), []);
});

// The whole reason for \x1F-delimited encoding instead of plain "/" joins:
// a prefix match on "team-a" must never match a sibling like "team-abc".
// Mirrors the exact regression this fixed in the Go SQLite/Postgres
// stores and the Python runner's store.py.
test("nsPrefixPattern never matches a same-string-prefix sibling", () => {
  const pattern = nsPrefixPattern(["team-a"]);
  const teamA = nsToString(["team-a", "docs"]);
  const teamAbc = nsToString(["team-abc", "docs"]);

  const likeToRegex = (p: string) => new RegExp("^" + p.replace(/%/g, ".*") + "$");
  const re = likeToRegex(pattern);

  assert.ok(re.test(teamA), "expected team-a/docs to match the team-a prefix");
  assert.ok(!re.test(teamAbc), "expected team-abc/docs to NOT match the team-a prefix");
});

test("nsPrefixPattern with empty prefix matches everything", () => {
  assert.equal(nsPrefixPattern([]), "%");
});

test("segments containing '/' round-trip correctly (not confused with the delimiter)", () => {
  const ns = ["a/b", "c"];
  assert.deepEqual(stringToNs(nsToString(ns)), ns);
});
