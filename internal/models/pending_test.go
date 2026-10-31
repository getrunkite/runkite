package models

import (
	"encoding/json"
	"testing"
)

func TestPendingArgsKeepExactNumericDisplay(t *testing.T) {
	raw := []byte(`{"id":9007199254740993,"nested":[18446744073709551615,0.12345678901234567890123456789]}`)
	args := UnmarshalPendingArgs(raw)
	if args["id"] != json.Number("9007199254740993") {
		t.Fatalf("pending argument changed: %#v", args["id"])
	}
	if encoded := string(MarshalPendingArgs(args)); encoded != string(raw) {
		t.Fatalf("pending preview changed numeric values: %s", encoded)
	}
	for _, invalid := range []string{`{"id":1} trailing`, `[]`, `null`, `{}`} {
		if got := UnmarshalPendingArgs([]byte(invalid)); got != nil {
			t.Fatalf("invalid/empty args %q decoded: %#v", invalid, got)
		}
	}
}
