package jsonvalue

import (
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"strings"
	"testing"
)

func TestDecodeUnique(t *testing.T) {
	for _, raw := range []string{`{"x":1,"x":2}`, `{"a":[{"x":1,"\u0078":2}]}`, `{"x":"\ud800"}`, `{"x":"\udc00"}`, `{"x":"\ud800\u1234"}`, `{} {}`, string([]byte{'"', 0xff, '"'})} {
		if _, err := DecodeUnique([]byte(raw)); err == nil {
			t.Fatalf("accepted ambiguous JSON %q", raw)
		}
	}
	for _, raw := range []string{`{"x":9007199254740993,"X":2}`, `{"x":"\ud83d\ude00"}`, `{"x":"\\ud800"}`, `[]`, `null`, `"ok"`, `-0`} {
		if _, err := DecodeUnique([]byte(raw)); err != nil {
			t.Fatalf("rejected %s: %v", raw, err)
		}
	}
	value, _ := DecodeUnique([]byte(`{"x":9007199254740993}`))
	if value.(map[string]any)["x"] != json.Number("9007199254740993") {
		t.Fatal(value)
	}
}

func TestCompareNumbersOracle(t *testing.T) {
	cases := []string{"0", "-0", "1", "1.00", "-1", "1e3", "0.001", "-1e-20", "9007199254740992", "9007199254740993", "18446744073709551615", "0.12345678901234567890123456789", "-9223372036854775808"}
	rng := rand.New(rand.NewSource(41))
	for i := 0; i < 100; i++ {
		cases = append(cases, fmt.Sprintf("%de%d", rng.Int63n(20000000000)-10000000000, rng.Intn(81)-40))
	}
	for _, a := range cases {
		for _, b := range cases {
			ar, _ := new(big.Rat).SetString(a)
			br, _ := new(big.Rat).SetString(b)
			got, ok := CompareNumbers(json.Number(a), json.Number(b))
			if !ok || got != ar.Cmp(br) {
				t.Fatalf("compare %s vs %s: %d, want %d", a, b, got, ar.Cmp(br))
			}
		}
	}
	exponent := strings.Repeat("9", 10000)
	for _, pair := range [][2]string{{"1e" + exponent, "9e" + exponent}, {"-9e" + exponent, "-1e" + exponent}, {"0", "1e-" + exponent}} {
		if order, ok := CompareNumbers(json.Number(pair[0]), json.Number(pair[1])); !ok || order >= 0 {
			t.Fatal("huge exponent order failed")
		}
	}
}
