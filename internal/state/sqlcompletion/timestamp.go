package sqlcompletion

import (
	"fmt"
	"time"
)

// storedTime accepts database-native times and the existing SQLite text forms
// without reducing their precision through SQL date conversion functions.
type storedTime struct{ time.Time }

func (t *storedTime) Scan(value interface{}) error {
	if direct, ok := value.(time.Time); ok {
		t.Time = direct.UTC()
		return nil
	}
	var text string
	switch value := value.(type) {
	case string:
		text = value
	case []byte:
		text = string(value)
	default:
		return fmt.Errorf("invalid stored timestamp")
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			t.Time = parsed.UTC()
			return nil
		}
	}
	return fmt.Errorf("invalid stored timestamp")
}

func nextCheckpointTime(now, parent time.Time) time.Time {
	next := now.UTC().Truncate(time.Microsecond)
	if !parent.IsZero() && !next.After(parent) {
		next = parent.UTC().Truncate(time.Microsecond).Add(time.Microsecond)
	}
	return next
}
