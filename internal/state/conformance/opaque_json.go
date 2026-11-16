package conformance

import (
	"context"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
)

const exactOpaqueJSONNumbers = `{"id":9007199254740993,"decimal":1.0000000000000001,"nested":[9007199254740993,{"decimal":2.0000000000000001,"small":0.12345678901234567890123456789},null,false,"9007199254740993"]}`

// RunSQLControlJSONNumberTests covers opaque JSON projections on all SQL
// backends with values within native MySQL JSON's storage precision. SQLite
// and Postgres additionally run exactOpaqueJSONNumbers via their exact suite.
func RunSQLControlJSONNumberTests(t *testing.T, factory StoreFactory) {
	t.Helper()
	t.Run("opaque_control_json", func(t *testing.T) { runOpaqueControlJSONTests(t, factory, jsonNumbers) })
}

func runOpaqueControlJSONTests(t *testing.T, factory StoreFactory, raw string) {
	t.Run("capabilities_current_search_and_immutable_versions", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		want := numberValues(t, raw)
		if err := s.UpsertAgent(ctx, &models.Agent{AgentID: "opaque-agent", Name: "opaque", Capabilities: want}); err != nil {
			t.Fatal(err)
		}
		loaded, err := s.GetAgent(ctx, "opaque-agent")
		if err != nil {
			t.Fatal(err)
		}
		assertJSONValue(t, loaded.Capabilities, want)
		// The config bootstrap reads, merges, and re-upserts this map. Even an
		// unchanged registration must not rewrite its opaque numeric values.
		if err := s.UpsertAgent(ctx, loaded); err != nil {
			t.Fatal(err)
		}
		loaded, err = s.GetAgent(ctx, loaded.AgentID)
		if err != nil || loaded.Version != 1 {
			t.Fatalf("unchanged registration: agent=%+v err=%v", loaded, err)
		}
		loaded.Description = "visible field changed"
		if err := s.UpsertAgent(ctx, loaded); err != nil {
			t.Fatal(err)
		}
		agents, err := s.SearchAgents(ctx, &models.AgentSearchRequest{Limit: 10})
		if err != nil || len(agents) != 1 || agents[0].Version != 2 {
			t.Fatalf("search agents: agents=%+v err=%v", agents, err)
		}
		assertJSONValue(t, agents[0].Capabilities, want)
		versions, err := s.ListAgentVersions(ctx, loaded.AgentID)
		if err != nil || len(versions) != 2 {
			t.Fatalf("list versions: versions=%+v err=%v", versions, err)
		}
		for _, version := range versions {
			assertJSONValue(t, version.Capabilities, want)
			got, err := s.GetAgentVersion(ctx, loaded.AgentID, version.Version)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONValue(t, got.Capabilities, want)
		}
	})
	t.Run("registry_current_search_and_immutable_versions", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		want := numberValues(t, raw)
		if err := s.PublishRegistryEntry(ctx, &models.RegistryEntry{Name: "opaque-entry", SourceType: "inline", SourceRef: "{}", Metadata: want}); err != nil {
			t.Fatal(err)
		}
		loaded, err := s.GetRegistryEntry(ctx, "opaque-entry")
		if err != nil {
			t.Fatal(err)
		}
		assertJSONValue(t, loaded.Metadata, want)
		loaded.Description = "visible field changed"
		if err := s.PublishRegistryEntry(ctx, loaded); err != nil {
			t.Fatal(err)
		}
		entries, err := s.SearchRegistryEntries(ctx, &models.RegistrySearchRequest{Limit: 10})
		if err != nil || len(entries) != 1 || entries[0].Version != 2 {
			t.Fatalf("search entries: entries=%+v err=%v", entries, err)
		}
		assertJSONValue(t, entries[0].Metadata, want)
		versions, err := s.ListRegistryEntryVersions(ctx, loaded.Name)
		if err != nil || len(versions) != 2 {
			t.Fatalf("list versions: versions=%+v err=%v", versions, err)
		}
		for _, version := range versions {
			assertJSONValue(t, version.Metadata, want)
			got, err := s.GetRegistryEntryVersion(ctx, loaded.Name, version.Version)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONValue(t, got.Metadata, want)
		}
	})
}
