package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

var _ state.AgentSchemaRecoveryStore = (*Store)(nil)

// RecoverAgentSchema replaces only the bootstrap JSONB values in one update.
func (s *Store) RecoverAgentSchema(ctx context.Context, schema *models.AgentSchema) (bool, error) {
	if schema == nil {
		return false, fmt.Errorf("recover agent schema: schema is required")
	}
	if state.IsBootstrapAgentSchema(schema) {
		return false, nil
	}
	var encoded [4][]byte
	for i, field := range []map[string]interface{}{schema.InputSchema, schema.OutputSchema, schema.StateSchema, schema.ConfigSchema} {
		data, err := json.Marshal(field)
		if err != nil {
			return false, fmt.Errorf("encode recovery schema: %w", err)
		}
		encoded[i] = data
	}
	result, err := s.pool.Exec(ctx, `
		UPDATE agent_schemas SET input_schema=$1, output_schema=$2, state_schema=$3, config_schema=$4
		WHERE tenant_id=$5 AND agent_id=$6
			AND input_schema='{"type":"object"}'::jsonb AND output_schema='{"type":"object"}'::jsonb
			AND state_schema='{"type":"object"}'::jsonb AND config_schema='{"type":"object"}'::jsonb
	`, encoded[0], encoded[1], encoded[2], encoded[3], tenant.FromContext(ctx), schema.AgentID)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() > 0, nil
}
