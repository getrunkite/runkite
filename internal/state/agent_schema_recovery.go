package state

import (
	"context"

	"github.com/getrunkite/runkite/internal/models"
)

// AgentSchemaRecoveryStore is an optional extension for recovering an existing
// bootstrap schema without replacing any report already published by a runner.
// Stores that do not implement it retain the ordinary schema publication API.
type AgentSchemaRecoveryStore interface {
	// RecoverAgentSchema atomically replaces an existing row only when all four
	// stored fields are exactly {"type":"object"}. The write is scoped to
	// tenant.FromContext(ctx), including for system contexts. Missing rows,
	// existing reports and an incoming all-stub schema return false, nil.
	// Storage and encoding failures return an error. No row is inserted and
	// neither schema nor its maps may be mutated.
	RecoverAgentSchema(ctx context.Context, schema *models.AgentSchema) (bool, error)
}

// IsBootstrapAgentSchema recognizes only the exact four-field bootstrap stub.
// A partial report, omitted field or annotation is not a bootstrap schema.
func IsBootstrapAgentSchema(schema *models.AgentSchema) bool {
	if schema == nil {
		return false
	}
	for _, field := range []map[string]interface{}{schema.InputSchema, schema.OutputSchema, schema.StateSchema, schema.ConfigSchema} {
		if len(field) != 1 || field["type"] != "object" {
			return false
		}
	}
	return true
}
