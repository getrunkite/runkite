// Package jsonvalue preserves numbers in application JSON without changing
// decoding of control-plane metadata, policy configuration or typed counters.
package jsonvalue

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Unmarshal has json.Unmarshal's single-document contract, but decodes numbers
// inside interfaces as json.Number instead of converting them to float64.
func Unmarshal(data []byte, value any) error {
	// Decoder.Decode alone accepts a valid prefix followed by another document.
	// Validate first, preserving the standard error and unchanged destination
	// for invalid input, including trailing non-whitespace data.
	if !json.Valid(data) {
		return json.Unmarshal(data, value)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(value)
}

// Equal compares decoded JSON values without rounding numbers. Equivalent
// numeric spellings such as 1, 1.0 and 1e0 compare equal.
func Equal(a, b any) bool {
	switch a := a.(type) {
	case json.Number:
		b, ok := b.(json.Number)
		return ok && EqualNumbers(a, b)
	case map[string]any:
		b, ok := b.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for key, value := range a {
			other, exists := b[key]
			if !exists || !Equal(value, other) {
				return false
			}
		}
		return true
	case []any:
		b, ok := b.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i, value := range a {
			if !Equal(value, b[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

// EqualNumbers compares JSON decimal values exactly, without expanding their
// exponents. Memory and work remain bounded by the number tokens' lengths.
func EqualNumbers(a, b json.Number) bool {
	an, ac, ae, aok := numberParts(a)
	bn, bc, be, bok := numberParts(b)
	return aok && bok && an == bn && ac == bc && ae == be
}

// CanonicalNumber compacts a JSON number without expanding its exponent. It
// removes insignificant zero digits before passing a value to native codecs.
func CanonicalNumber(number json.Number) (json.Number, error) {
	negative, coefficient, exponent, ok := numberParts(number)
	if !ok {
		return "", fmt.Errorf("invalid JSON number")
	}
	if negative {
		coefficient = "-" + coefficient
	}
	if exponent != "0" {
		coefficient += "e" + exponent
	}
	return json.Number(coefficient), nil
}

func numberParts(number json.Number) (negative bool, coefficient, exponent string, ok bool) {
	raw := number.String()
	if raw == "" || strings.TrimSpace(raw) != raw || !json.Valid([]byte(raw)) || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return false, "", "", false
	}
	negative = raw[0] == '-'
	if negative {
		raw = raw[1:]
	}
	exponent = "0"
	if pos := strings.IndexAny(raw, "eE"); pos >= 0 {
		exponent, raw = raw[pos+1:], raw[:pos]
	}
	fraction := 0
	if pos := strings.IndexByte(raw, '.'); pos >= 0 {
		fraction = len(raw) - pos - 1
		raw = raw[:pos] + raw[pos+1:]
	}
	coefficient = strings.TrimLeft(raw, "0")
	if coefficient == "" {
		return false, "0", "0", true
	}
	trimmed := strings.TrimRight(coefficient, "0")
	shift := len(coefficient) - len(trimmed) - fraction
	return negative, trimmed, shiftExponent(exponent, shift), true
}

func shiftExponent(exponent string, shift int) string {
	negative := exponent[0] == '-'
	if exponent[0] == '-' || exponent[0] == '+' {
		exponent = exponent[1:]
	}
	exponent = strings.TrimLeft(exponent, "0")
	if exponent == "" {
		return strconv.Itoa(shift)
	}
	if shift != 0 {
		shiftNegative := shift < 0
		if shiftNegative {
			shift = -shift // The shift is bounded by the input length.
		}
		magnitude := strconv.Itoa(shift)
		if negative == shiftNegative {
			exponent = addDigits(exponent, magnitude)
		} else if len(exponent) > len(magnitude) || len(exponent) == len(magnitude) && exponent >= magnitude {
			exponent = subtractDigits(exponent, magnitude)
		} else {
			exponent = subtractDigits(magnitude, exponent)
			negative = shiftNegative
		}
	}
	if exponent == "0" {
		return "0"
	}
	if negative {
		return "-" + exponent
	}
	return exponent
}

func addDigits(a, b string) string {
	size := len(a)
	if len(b) > size {
		size = len(b)
	}
	out := make([]byte, size+1)
	i, j, carry := len(a)-1, len(b)-1, 0
	for k := size; k >= 0; k-- {
		value := carry
		if i >= 0 {
			value += int(a[i] - '0')
			i--
		}
		if j >= 0 {
			value += int(b[j] - '0')
			j--
		}
		out[k], carry = byte(value%10)+'0', value/10
	}
	return strings.TrimLeft(string(out), "0")
}

// subtractDigits requires a >= b, with canonical unsigned decimal inputs.
func subtractDigits(a, b string) string {
	out := []byte(a)
	j, borrow := len(b)-1, 0
	for i := len(out) - 1; i >= 0; i-- {
		value := int(out[i]-'0') - borrow
		if j >= 0 {
			value -= int(b[j] - '0')
			j--
		}
		borrow = 0
		if value < 0 {
			value += 10
			borrow = 1
		}
		out[i] = byte(value) + '0'
	}
	value := strings.TrimLeft(string(out), "0")
	if value == "" {
		return "0"
	}
	return value
}
