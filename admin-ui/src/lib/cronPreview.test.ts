import { test } from "node:test";
import assert from "node:assert/strict";
import type { AdminCronPreview } from "../api/types.ts";
import { adminCronListPath, cronPreviewDisplay, cronPreviewUtc } from "./cronPreview.ts";

const computed_at = "2026-11-17T09:30:00Z";
const preview: AdminCronPreview = {
  computed_at, status: "scheduled", next_fire_at: "2026-11-17T10:00:00Z", effective_timezone: "Europe/London",
};

test("Cron opts into history and projection independently at the API boundary", () => {
  const query = new URL(adminCronListPath, "https://plane.example").searchParams;
  assert.equal(query.get("include_history"), "true");
  assert.equal(query.get("include_preview"), "true");
  assert.equal([...query].length, 2);
});

test("calendar preview renders explicit UTC boundary, computation time, and effective zone", () => {
  assert.deepEqual(cronPreviewDisplay(preview), {
    label: "2026-11-17 10:00:00 UTC", tone: "secondary", computedAt: "2026-11-17 09:30:00 UTC",
    timezone: "Europe/London", fallback: false,
  });
  const fallback = cronPreviewDisplay({ ...preview, effective_timezone: "UTC", timezone_fallback: true });
  assert.equal(fallback.fallback, true);
  assert.equal(fallback.timezone, "UTC");
});

test("disabled, invalid, impossible, and relative schedules have distinct non-dispatch labels", () => {
  for (const [status, label] of [
    ["disabled", "Disabled"], ["invalid_expression", "Invalid expression"],
    ["no_future_fire", "No next boundary found"], ["relative_interval_unavailable", "Relative interval preview unavailable"],
  ] as const) {
    const result = cronPreviewDisplay({ computed_at, status });
    assert.equal(result.label, label);
    assert.equal(result.timezone, undefined);
    assert.equal(result.computedAt, "2026-11-17 09:30:00 UTC");
  }
});

test("missing or malformed previews never manufacture a next fire", () => {
  assert.equal(cronPreviewDisplay(undefined).label, "Preview unavailable");
  assert.equal(cronPreviewDisplay({ ...preview, next_fire_at: undefined }).label, "Preview unavailable");
  assert.equal(cronPreviewDisplay({ ...preview, next_fire_at: "invalid" }).label, "Preview unavailable");
  assert.equal(cronPreviewUtc("invalid"), undefined);
  assert.equal(cronPreviewDisplay({ ...preview, computed_at: "invalid" }).computedAt, undefined);
  assert.equal(cronPreviewUtc("2026-11-17T15:30:00+05:30"), "2026-11-17 10:00:00 UTC");
});

test("Local means the server's timezone and never the browser's timezone", () => {
  const local = { ...preview, effective_timezone: "Local" };
  assert.equal(cronPreviewDisplay(local).timezone, "Local (server)");
  assert.equal(local.effective_timezone, "Local");
  assert.equal(cronPreviewDisplay({ ...preview, effective_timezone: "UTC" }).timezone, "UTC");
});
