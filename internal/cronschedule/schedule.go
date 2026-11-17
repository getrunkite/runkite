// Package cronschedule shares cron parsing between dispatch and advisory previews.
package cronschedule

import (
	"errors"
	"strings"
	"time"

	cronlib "github.com/robfig/cron/v3"
)

// Compiled preserves both the scheduler's input location and the calendar's
// effective location. An explicit expression zone can override the former.
type Compiled struct {
	Schedule                cronlib.Schedule
	Location                *time.Location
	EffectiveTimezone       string
	TimezoneFallback        bool
	ConfiguredTimezoneError error
}

// Compile uses the existing standard parser, including descriptors and TZ/
// CRON_TZ prefixes. Invalid configured zones fall back to UTC; invalid zones in
// the expression remain parse errors. Local retains the parser's special
// meaning of using the location supplied to Next.
func Compile(expression, timezone string) (Compiled, error) {
	location, locationErr := time.LoadLocation(timezone)
	if locationErr != nil {
		location = time.UTC
	}
	compiled := Compiled{Location: location, ConfiguredTimezoneError: locationErr}
	// The pinned parser slices a timezone prefix at its first literal space.
	// Missing that separator would panic before returning a normal parse error.
	if (strings.HasPrefix(expression, "TZ=") || strings.HasPrefix(expression, "CRON_TZ=")) && !strings.Contains(expression, " ") {
		return compiled, errors.New("cron timezone prefix requires a following expression")
	}
	parsed, err := cronlib.ParseStandard(expression)
	if err != nil {
		return compiled, err
	}
	compiled.Schedule = parsed
	effective := location
	compiled.TimezoneFallback = locationErr != nil
	if calendar, ok := parsed.(*cronlib.SpecSchedule); ok && calendar.Location != time.Local {
		effective = calendar.Location
		compiled.TimezoneFallback = false
	}
	compiled.EffectiveTimezone = effective.String()
	return compiled, nil
}

// Next evaluates the parsed schedule in the scheduler's configured location.
func (c Compiled) Next(after time.Time) time.Time {
	return c.Schedule.Next(after.In(c.Location))
}

type PreviewStatus string

const (
	StatusScheduled                   PreviewStatus = "scheduled"
	StatusDisabled                    PreviewStatus = "disabled"
	StatusInvalidExpression           PreviewStatus = "invalid_expression"
	StatusNoFutureFire                PreviewStatus = "no_future_fire"
	StatusRelativeIntervalUnavailable PreviewStatus = "relative_interval_unavailable"
)

// PreviewResult describes one future expression occurrence, not a claim,
// readiness check or promise to dispatch. Relative intervals need the live
// scheduler's anchor and therefore have no projected timestamp here.
type PreviewResult struct {
	ComputedAt        time.Time     `json:"computed_at"`
	Status            PreviewStatus `json:"status"`
	NextFireAt        *time.Time    `json:"next_fire_at,omitempty"`
	EffectiveTimezone string        `json:"effective_timezone,omitempty"`
	TimezoneFallback  bool          `json:"timezone_fallback,omitempty"`
}

// Preview returns a single calendar occurrence strictly after now. Disabled
// definitions are not parsed, matching the scheduler's early disabled check.
func Preview(expression, timezone string, enabled bool, now time.Time) PreviewResult {
	result := PreviewResult{ComputedAt: now.UTC(), Status: StatusDisabled}
	if !enabled {
		return result
	}
	compiled, err := Compile(expression, timezone)
	if err != nil {
		result.Status = StatusInvalidExpression
		return result
	}
	if _, relative := compiled.Schedule.(cronlib.ConstantDelaySchedule); relative {
		result.Status = StatusRelativeIntervalUnavailable
		return result
	}
	result.EffectiveTimezone = compiled.EffectiveTimezone
	result.TimezoneFallback = compiled.TimezoneFallback
	next := compiled.Next(now)
	if next.IsZero() || !next.After(now) {
		result.Status = StatusNoFutureFire
		return result
	}
	result.Status = StatusScheduled
	next = next.UTC()
	result.NextFireAt = &next
	return result
}
