package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpInputSchemaLimit   = 64 << 10
	mcpSchemaCatalogLimit = 1 << 20
	mcpSchemaMetaKey      = "getrunkite.com/input-schema"
)

// MCP schemas are advisory session snapshots. Reporting a shape does not prove
// which code a worker has loaded, and the low-level MCP handler still forwards
// input without applying defaults, coercion or schema-based authorization.
func (s *Server) mcpInputSchema(ctx context.Context, agentID string, remaining *int) (map[string]any, mcp.Meta) {
	fallback := func(reason string) (map[string]any, mcp.Meta) {
		return freshMCPToolEnvelope(), mcp.Meta{mcpSchemaMetaKey: map[string]any{
			"status": "generic", "reason": reason, "freshness": "unknown", "session_snapshot": true,
		}}
	}
	reported, err := s.store.GetAgentSchema(ctx, agentID)
	if err != nil || reported == nil || len(reported.InputSchema) == 0 {
		return fallback("missing_or_unavailable")
	}
	budget := mcpSchemaBudget{}
	if !budget.check(reported.InputSchema, 0) {
		return fallback("bounds_or_value_type")
	}
	raw, err := json.Marshal(reported.InputSchema)
	if err != nil || len(raw) > mcpInputSchemaLimit {
		return fallback("bounds_or_value_type")
	}
	var input map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&input) != nil || input["type"] != "object" {
		return fallback("unsupported_root")
	}
	if !hasMCPInputShape(input) {
		return fallback("stub")
	}
	if reason := relocateMCPInputSchema(input); reason != "" {
		return fallback(reason)
	}
	envelope := freshMCPToolEnvelope()
	envelope["properties"].(map[string]any)["input"] = input
	encoded, err := json.Marshal(envelope)
	if err != nil || len(encoded) > mcpInputSchemaLimit {
		return fallback("bounds_or_value_type")
	}
	var checked jsonschema.Schema
	if json.Unmarshal(encoded, &checked) != nil {
		return fallback("invalid_schema")
	}
	// A nil loader refuses remote references. Resolve checks structure and local
	// references only; it neither validates a tool invocation nor applies defaults.
	if _, err := checked.Resolve(nil); err != nil {
		return fallback("invalid_schema")
	}
	if remaining != nil {
		if len(encoded) > *remaining {
			return fallback("catalog_budget")
		}
		*remaining -= len(encoded)
	}
	digest := sha256.Sum256(raw)
	return envelope, mcp.Meta{mcpSchemaMetaKey: map[string]any{
		"status": "reported", "source": "agent_schema", "freshness": "unknown",
		"session_snapshot": true, "reported_schema_sha256": hex.EncodeToString(digest[:]),
	}}
}

func freshMCPToolEnvelope() map[string]any {
	envelope := maps.Clone(mcpToolInputSchema)
	properties := maps.Clone(envelope["properties"].(map[string]any))
	for name, property := range properties {
		properties[name] = maps.Clone(property.(map[string]any))
	}
	envelope["properties"] = properties
	return envelope
}

func hasMCPInputShape(schema map[string]any) bool {
	for _, key := range []string{"properties", "patternProperties", "required", "allOf", "anyOf", "oneOf", "additionalProperties", "minProperties", "maxProperties", "$ref"} {
		value, exists := schema[key]
		if !exists {
			continue
		}
		switch v := value.(type) {
		case map[string]any:
			if len(v) > 0 {
				return true
			}
		case []any:
			if len(v) > 0 {
				return true
			}
		case bool:
			if key == "additionalProperties" && !v {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// Rebase only schema keywords. A $ref inside an example, default or const is
// ordinary instance data and must remain byte-for-byte equivalent JSON data.
func relocateMCPInputSchema(schema map[string]any) string {
	// Legacy schema-valued dependencies can contain references of their own.
	// Their draft-specific behavior is outside this v2020-12 subset; leaving
	// them untouched could bind a local reference to the surrounding envelope.
	if _, exists := schema["dependencies"]; exists {
		return "unsupported_legacy_keyword"
	}
	for _, key := range []string{"$id", "id", "$anchor", "$dynamicAnchor", "$dynamicRef", "$recursiveRef", "$recursiveAnchor"} {
		if _, exists := schema[key]; exists {
			return "unsupported_resource_or_reference"
		}
	}
	if dialect, exists := schema["$schema"]; exists {
		if dialect != "https://json-schema.org/draft/2020-12/schema" && dialect != "https://json-schema.org/draft/2020-12/schema#" {
			return "unsupported_dialect"
		}
		delete(schema, "$schema")
	}
	if reference, exists := schema["$ref"]; exists {
		ref, ok := reference.(string)
		if !ok || (ref != "#" && !strings.HasPrefix(ref, "#/")) {
			return "unsupported_resource_or_reference"
		}
		schema["$ref"] = "#/properties/input" + strings.TrimPrefix(ref, "#")
	}
	visit := func(value any) string {
		if child, ok := value.(map[string]any); ok {
			return relocateMCPInputSchema(child)
		}
		if _, ok := value.(bool); ok {
			return ""
		}
		return "invalid_schema"
	}
	for _, key := range []string{"$defs", "definitions", "properties", "patternProperties", "dependentSchemas"} {
		if value, exists := schema[key]; exists {
			children, ok := value.(map[string]any)
			if !ok {
				return "invalid_schema"
			}
			for _, child := range children {
				if reason := visit(child); reason != "" {
					return reason
				}
			}
		}
	}
	for _, key := range []string{"additionalProperties", "unevaluatedProperties", "propertyNames", "contains", "not", "if", "then", "else", "items", "additionalItems", "unevaluatedItems", "contentSchema"} {
		if child, exists := schema[key]; exists {
			if reason := visit(child); reason != "" {
				return reason
			}
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		if value, exists := schema[key]; exists {
			children, ok := value.([]any)
			if !ok {
				return "invalid_schema"
			}
			for _, child := range children {
				if reason := visit(child); reason != "" {
					return reason
				}
			}
		}
	}
	return ""
}

type mcpSchemaBudget struct{ nodes, bytes int }

func (b *mcpSchemaBudget) check(value any, depth int) bool {
	b.nodes++
	if depth > 32 || b.nodes > 4096 || b.bytes > mcpInputSchemaLimit {
		return false
	}
	switch v := value.(type) {
	case nil, bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		b.bytes += 32
	case json.Number:
		b.bytes += len(v)
	case string:
		b.bytes += len(v)
	case map[string]any:
		for key, child := range v {
			b.bytes += len(key)
			if !b.check(child, depth+1) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !b.check(child, depth+1) {
				return false
			}
		}
	case []string:
		for _, child := range v {
			if !b.check(child, depth+1) {
				return false
			}
		}
	default:
		return false
	}
	return b.bytes <= mcpInputSchemaLimit
}
