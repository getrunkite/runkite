package jsonvalue

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUnmarshalPreservesNestedNumberTokens(t *testing.T) {
	raw := `{"id":9007199254740993,"nested":[18446744073709551615,-9223372036854775808,0.123456789012345678901,1.234567890123456789e-80],"ordinary":7}`
	var value map[string]any
	if err := Unmarshal([]byte(raw), &value); err != nil {
		t.Fatal(err)
	}
	if value["id"] != json.Number("9007199254740993") {
		t.Fatalf("id changed: %#v", value["id"])
	}
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"9007199254740993", "18446744073709551615", "-9223372036854775808", "0.123456789012345678901", "1.234567890123456789e-80"} {
		if !strings.Contains(string(out), token) {
			t.Fatalf("number %s changed in %s", token, out)
		}
	}
	if value["ordinary"] != json.Number("7") {
		t.Fatalf("ordinary application number is not preserved: %#v", value["ordinary"])
	}
}

func TestEqualNumbersKeepsNumericEquivalenceWithoutRounding(t *testing.T) {
	for _, pair := range [][2]string{
		{"1", "1.0"}, {"1", "1e0"}, {"0", "-0.000e9999999999999999999999"},
		{"9007199254740993", "9007199254740993.000"},
		{"123.4500", "12345e-2"}, {"100e-2", "1"}, {"1e+0010", "10e9"},
		{"1e100000000000000000000", "10e99999999999999999999"},
		{"1e-100000000000000000000", "0.1e-99999999999999999999"},
	} {
		if !EqualNumbers(json.Number(pair[0]), json.Number(pair[1])) || !EqualNumbers(json.Number(pair[1]), json.Number(pair[0])) {
			t.Fatalf("equivalent numbers differ: %v", pair)
		}
	}
	for _, pair := range [][2]string{
		{"9007199254740992", "9007199254740993"}, {"0.1234567890123456789", "0.1234567890123456788"},
		{"1e100000000000000000000", "1e100000000000000000001"}, {"-1", "1"},
		{"1 ", "1"}, {"true", "1"}, {"NaN", "0"}, {"01", "1"},
	} {
		if EqualNumbers(json.Number(pair[0]), json.Number(pair[1])) {
			t.Fatalf("different numbers compare equal: %v", pair)
		}
	}
}

func TestEqualDecodedJSONDistinguishesLargeNumbers(t *testing.T) {
	var a, b, c any
	for _, item := range []struct {
		raw  string
		dest *any
	}{
		{`{"id":9007199254740993,"nested":[1,null,true]}`, &a},
		{`{"nested":[1.0,null,true],"id":9007199254740993.0}`, &b},
		{`{"id":9007199254740992,"nested":[1,null,true]}`, &c},
	} {
		if err := Unmarshal([]byte(item.raw), item.dest); err != nil {
			t.Fatal(err)
		}
	}
	if !Equal(a, b) || Equal(a, c) {
		t.Fatal("JSON numeric equivalence changed or neighboring IDs collapsed")
	}
}

func TestCanonicalNumberCompactsEquivalentValuesWithoutExpansion(t *testing.T) {
	for _, item := range []struct{ input, output string }{
		{"123.45000", "12345e-2"}, {"1.000e+0003", "1e3"},
		{"-0.000e999999999999999999999999999", "0"},
		{"1" + strings.Repeat("0", 100_000) + "e-100000", "1"},
		{"1e100000000000000000000", "1e100000000000000000000"},
	} {
		got, err := CanonicalNumber(json.Number(item.input))
		if err != nil || got.String() != item.output || !EqualNumbers(json.Number(item.input), got) {
			t.Fatalf("canonical number changed value: got=%q err=%v expected=%q", got, err, item.output)
		}
	}
	if _, err := CanonicalNumber(json.Number("NaN")); err == nil {
		t.Fatal("invalid numeric token accepted")
	}
}

func TestUnmarshalRejectsInvalidWholeDocumentsWithoutMutatingDestination(t *testing.T) {
	for _, raw := range []string{`{"id":9007199254740993} {}`, `{"id":9007199254740993} trailing`, `{"id":`, ``, `{"id":01}`} {
		t.Run(raw, func(t *testing.T) {
			value := map[string]any{"retained": "original"}
			if err := Unmarshal([]byte(raw), &value); err == nil {
				t.Fatal("invalid JSON accepted")
			}
			if len(value) != 1 || value["retained"] != "original" {
				t.Fatalf("invalid JSON mutated destination: %#v", value)
			}
		})
	}
}
