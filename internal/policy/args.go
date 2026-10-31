package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/getrunkite/runkite/internal/jsonvalue"
)

const (
	argsMetaMaxBytes   = 4096
	argsMetaMaxDepth   = 3
	argsStringMaxRunes = 256
	argsMetaOmitted    = "<omitted>"
)

// BindArgs turns MCP tools/call params.arguments into the three PolicyInput
// fields: full value for predicate eval, SHA-256 digest for the decision
// cache, and a size-capped redacted map for audit/webhook display.
//
// Missing or empty arguments digest as the JSON object {}. The digest is
// a versioned sha256 of sorted-key JSON with exact normalized numbers. This is
// not RFC 8785. Secret-looking keys are stripped
// from the display map only; they still feed the digest.
func BindArgs(raw json.RawMessage) (parsed any, digest string, meta map[string]any) {
	parsed, digest, meta, _ = BindArgsStrict(raw)
	return parsed, digest, meta
}

// BindArgsStrict rejects ambiguous arguments instead of authorizing a different
// interpretation from the downstream service. HTTP callers must handle its error.
func BindArgsStrict(raw json.RawMessage) (parsed any, digest string, meta map[string]any, err error) {
	parsed, err = parseCallArguments(raw)
	if err != nil {
		return nil, "", nil, err
	}
	return parsed, digestArgs(parsed), redactArgs(parsed), nil
}

func parseCallArguments(raw json.RawMessage) (any, error) {
	trim := strings.TrimSpace(string(raw))
	if trim == "" || trim == "null" {
		return map[string]any{}, nil
	}
	v, err := jsonvalue.DecodeUnique(raw)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func digestArgs(v any) string {
	if v == nil {
		v = map[string]any{}
	}
	b, err := json.Marshal(canonicalDigestValue(v))
	if err != nil {
		b = []byte("{}")
	}
	// Old digests rounded arguments through float64. Do not let an old
	// approval authorize a different operation after upgrading the parser.
	sum := sha256.Sum256(append([]byte("runkite-policy-args-v2\x00"), b...))
	return hex.EncodeToString(sum[:])
}

func canonicalDigestValue(v any) any {
	switch value := v.(type) {
	case json.Number:
		n, err := jsonvalue.CanonicalNumber(value)
		if err == nil {
			return n
		}
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[key] = canonicalDigestValue(item)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = canonicalDigestValue(item)
		}
		return out
	}
	return v
}

func redactArgs(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := copyRedactedMap(m, 1)
	for {
		b, err := json.Marshal(out)
		if err != nil || len(b) <= argsMetaMaxBytes {
			return out
		}
		if len(out) == 0 {
			return map[string]any{"_": argsMetaOmitted}
		}
		// Drop an arbitrary key until the display map fits.
		for k := range out {
			delete(out, k)
			break
		}
	}
}

func copyRedactedMap(m map[string]any, depth int) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if secretArgKey(k) {
			continue
		}
		out[k] = copyRedactedValue(v, depth)
	}
	return out
}

func copyRedactedValue(v any, depth int) any {
	if depth > argsMetaMaxDepth {
		return argsMetaOmitted
	}
	switch t := v.(type) {
	case map[string]any:
		return copyRedactedMap(t, depth+1)
	case []any:
		if depth+1 > argsMetaMaxDepth {
			return argsMetaOmitted
		}
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = copyRedactedValue(item, depth+1)
		}
		return out
	case string:
		return truncateRunes(t, argsStringMaxRunes)
	default:
		return v
	}
}

func secretArgKey(k string) bool {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "password", "token", "secret", "authorization", "api_key":
		return true
	default:
		return false
	}
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max])
}
