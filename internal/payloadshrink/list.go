package payloadshrink

import "encoding/json"

var retrieveToolJSON = json.RawMessage(`{"name":"runkite_retrieve_payload","description":"Fetch the original connector result for a truncation ref from this run. Arguments: {\"ref\": string}.","inputSchema":{"type":"object","properties":{"ref":{"type":"string"}},"required":["ref"]}}`)

// InjectRetrieveTool appends (or replaces) the retrieve tool on a
// tools/list JSON-RPC result. Unparseable bodies pass through.
func InjectRetrieveTool(body []byte) []byte {
	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Result  *struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result,omitempty"`
		Error json.RawMessage `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Result == nil {
		return body
	}
	if hasJSONRPCError(resp.Error) {
		return body
	}
	out := make([]json.RawMessage, 0, len(resp.Result.Tools)+1)
	for _, raw := range resp.Result.Tools {
		var tool struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &tool) == nil && tool.Name == ToolName {
			continue
		}
		out = append(out, raw)
	}
	out = append(out, retrieveToolJSON)
	resp.Result.Tools = out
	b, err := json.Marshal(resp)
	if err != nil {
		return body
	}
	return b
}
