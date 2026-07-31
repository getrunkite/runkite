package matrix

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// NormalizedRun is the structural shape a scenario's real, live output
// gets reduced to before comparing against a golden fixture. Timestamps,
// run/thread UUIDs, and exact wording from a framework's (fake, but not
// necessarily byte-stable) LLM are deliberately excluded -- this proves
// "the same kind of thing happened, in the same order, ending the same
// way" across every backend a scenario runs against, which is the
// actual compatibility claim worth making. It is NOT a substitute for
// the runner-protocol/examples/*.json fixture format (item 19's
// original ask) or PROTOCOL.md's richer per-method/per-namespace shape
// diff -- this is deliberately the lighter matrix-scale version of that
// same idea, scoped to what's cheap to keep golden across 30+ cells
// without becoming so brittle that every fake-LLM wording tweak breaks
// the whole matrix.
type NormalizedRun struct {
	EventTypes  []string `json:"event_types"`
	FinalStatus string   `json:"final_status"`
}

// normalize reduces a raw SSE event capture down to the event type
// sequence plus whichever terminal status the stream's own "end" event
// (or, if absent, the last event carrying a "status" field) reported.
func normalize(events []sseEvent) NormalizedRun {
	n := NormalizedRun{EventTypes: eventTypesOf(events)}
	for _, e := range events {
		if s, ok := e.Data["status"].(string); ok {
			n.FinalStatus = s
		}
	}
	return n
}

const goldenRecordEnv = "RUNKITE_GOLDEN_RECORD"

// goldenDir resolves relative to this file, not the test binary's CWD
// (which `go test` sets to the package directory anyway, but resolving
// explicitly keeps this correct if that assumption ever changes).
var goldenDir = filepath.Join(repoRoot, "test", "e2e", "matrix", "golden")

// checkGolden compares got against the recorded fixture <name>.json.
//
// With RUNKITE_GOLDEN_RECORD=1 set, it (re)writes the fixture instead of
// comparing -- run the matrix once in record mode after intentionally
// changing expected behavior, review the resulting git diff like any
// other code change, then commit the new fixtures alongside it. Without
// that env var, a missing fixture is a hard failure (not a silent
// auto-create) so a genuinely new scenario can't accidentally ship
// without ever having its golden reviewed.
func checkGolden(t *testing.T, name string, got NormalizedRun) {
	t.Helper()
	path := filepath.Join(goldenDir, name+".json")

	if os.Getenv(goldenRecordEnv) != "" {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatalf("write golden fixture %s: %v", path, err)
		}
		t.Logf("recorded golden fixture: %s", path)
		return
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no golden fixture at %s -- run with %s=1 to record one after reviewing the failure above is expected: %v",
			path, goldenRecordEnv, err)
	}
	var want NormalizedRun
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("parse golden fixture %s: %v", path, err)
	}

	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("golden fixture mismatch for %s\n--- want (%s) ---\n%s\n--- got ---\n%s",
			name, path, wantJSON, gotJSON)
	}
}
