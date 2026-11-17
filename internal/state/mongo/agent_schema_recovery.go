package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

var _ state.AgentSchemaRecoveryStore = (*Store)(nil)

// RecoverAgentSchema conditionally replaces a single existing bootstrap row.
func (s *Store) RecoverAgentSchema(ctx context.Context, schema *models.AgentSchema) (bool, error) {
	if schema == nil {
		return false, fmt.Errorf("recover agent schema: schema is required")
	}
	if state.IsBootstrapAgentSchema(schema) {
		return false, nil
	}
	// Expression equality compares the complete field value, so an array that
	// contains a bootstrap object cannot satisfy the predicate as a query
	// document equality match could. Do not use tenantFilter: this write always
	// has an exact context tenant, even when ctx carries system read authority.
	conditions := bson.A{}
	for _, field := range []string{"input_schema", "output_schema", "state_schema", "config_schema"} {
		conditions = append(conditions, bson.M{"$eq": bson.A{"$" + field, bson.M{"$literal": bson.M{"type": "object"}}}})
	}
	result, err := s.col("agent_schemas").UpdateOne(ctx,
		bson.M{"tenant_id": tenant.FromContext(ctx), "agent_id": schema.AgentID, "$expr": bson.M{"$and": conditions}},
		bson.M{"$set": bson.M{
			"input_schema": schema.InputSchema, "output_schema": schema.OutputSchema,
			"state_schema": schema.StateSchema, "config_schema": schema.ConfigSchema,
		}},
	)
	if err != nil {
		return false, err
	}
	return result.ModifiedCount > 0, nil
}
