package jsonvalue

import (
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

func TestReviewDecimalOrderAgainstIndependentRationalOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(20261031))
	cases := []string{"0", "-0", "0.000e100", "1", "1.00", "10e-1", "-1", "-0.00000000000000000001"}
	for i := 0; i < 500; i++ {
		digits := make([]byte, 1+rng.Intn(60))
		digits[0] = byte('1' + rng.Intn(9))
		for j := 1; j < len(digits); j++ {
			digits[j] = byte('0' + rng.Intn(10))
		}
		fraction := rng.Intn(len(digits) + 1)
		number := string(digits)
		if fraction > 0 {
			integer := string(digits[:len(digits)-fraction])
			if integer == "" {
				integer = "0"
			}
			number = integer + "." + string(digits[len(digits)-fraction:])
		}
		if rng.Intn(2) == 0 {
			number = "-" + number
		}
		cases = append(cases, fmt.Sprintf("%se%d", number, rng.Intn(401)-200))
	}
	oracle := make([]*big.Rat, len(cases))
	for i, number := range cases {
		var ok bool
		oracle[i], ok = new(big.Rat).SetString(number)
		if !ok {
			t.Fatalf("invalid oracle fixture: %s", number)
		}
	}
	for i := 0; i < 20000; i++ {
		a, b := rng.Intn(len(cases)), rng.Intn(len(cases))
		order, ok := CompareNumbers(json.Number(cases[a]), json.Number(cases[b]))
		if !ok || order != oracle[a].Cmp(oracle[b]) {
			t.Fatalf("%s versus %s: order=%d valid=%t", cases[a], cases[b], order, ok)
		}
	}
	exponent := strings.Repeat("9", 10000)
	nextExponent := "1" + strings.Repeat("0", 10000)
	for _, pair := range [][2]string{
		{"10e" + exponent, "1e" + nextExponent},
		{"-10e" + exponent, "-1e" + nextExponent},
	} {
		if order, ok := CompareNumbers(json.Number(pair[0]), json.Number(pair[1])); !ok || order != 0 {
			t.Fatal("huge exponent carry changed equal numeric values")
		}
	}
	for _, invalid := range []string{"", "NaN", "Inf", "01", "+1", "1e", "1. ", `"1"`} {
		if _, ok := CompareNumbers(json.Number(invalid), json.Number("1")); ok {
			t.Fatalf("accepted invalid number %q", invalid)
		}
	}
}

func TestReviewUnicodeEscapeScannerAgainstUTF16Oracle(t *testing.T) {
	rng := rand.New(rand.NewSource(1701))
	for i := 0; i < 3000; i++ {
		units := make([]uint16, rng.Intn(24))
		var encoded strings.Builder
		encoded.WriteByte('"')
		for j := range units {
			unit := uint16(rng.Intn(0x10000))
			if rng.Intn(3) == 0 {
				unit = 0xd800 + uint16(rng.Intn(0x800))
			}
			if unit == utf8.RuneError {
				unit-- // Reserve RuneError as the oracle's unpaired-escape signal.
			}
			units[j] = unit
			fmt.Fprintf(&encoded, `\u%04x`, unit)
		}
		encoded.WriteByte('"')
		runes := utf16.Decode(units)
		valid := true
		for _, r := range runes {
			valid = valid && r != utf8.RuneError
		}
		value, err := DecodeUnique([]byte(encoded.String()))
		if valid {
			if err != nil || value != string(runes) {
				t.Fatalf("valid UTF-16 string changed: %s value=%v err=%v", encoded.String(), value, err)
			}
		} else if err == nil {
			t.Fatalf("accepted unpaired UTF-16 escapes: %s", encoded.String())
		}
	}
	for _, text := range []string{"\"", "\\", "\\ud800", "a\"b\\", "😀", "\ufffd", "\x00", "\u2028"} {
		raw, err := json.Marshal(text)
		if err != nil {
			t.Fatal(err)
		}
		value, err := DecodeUnique(raw)
		if err != nil || value != text {
			t.Fatalf("valid JSON escape changed: raw=%s value=%v err=%v", raw, value, err)
		}
	}
	if _, err := DecodeUnique([]byte(`{"\ud83d\ude00":1,"😀":2}`)); err == nil {
		t.Fatal("equivalent escaped and UTF-8 object keys were not rejected as duplicates")
	}
}

func TestReviewUniqueDecoderRejectsExcessiveJSONNestingWithoutPanic(t *testing.T) {
	for _, depth := range []int{10000, 10001} {
		raw := []byte(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))
		_, err := DecodeUnique(raw)
		if depth == 10000 && err != nil {
			t.Fatalf("valid maximum JSON nesting rejected: %v", err)
		}
		if depth > 10000 && err == nil {
			t.Fatal("excessive JSON nesting accepted")
		}
	}
}
