package payloadshrink

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

const retrieveHintFmt = "\n\n[runkite: result truncated; call runkite_retrieve_payload with {\"ref\":%q} for the original]"

type jsonRPCEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type mcpTextResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func hasJSONRPCError(errField json.RawMessage) bool {
	if len(errField) == 0 {
		return false
	}
	t := bytes.TrimSpace(errField)
	return len(t) > 0 && string(t) != "null"
}

func innerResult(body []byte) (env jsonRPCEnvelope, inner []byte, ok bool) {
	if json.Unmarshal(body, &env) != nil {
		return env, nil, false
	}
	if hasJSONRPCError(env.Error) {
		return env, nil, false
	}
	if len(bytes.TrimSpace(env.Result)) == 0 || string(bytes.TrimSpace(env.Result)) == "null" {
		return env, nil, false
	}
	return env, env.Result, true
}

func contentAddressRef(inner []byte) string {
	sum := sha256.Sum256(inner)
	return hex.EncodeToString(sum[:])[:16]
}

func previewText(inner []byte, previewBytes int) string {
	var parsed mcpTextResult
	src := inner
	if json.Unmarshal(inner, &parsed) == nil && len(parsed.Content) > 0 && parsed.Content[0].Text != "" {
		src = []byte(parsed.Content[0].Text)
	}
	return string(truncateUTF8(src, previewBytes))
}

func truncateUTF8(b []byte, n int) []byte {
	if n <= 0 {
		return nil
	}
	if len(b) <= n {
		return b
	}
	for n > 0 && !utf8.RuneStart(b[n]) {
		n--
	}
	return b[:n]
}

func buildStub(env jsonRPCEnvelope, preview, ref string) ([]byte, error) {
	text := preview + fmt.Sprintf(retrieveHintFmt, ref)
	result := map[string]any{
		"content": []map[string]string{
			{"type": "text", "text": text},
		},
		"isError": false,
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	out := jsonRPCEnvelope{
		JSONRPC: env.JSONRPC,
		ID:      env.ID,
		Result:  raw,
	}
	if out.JSONRPC == "" {
		out.JSONRPC = "2.0"
	}
	return json.Marshal(out)
}
