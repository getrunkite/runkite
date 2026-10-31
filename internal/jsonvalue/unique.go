package jsonvalue

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// DecodeUnique preserves numbers and rejects ambiguous object members at every
// depth. Use it when one interpretation authorizes bytes another service reads.
func DecodeUnique(data []byte) (any, error) {
	if !utf8.Valid(data) || !json.Valid(data) || !validSurrogates(data) {
		return nil, fmt.Errorf("expected one valid UTF-8 JSON value")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	return uniqueValue(d)
}

// encoding/json replaces unpaired UTF-16 escapes with U+FFFD. Other parsers
// retain them, so an authorization digest must reject that ambiguity.
func validSurrogates(data []byte) bool {
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
		}
		if !inString || data[i] != '\\' {
			continue
		}
		if data[i+1] != 'u' {
			i++
			continue
		}
		unit := hexUnit(data[i+2 : i+6])
		if unit >= 0xdc00 && unit <= 0xdfff {
			return false
		}
		if unit >= 0xd800 && unit <= 0xdbff {
			if i+12 > len(data) || data[i+6] != '\\' || data[i+7] != 'u' {
				return false
			}
			low := hexUnit(data[i+8 : i+12])
			if low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 11
		} else {
			i += 5
		}
	}
	return true
}

func hexUnit(digits []byte) int {
	n := 0
	for _, digit := range digits {
		n *= 16
		switch {
		case digit <= '9':
			n += int(digit - '0')
		case digit <= 'F':
			n += int(digit-'A') + 10
		default:
			n += int(digit-'a') + 10
		}
	}
	return n
}

func uniqueValue(d *json.Decoder) (any, error) {
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		out := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			name := key.(string) // json.Valid proved the object grammar.
			if _, exists := out[name]; exists {
				return nil, fmt.Errorf("duplicate JSON object member")
			}
			value, err := uniqueValue(d)
			if err != nil {
				return nil, err
			}
			out[name] = value
		}
		_, err := d.Token()
		return out, err
	case json.Delim('['):
		out := []any{}
		for d.More() {
			value, err := uniqueValue(d)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		_, err := d.Token()
		return out, err
	default:
		return token, nil
	}
}
