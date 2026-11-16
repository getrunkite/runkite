package sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
)

func TestOpaqueControlJSONRejectsCorruptStoredRows(t *testing.T) {
	for _, test := range []struct {
		name, table, column string
		reads               func(*SQLiteStore) []error
	}{
		{"agent_current", "agents", "capabilities", func(s *SQLiteStore) []error {
			_, get := s.GetAgent(context.Background(), "opaque")
			_, search := s.SearchAgents(context.Background(), &models.AgentSearchRequest{Limit: 10})
			return []error{get, search}
		}},
		{"agent_history", "agent_versions", "capabilities", func(s *SQLiteStore) []error {
			_, get := s.GetAgentVersion(context.Background(), "opaque", 1)
			_, list := s.ListAgentVersions(context.Background(), "opaque")
			return []error{get, list}
		}},
		{"registry_current", "registry_entries", "metadata", func(s *SQLiteStore) []error {
			_, get := s.GetRegistryEntry(context.Background(), "opaque")
			_, search := s.SearchRegistryEntries(context.Background(), &models.RegistrySearchRequest{Limit: 10})
			return []error{get, search}
		}},
		{"registry_history", "registry_entry_versions", "metadata", func(s *SQLiteStore) []error {
			_, get := s.GetRegistryEntryVersion(context.Background(), "opaque", 1)
			_, list := s.ListRegistryEntryVersions(context.Background(), "opaque")
			return []error{get, list}
		}},
	} {
		for _, malformed := range []string{`{"private_corruption_sentinel":`, `["private_corruption_sentinel"]`} {
			t.Run(test.name+"/"+malformed[:1], func(t *testing.T) {
				s, err := New("")
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				ctx := context.Background()
				if err := s.Init(ctx); err != nil {
					t.Fatal(err)
				}
				if err := s.UpsertAgent(ctx, &models.Agent{AgentID: "opaque", Name: "opaque", Capabilities: map[string]interface{}{"keep": true}}); err != nil {
					t.Fatal(err)
				}
				if err := s.PublishRegistryEntry(ctx, &models.RegistryEntry{Name: "opaque", SourceType: "inline", SourceRef: "{}", Metadata: map[string]interface{}{"keep": true}}); err != nil {
					t.Fatal(err)
				}
				// Names are fixed test constants. Only the corrupted value is data.
				if _, err := s.db.ExecContext(ctx, "UPDATE "+test.table+" SET "+test.column+" = ?", malformed); err != nil {
					t.Fatal(err)
				}
				for _, err := range test.reads(s) {
					if err == nil {
						t.Fatal("corrupt opaque JSON became a successful empty-map read")
					}
					if strings.Contains(err.Error(), "private_corruption_sentinel") {
						t.Fatal("parse error exposed stored metadata")
					}
				}
			})
		}
	}
}
