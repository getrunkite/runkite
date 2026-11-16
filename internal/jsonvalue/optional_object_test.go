package jsonvalue

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUnmarshalOptionalObjectPreservesLegacyEmptyAndExactNumbers(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, []byte("null")} {
		value := map[string]interface{}{"previous": true}
		if err := UnmarshalOptionalObject(raw, &value); err != nil || value != nil {
			t.Fatalf("empty/null object: %#v %v", value, err)
		}
	}
	var value map[string]interface{}
	if err := UnmarshalOptionalObject([]byte(`{"nested":[9007199254740993,1.0000000000000001]}`), &value); err != nil {
		t.Fatal(err)
	}
	numbers := value["nested"].([]interface{})
	if numbers[0] != json.Number("9007199254740993") || numbers[1] != json.Number("1.0000000000000001") {
		t.Fatalf("opaque numbers rounded: %#v", value)
	}
}

func TestUnmarshalOptionalObjectSanitizesMalformedNonemptyValues(t *testing.T) {
	for _, raw := range []string{`{"private_corruption_sentinel":`, `{"private_corruption_sentinel":"text"} {}`, `["private_corruption_sentinel"]`, `"private_corruption_sentinel"`, "1", "false", " "} {
		var value map[string]interface{}
		err := UnmarshalOptionalObject([]byte(raw), &value)
		if err == nil || strings.Contains(err.Error(), "private_corruption_sentinel") {
			t.Fatalf("expected sanitized error, got %v", err)
		}
	}
}
