package mysql

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

var _ state.AgentSchemaRecoveryStore = (*Store)(nil)

// RecoverAgentSchema replaces only the bootstrap JSON values in one update.
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
	result, err := s.db.ExecContext(ctx, `
		UPDATE agent_schemas SET input_schema=?, output_schema=?, state_schema=?, config_schema=?
		WHERE tenant_id=? AND agent_id=?
			AND input_schema=CAST('{"type":"object"}' AS JSON) AND output_schema=CAST('{"type":"object"}' AS JSON)
			AND state_schema=CAST('{"type":"object"}' AS JSON) AND config_schema=CAST('{"type":"object"}' AS JSON)
	`, encoded[0], encoded[1], encoded[2], encoded[3], tenant.FromContext(ctx), schema.AgentID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}
