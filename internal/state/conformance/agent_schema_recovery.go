package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// RunAgentSchemaRecoveryTests checks the optional atomic recovery extension.
// Recovery fills an existing exact bootstrap stub, never an absent or useful
// report. Ordinary publication must remain authoritative under either ordering.
func RunAgentSchemaRecoveryTests(t *testing.T, factory StoreFactory) {
	t.Run("exact_stub_and_candidate_ownership", func(t *testing.T) {
		s, recovery := schemaRecoveryStore(t, factory)
		ctx := t.Context()
		schemaRecoverySeed(t, s, ctx, schemaRecoveryStub("recover"))
		if applied, err := recovery.RecoverAgentSchema(ctx, schemaRecoveryStub("recover")); err != nil || applied {
			t.Fatalf("all-stub candidate must be a no-op: applied=%v err=%v", applied, err)
		}
		if applied, err := recovery.RecoverAgentSchema(ctx, nil); err == nil || applied {
			t.Fatalf("nil candidate must fail without writing: applied=%v err=%v", applied, err)
		}
		schemaRecoveryWant(t, s, ctx, schemaRecoveryStub("recover"))
		candidate := schemaRecoveryReport("recover", "candidate")
		before := schemaRecoveryJSON(t, candidate)
		applied, err := recovery.RecoverAgentSchema(ctx, candidate)
		if err != nil || !applied {
			t.Fatalf("exact stub recovery: applied=%v err=%v", applied, err)
		}
		if !reflect.DeepEqual(before, schemaRecoveryJSON(t, candidate)) {
			t.Fatal("recovery mutated the caller's candidate")
		}
		schemaRecoveryWant(t, s, ctx, candidate)
		applied, err = recovery.RecoverAgentSchema(ctx, schemaRecoveryReport("recover", "replacement"))
		if err != nil || applied {
			t.Fatalf("recovery replaced a useful report: applied=%v err=%v", applied, err)
		}
		schemaRecoveryWant(t, s, ctx, candidate)
	})

	t.Run("every_field_requires_the_exact_stub", func(t *testing.T) {
		s, recovery := schemaRecoveryStore(t, factory)
		ctx := t.Context()
		variants := []struct {
			name  string
			value map[string]interface{}
		}{
			{"nil", nil},
			{"empty", map[string]interface{}{}},
			{"title_only", map[string]interface{}{"title": "existing"}},
			{"type_case", map[string]interface{}{"type": "Object"}},
			{"type_whitespace", map[string]interface{}{"type": " object "}},
			{"type_array", map[string]interface{}{"type": []interface{}{"object"}}},
			{"type_null", map[string]interface{}{"type": nil}},
			{"title", map[string]interface{}{"type": "object", "title": "existing"}},
			{"unknown_nested", map[string]interface{}{"type": "object", "x-unknown": map[string]interface{}{"nested": []interface{}{"retained", false}}}},
			{"properties", map[string]interface{}{"type": "object", "properties": map[string]interface{}{"value": map[string]interface{}{"type": "string"}}}},
		}
		for field := 0; field < 4; field++ {
			for _, variant := range variants {
				t.Run(fmt.Sprintf("field_%d/%s", field, variant.name), func(t *testing.T) {
					id := fmt.Sprintf("preserve-%d-%s", field, variant.name)
					original := schemaRecoveryStub(id)
					switch field {
					case 0:
						original.InputSchema = variant.value
					case 1:
						original.OutputSchema = variant.value
					case 2:
						original.StateSchema = variant.value
					case 3:
						original.ConfigSchema = variant.value
					}
					schemaRecoverySeed(t, s, ctx, original)
					applied, err := recovery.RecoverAgentSchema(ctx, schemaRecoveryReport(id, "candidate"))
					if err != nil || applied {
						t.Fatalf("non-stub field was overwritten: applied=%v err=%v", applied, err)
					}
					schemaRecoveryWant(t, s, ctx, original)
				})
			}
		}
	})

	t.Run("missing_schema_is_not_inserted", func(t *testing.T) {
		s, recovery := schemaRecoveryStore(t, factory)
		ctx := t.Context()
		if err := s.UpsertAgent(ctx, &models.Agent{AgentID: "missing", Name: "missing"}); err != nil {
			t.Fatal(err)
		}
		applied, err := recovery.RecoverAgentSchema(ctx, schemaRecoveryReport("missing", "candidate"))
		if err != nil || applied {
			t.Fatalf("missing row recovery: applied=%v err=%v", applied, err)
		}
		_, err = s.GetAgentSchema(ctx, "missing")
		var notFound *state.ErrNotFound
		if !errors.As(err, &notFound) {
			t.Fatalf("recovery inserted a missing row: %v", err)
		}
	})

	t.Run("tenant_scope_including_system_context", func(t *testing.T) {
		s, recovery := schemaRecoveryStore(t, factory)
		ctx := t.Context()
		alpha := tenant.ScopedContext(ctx, "schema-alpha")
		beta := tenant.ScopedContext(ctx, "schema-beta")
		for _, scope := range []context.Context{ctx, alpha, beta} {
			schemaRecoverySeed(t, s, scope, schemaRecoveryStub("shared"))
		}
		alphaReport := schemaRecoveryReport("shared", "alpha")
		if applied, err := recovery.RecoverAgentSchema(alpha, alphaReport); err != nil || !applied {
			t.Fatalf("alpha recovery: applied=%v err=%v", applied, err)
		}
		schemaRecoveryWant(t, s, alpha, alphaReport)
		schemaRecoveryWant(t, s, beta, schemaRecoveryStub("shared"))
		schemaRecoveryWant(t, s, ctx, schemaRecoveryStub("shared"))
		defaultReport := schemaRecoveryReport("shared", "default")
		if applied, err := recovery.RecoverAgentSchema(tenant.SystemContext(ctx), defaultReport); err != nil || !applied {
			t.Fatalf("system-context default recovery: applied=%v err=%v", applied, err)
		}
		schemaRecoveryWant(t, s, ctx, defaultReport)
		schemaRecoveryWant(t, s, alpha, alphaReport)
		schemaRecoveryWant(t, s, beta, schemaRecoveryStub("shared"))
	})

	t.Run("concurrent_recovery_has_one_winner", func(t *testing.T) {
		s, recovery := schemaRecoveryStore(t, factory)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		schemaRecoverySeed(t, s, ctx, schemaRecoveryStub("race"))
		type result struct {
			candidate *models.AgentSchema
			applied   bool
			err       error
		}
		const callers = 8
		start := make(chan struct{})
		results := make(chan result, callers)
		for i := 0; i < callers; i++ {
			candidate := schemaRecoveryReport("race", fmt.Sprintf("candidate-%d", i))
			go func() {
				<-start
				applied, err := recovery.RecoverAgentSchema(ctx, candidate)
				results <- result{candidate, applied, err}
			}()
		}
		close(start)
		var winner *models.AgentSchema
		wins := 0
		for i := 0; i < callers; i++ {
			got := <-results
			if got.err != nil {
				t.Errorf("concurrent recovery: %v", got.err)
			}
			if got.applied {
				wins++
				winner = got.candidate
			}
		}
		if wins != 1 {
			t.Fatalf("recovery winners=%d, want 1", wins)
		}
		schemaRecoveryWant(t, s, ctx, winner)
	})

	t.Run("normal_publication_survives_every_order", func(t *testing.T) {
		s, recovery := schemaRecoveryStore(t, factory)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		for _, order := range []string{"recovery_first", "publication_first", "concurrent"} {
			t.Run(order, func(t *testing.T) {
				schemaRecoverySeed(t, s, ctx, schemaRecoveryStub(order))
				candidate := schemaRecoveryReport(order, "recovery")
				publication := schemaRecoveryReport(order, "ordinary-publication")
				recover := func() error { _, err := recovery.RecoverAgentSchema(ctx, candidate); return err }
				publish := func() error { return s.UpsertAgentSchema(ctx, publication) }
				switch order {
				case "recovery_first":
					if err := recover(); err != nil {
						t.Fatal(err)
					}
					if err := publish(); err != nil {
						t.Fatal(err)
					}
				case "publication_first":
					if err := publish(); err != nil {
						t.Fatal(err)
					}
					if err := recover(); err != nil {
						t.Fatal(err)
					}
				case "concurrent":
					start := make(chan struct{})
					results := make(chan error, 2)
					for _, operation := range []func() error{recover, publish} {
						go func() { <-start; results <- operation() }()
					}
					close(start)
					for i := 0; i < 2; i++ {
						if err := <-results; err != nil {
							t.Error(err)
						}
					}
				}
				schemaRecoveryWant(t, s, ctx, publication)
			})
		}
	})

	t.Run("canceled_recovery_does_not_write", func(t *testing.T) {
		s, recovery := schemaRecoveryStore(t, factory)
		ctx := t.Context()
		schemaRecoverySeed(t, s, ctx, schemaRecoveryStub("canceled"))
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if applied, err := recovery.RecoverAgentSchema(canceled, schemaRecoveryReport("canceled", "candidate")); err == nil || applied {
			t.Fatalf("canceled recovery: applied=%v err=%v", applied, err)
		}
		schemaRecoveryWant(t, s, ctx, schemaRecoveryStub("canceled"))
	})
}

func schemaRecoveryStore(t *testing.T, factory StoreFactory) (state.Store, state.AgentSchemaRecoveryStore) {
	t.Helper()
	s := factory(t)
	recovery, ok := s.(state.AgentSchemaRecoveryStore)
	if !ok {
		t.Skip("backend does not implement optional agent schema recovery")
	}
	return s, recovery
}

func schemaRecoveryStub(id string) *models.AgentSchema {
	return &models.AgentSchema{
		AgentID:      id,
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
		StateSchema:  map[string]interface{}{"type": "object"},
		ConfigSchema: map[string]interface{}{"type": "object"},
	}
}

func schemaRecoveryReport(id, label string) *models.AgentSchema {
	schema := schemaRecoveryStub(id)
	schema.InputSchema["properties"] = map[string]interface{}{"value": map[string]interface{}{"type": "string"}}
	schema.InputSchema["x-unknown"] = map[string]interface{}{"label": label, "nested": []interface{}{false, "retained"}}
	schema.OutputSchema["title"] = label + " output"
	schema.StateSchema["title"] = label + " state"
	schema.ConfigSchema["title"] = label + " config"
	return schema
}

func schemaRecoverySeed(t *testing.T, s state.Store, ctx context.Context, schema *models.AgentSchema) {
	t.Helper()
	if err := s.UpsertAgent(ctx, &models.Agent{AgentID: schema.AgentID, Name: schema.AgentID}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAgentSchema(ctx, schema); err != nil {
		t.Fatal(err)
	}
}

func schemaRecoveryWant(t *testing.T, s state.Store, ctx context.Context, want *models.AgentSchema) {
	t.Helper()
	got, err := s.GetAgentSchema(ctx, want.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stored schema changed: got=%#v want=%#v", got, want)
	}
}

func schemaRecoveryJSON(t *testing.T, schema *models.AgentSchema) []byte {
	t.Helper()
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
