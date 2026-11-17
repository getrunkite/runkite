package cronschedule

import (
	"encoding/json"
	"testing"
	"time"
	_ "time/tzdata" // Keep fixed IANA/DST fixtures independent of host zone files.

	cronlib "github.com/robfig/cron/v3"
)

func instant(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestPreviewCalendarOccurrencesAndEffectiveTimezone(t *testing.T) {
	for _, tc := range []struct {
		name, expression, timezone, now, next, effective string
		fallback                                         bool
	}{
		{"empty_zone", "0 9 * * *", "", "2026-01-01T08:30:00Z", "2026-01-01T09:00:00Z", "UTC", false},
		{"strictly_after_boundary", "0 9 * * *", "UTC", "2026-01-01T09:00:00Z", "2026-01-02T09:00:00Z", "UTC", false},
		{"non_utc_clock", "0 9 * * *", "UTC", "2026-01-01T08:30:00+05:30", "2026-01-01T09:00:00Z", "UTC", false},
		{"winter", "0 9 * * *", "America/New_York", "2026-01-01T12:00:00Z", "2026-01-01T14:00:00Z", "America/New_York", false},
		{"summer", "0 9 * * *", "America/New_York", "2026-07-01T12:00:00Z", "2026-07-01T13:00:00Z", "America/New_York", false},
		{"expression_override", "CRON_TZ=America/Los_Angeles 0 9 * * *", "America/New_York", "2026-01-01T12:00:00Z", "2026-01-01T17:00:00Z", "America/Los_Angeles", false},
		{"invalid_configured_zone", "0 9 * * *", "Invalid/Runkite_Zone", "2026-01-01T08:00:00Z", "2026-01-01T09:00:00Z", "UTC", true},
		{"expression_zone_bypasses_fallback", "TZ=America/Los_Angeles 0 9 * * *", "Invalid/Runkite_Zone", "2026-01-01T12:00:00Z", "2026-01-01T17:00:00Z", "America/Los_Angeles", false},
		{"expression_utc_bypasses_fallback", "TZ=UTC 0 9 * * *", "Invalid/Runkite_Zone", "2026-01-01T08:00:00Z", "2026-01-01T09:00:00Z", "UTC", false},
		{"expression_local_uses_input_zone", "TZ=Local 0 9 * * *", "America/New_York", "2026-01-01T12:00:00Z", "2026-01-01T14:00:00Z", "America/New_York", false},
		{"expression_local_keeps_fallback", "CRON_TZ=Local 0 9 * * *", "Invalid/Runkite_Zone", "2026-01-01T08:00:00Z", "2026-01-01T09:00:00Z", "UTC", true},
		{"daily_descriptor", "@daily", "UTC", "2026-01-01T08:00:00Z", "2026-01-02T00:00:00Z", "UTC", false},
		{"hourly_descriptor", "@hourly", "UTC", "2026-01-01T08:59:59.5Z", "2026-01-01T09:00:00Z", "UTC", false},
		{"spring_gap_skips_missing_wall_time", "30 2 * * *", "America/New_York", "2026-03-08T05:00:00Z", "2026-03-09T06:30:00Z", "America/New_York", false},
		{"fall_fold_first_occurrence", "30 1 * * *", "America/New_York", "2026-11-01T05:00:00Z", "2026-11-01T05:30:00Z", "America/New_York", false},
		{"fall_fold_second_occurrence", "30 1 * * *", "America/New_York", "2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z", "America/New_York", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := instant(t, tc.now)
			got := Preview(tc.expression, tc.timezone, true, now)
			want := instant(t, tc.next)
			if got.Status != StatusScheduled || got.NextFireAt == nil || !got.NextFireAt.Equal(want) || !got.NextFireAt.After(now) {
				t.Fatalf("preview=%+v, want %v strictly after %v", got, want, now)
			}
			if !got.ComputedAt.Equal(now) || got.ComputedAt.Location() != time.UTC || got.NextFireAt.Location() != time.UTC {
				t.Fatalf("preview did not retain the instant and normalize timestamps to UTC: %+v", got)
			}
			if got.EffectiveTimezone != tc.effective || got.TimezoneFallback != tc.fallback {
				t.Fatalf("timezone metadata=%+v, want zone=%s fallback=%v", got, tc.effective, tc.fallback)
			}
		})
	}
}

func TestPreviewUnavailableStatesAndOptionalJSON(t *testing.T) {
	now := instant(t, "2026-01-01T08:00:00Z")
	for _, tc := range []struct {
		name, expression, timezone string
		enabled                    bool
		status                     PreviewStatus
	}{
		{"disabled_invalid_definition", "invalid", "Invalid/Runkite_Zone", false, StatusDisabled},
		{"empty_expression", "", "UTC", true, StatusInvalidExpression},
		{"bad_expression", "60 * * * *", "UTC", true, StatusInvalidExpression},
		{"missing_tz_separator", "TZ=UTC", "UTC", true, StatusInvalidExpression},
		{"missing_cron_tz_separator", "CRON_TZ=UTC", "UTC", true, StatusInvalidExpression},
		{"empty_tz_prefix", "TZ=", "UTC", true, StatusInvalidExpression},
		{"tabs_without_literal_separator", "TZ=UTC\t0\t9\t*\t*\t*", "UTC", true, StatusInvalidExpression},
		{"missing_prefixed_expression", "TZ=UTC ", "UTC", true, StatusInvalidExpression},
		{"bad_expression_zone", "TZ=Invalid/Runkite_Zone 0 9 * * *", "UTC", true, StatusInvalidExpression},
		{"bad_interval", "@every unknown", "UTC", true, StatusInvalidExpression},
		{"relative_interval", "@every 1m", "UTC", true, StatusRelativeIntervalUnavailable},
		{"relative_with_expression_zone", "TZ=America/New_York @every 1h", "UTC", true, StatusRelativeIntervalUnavailable},
		{"minimum_interval", "@every 0s", "UTC", true, StatusRelativeIntervalUnavailable},
		{"impossible_calendar", "0 0 31 2 *", "UTC", true, StatusNoFutureFire},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Preview(tc.expression, tc.timezone, tc.enabled, now)
			if got.Status != tc.status || got.NextFireAt != nil || !got.ComputedAt.Equal(now) {
				t.Fatalf("preview=%+v, want status=%s with no date", got, tc.status)
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if _, present := fields["next_fire_at"]; present {
				t.Fatalf("unavailable preview serialized a timestamp: %s", raw)
			}
			if tc.status != StatusNoFutureFire {
				if _, present := fields["effective_timezone"]; present || got.TimezoneFallback {
					t.Fatalf("unevaluated calendar claimed timezone metadata: %s", raw)
				}
			}
			if _, present := fields["timezone_fallback"]; present {
				t.Fatalf("false timezone fallback was not omitted: %s", raw)
			}
		})
	}
}

func TestCompileKeepsConfiguredLocationAndParserSemantics(t *testing.T) {
	now := instant(t, "2026-01-01T08:20:30Z")
	for _, expression := range []string{"*/5 * * * *", "@monthly", "@every 90s", "CRON_TZ=Asia/Kolkata 0 9 * * *", "TZ=Local 0 9 * * *"} {
		for _, zone := range []string{"", "UTC", "Local", "America/New_York", "Invalid/Runkite_Zone"} {
			t.Run(expression+"/"+zone, func(t *testing.T) {
				got, err := Compile(expression, zone)
				if err != nil {
					t.Fatal(err)
				}
				location, locationErr := time.LoadLocation(zone)
				if locationErr != nil {
					location = time.UTC
				}
				if got.Location.String() != location.String() || (got.ConfiguredTimezoneError != nil) != (locationErr != nil) {
					t.Fatalf("configured location changed: %+v want %s", got, location)
				}
				original, err := cronlib.ParseStandard(expression)
				if err != nil {
					t.Fatal(err)
				}
				if next := got.Next(now); !next.Equal(original.Next(now.In(location))) {
					t.Fatalf("shared parser changed occurrence: %v", next)
				}
			})
		}
	}
}
