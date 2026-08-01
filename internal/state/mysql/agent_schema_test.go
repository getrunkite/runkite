package mysql_test

import (
	"context"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
)

// TestAgentSchema_UpsertAndGet covers UpsertAgentSchema/GetAgentSchema --
// part of state.Store's interface, but not exercised anywhere in the
// shared conformance suite (internal/state/conformance) on ANY backend
// today, not just MySQL. Keeping this here rather than treating it as
// "conformance's job" until that gap is closed upstream.
func TestAgentSchema_UpsertAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.UpsertAgent(ctx, &models.Agent{AgentID: "a1", Name: "a", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})

	if err := s.UpsertAgentSchema(ctx, &models.AgentSchema{
		AgentID: "a1", InputSchema: map[string]interface{}{"a": float64(1)},
		OutputSchema: map[string]interface{}{}, StateSchema: map[string]interface{}{}, ConfigSchema: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("UpsertAgentSchema: %v", err)
	}
	sch, err := s.GetAgentSchema(ctx, "a1")
	if err != nil {
		t.Fatalf("GetAgentSchema: %v", err)
	}
	if sch.InputSchema["a"] != float64(1) {
		t.Fatalf("expected input_schema.a=1, got %+v", sch.InputSchema)
	}

	// Republish updates in place, not a second row.
	if err := s.UpsertAgentSchema(ctx, &models.AgentSchema{
		AgentID: "a1", InputSchema: map[string]interface{}{"a": float64(2)},
		OutputSchema: map[string]interface{}{}, StateSchema: map[string]interface{}{}, ConfigSchema: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("UpsertAgentSchema (update): %v", err)
	}
	sch, err = s.GetAgentSchema(ctx, "a1")
	if err != nil {
		t.Fatalf("GetAgentSchema (after update): %v", err)
	}
	if sch.InputSchema["a"] != float64(2) {
		t.Fatalf("expected updated input_schema.a=2, got %+v", sch.InputSchema)
	}
}
