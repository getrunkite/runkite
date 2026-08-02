// Package tests validates committed Runner Protocol example fixtures against
// the JSON schemas and a small set of lifecycle invariants.
//
// This is the schema/lifecycle PR-gate slice of protocol conformance: it
// ensures the golden examples under examples/ stay well-formed. The
// execute half (live execute_run → diff expected_events) lives in
// python/tests/test_protocol_execute_goldens.py (make test-protocol-execute).
package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// runner-protocol/tests -> runner-protocol -> repo root
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func protocolDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "runner-protocol")
}

func loadJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}

func asMap(t *testing.T, v any, label string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: want object, got %T", label, v)
	}
	return m
}

func asSlice(t *testing.T, v any, label string) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: want array, got %T", label, v)
	}
	return s
}

func requireString(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key]
	if !ok || v == nil {
		t.Fatalf("missing required %q", key)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		t.Fatalf("%q must be a non-empty string, got %T %#v", key, v, v)
	}
	return s
}

func validateAssignment(t *testing.T, assignment map[string]any) {
	t.Helper()
	for _, key := range []string{"run_id", "thread_id", "runner_kind", "graph_id"} {
		requireString(t, assignment, key)
	}
	if modes, ok := assignment["stream_modes"]; ok && modes != nil {
		for i, m := range asSlice(t, modes, "stream_modes") {
			if _, ok := m.(string); !ok {
				t.Fatalf("stream_modes[%d]: want string, got %T", i, m)
			}
		}
	}
}

func validateEvent(t *testing.T, ev map[string]any, idx int) {
	t.Helper()
	label := "expected_events[" + strconv.Itoa(idx) + "]"

	seqNum, ok := ev["seq"].(float64)
	if !ok || seqNum < 1 || seqNum != float64(int(seqNum)) {
		t.Fatalf("%s.seq: want integer >= 1, got %#v", label, ev["seq"])
	}
	seq := int(seqNum)
	if seq != idx+1 {
		t.Fatalf("%s.seq: want %d (1-based position), got %d", label, idx+1, seq)
	}

	method := requireString(t, ev, "method")
	ns := asSlice(t, ev["namespace"], label+".namespace")
	for i, n := range ns {
		if _, ok := n.(string); !ok {
			t.Fatalf("%s.namespace[%d]: want string, got %T", label, i, n)
		}
	}
	if _, ok := ev["data"]; !ok {
		t.Fatalf("%s.data: required", label)
	}
	_ = seq // seq already checked against position

	data := ev["data"]
	switch method {
	case "end":
		dm := asMap(t, data, label+".data")
		status := requireString(t, dm, "status")
		switch status {
		case "success", "error", "interrupted":
		default:
			t.Fatalf("%s.end.data.status: unexpected %q", label, status)
		}
	case "error":
		dm := asMap(t, data, label+".data")
		requireString(t, dm, "message")
	case "lifecycle":
		dm := asMap(t, data, label+".data")
		requireString(t, dm, "event")
	}
}

func validateEventSequence(t *testing.T, events []any, cancelAfter any) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("expected_events: empty")
	}
	for i, raw := range events {
		validateEvent(t, asMap(t, raw, "event"), i)
	}

	first := asMap(t, events[0], "first")
	if first["method"] != "lifecycle" {
		t.Fatalf("first event method: want lifecycle, got %v", first["method"])
	}
	firstData := asMap(t, first["data"], "first.data")
	if firstData["event"] != "running" {
		t.Fatalf("first lifecycle event: want running, got %v", firstData["event"])
	}

	last := asMap(t, events[len(events)-1], "last")
	lastMethod, _ := last["method"].(string)
	switch lastMethod {
	case "end", "error":
	default:
		t.Fatalf("last event method: want end or error, got %q", lastMethod)
	}

	if cancelAfter != nil {
		n, ok := cancelAfter.(float64)
		if !ok || n < 1 {
			t.Fatalf("cancel_after_seq: want integer >= 1, got %#v", cancelAfter)
		}
		if int(n) >= len(events) {
			t.Fatalf("cancel_after_seq=%d must be before terminal event (len=%d)", int(n), len(events))
		}
	}
}

func validateFixture(t *testing.T, doc map[string]any) {
	t.Helper()
	if id, ok := doc["_test_id"].(string); ok && id == "" {
		t.Fatal("_test_id empty")
	}

	// Standard single-run fixtures.
	if a, ok := doc["assignment"]; ok {
		validateAssignment(t, asMap(t, a, "assignment"))
		events := asSlice(t, doc["expected_events"], "expected_events")
		validateEventSequence(t, events, doc["cancel_after_seq"])
		return
	}

	// HITL two-phase fixture (003).
	if a, ok := doc["run_1_assignment"]; ok {
		validateAssignment(t, asMap(t, a, "run_1_assignment"))
		validateEventSequence(t, asSlice(t, doc["run_1_expected_events"], "run_1_expected_events"), nil)
		validateAssignment(t, asMap(t, doc["run_2_assignment"], "run_2_assignment"))
		validateEventSequence(t, asSlice(t, doc["run_2_expected_events"], "run_2_expected_events"), nil)
		return
	}

	t.Fatal("fixture missing assignment or run_1_assignment")
}

func TestProtocolExampleFixtures(t *testing.T) {
	examplesDir := filepath.Join(protocolDir(t), "examples")
	entries, err := os.ReadDir(examplesDir)
	if err != nil {
		t.Fatalf("read examples: %v", err)
	}
	var count int
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		count++
		path := filepath.Join(examplesDir, e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			validateFixture(t, loadJSON(t, path))
		})
	}
	if count != 10 {
		t.Fatalf("expected 10 example fixtures, found %d", count)
	}
}

func TestProtocolSchemasPresent(t *testing.T) {
	schemas := filepath.Join(protocolDir(t), "schemas")
	for _, name := range []string{"run_assignment.json", "run_event.json"} {
		path := filepath.Join(schemas, name)
		doc := loadJSON(t, path)
		if doc["$schema"] == nil {
			t.Fatalf("%s: missing $schema", name)
		}
		req, ok := doc["required"].([]any)
		if !ok || len(req) == 0 {
			t.Fatalf("%s: missing required[]", name)
		}
	}
}
