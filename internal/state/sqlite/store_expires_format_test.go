package sqlite

import (
	"testing"
	"time"
)

// TestFormatStoreExpiresLexOrderMatchesChrono is the regression for a
// real bug in the first TTL pass: time.RFC3339Nano trims trailing
// fractional zeros, so lexicographic string compares disagree with
// chronological order (e.g. "...00.1Z" > "...00.11Z" as strings while
// 100ms < 110ms as times). Fixed-width fractional seconds keep both
// orders identical, which SQLite TEXT comparison of expires_at needs.
func TestFormatStoreExpiresLexOrderMatchesChrono(t *testing.T) {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	earlier := base.Add(100 * time.Millisecond) // .1
	later := base.Add(110 * time.Millisecond)   // .11

	a := formatStoreExpires(earlier)
	b := formatStoreExpires(later)
	if a >= b {
		t.Fatalf("lex order broken: formatStoreExpires(%v)=%q should be < formatStoreExpires(%v)=%q", earlier, a, later, b)
	}
	if len(a) != len(b) {
		t.Fatalf("expected fixed-width timestamps, got len %d vs %d (%q / %q)", len(a), len(b), a, b)
	}

	// Broader spot-check across uneven fractional lengths that bite
	// RFC3339Nano string compares.
	times := []time.Duration{
		1 * time.Millisecond,
		10 * time.Millisecond,
		100 * time.Millisecond,
		101 * time.Millisecond,
		110 * time.Millisecond,
		999 * time.Millisecond,
		1000 * time.Millisecond,
	}
	var prev string
	var prevT time.Time
	for i, d := range times {
		ts := base.Add(d)
		s := formatStoreExpires(ts)
		if i > 0 && prev >= s {
			t.Fatalf("lex order broken at step %d: %q (%v) should be < %q (%v)", i, prev, prevT, s, ts)
		}
		prev, prevT = s, ts
	}
}
