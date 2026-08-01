package mysql_test

import (
	"context"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
)

// TestAgent_GoMapInsertionOrderDoesNotBumpVersion is a narrower claim
// than it might look: Go's own encoding/json ALWAYS sorts map keys
// alphabetically before marshaling (verified separately, not assumed --
// json.Marshal on {"b":2,"a":1} and {"a":1,"b":2} produces byte-identical
// output regardless of Go map insertion order), so this test's two
// UpsertAgent calls actually send byte-identical JSON to MySQL either
// way -- it confirms the Go-level guarantee holds through this code
// path, not MySQL's own JSON `!=` normalization at the SQL level.
// MySQL's OWN normalization (that its JSON columns compare by parsed
// value, not raw byte-for-byte text, independent of what Go sends) was
// verified directly against the server with raw, differently-formatted
// JSON literals (`{"x":1}` inserted then compared against MySQL's own
// re-serialized `{"x": 1}` with a space) before writing UpsertAgent at
// all -- see mysql.go's package doc for that raw-SQL verification, not
// repeated here as a Go test since it would require bypassing
// json.Marshal entirely to be a meaningful re-check.
func TestAgent_GoMapInsertionOrderDoesNotBumpVersion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.UpsertAgent(ctx, &models.Agent{
		AgentID: "json-order", Name: "n",
		Metadata:     map[string]interface{}{"a": float64(1), "b": float64(2)},
		Capabilities: map[string]interface{}{"x": true, "y": false},
	})
	// Same logical JSON, different Go map iteration insertion order.
	s.UpsertAgent(ctx, &models.Agent{
		AgentID: "json-order", Name: "n",
		Metadata:     map[string]interface{}{"b": float64(2), "a": float64(1)},
		Capabilities: map[string]interface{}{"y": false, "x": true},
	})
	got, err := s.GetAgent(ctx, "json-order")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Fatalf("expected version to stay 1 for logically-identical JSON regardless of map insertion order, got %d", got.Version)
	}
}
