package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

func mkEntry(name, displayName string) *models.RegistryEntry {
	return &models.RegistryEntry{
		Name: name, DisplayName: displayName, SourceType: "url", SourceRef: "x",
		Tags: []string{}, Metadata: map[string]interface{}{},
	}
}

// TestAdminRegistry_TenantIDParamDisambiguatesGetUnderNameCollision is a
// regression test for a real gap found via audit: /admin-api/registry/{name}
// used system context with no tenant filter at all -- under a
// cross-tenant name collision (two tenants both publish "shared-name"),
// GetRegistryEntry would return an arbitrary match. The ?tenant_id=
// query param scopes the lookup to one specific tenant instead.
func TestAdminRegistry_TenantIDParamDisambiguatesGetUnderNameCollision(t *testing.T) {
	env := newTestEnv(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")

	env.store.PublishRegistryEntry(ctxA, mkEntry("shared-name", "tenant-a's entry"))
	env.store.PublishRegistryEntry(ctxB, mkEntry("shared-name", "tenant-b's entry"))

	resp, err := http.Get(env.srv.URL + "/admin-api/registry/shared-name?tenant_id=tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, 200)
	var got map[string]interface{}
	json.Unmarshal(readBody(t, resp), &got)
	if got["display_name"] != "tenant-a's entry" {
		t.Fatalf("expected tenant_id=tenant-a to disambiguate to tenant-a's entry, got %+v", got)
	}

	resp2, err := http.Get(env.srv.URL + "/admin-api/registry/shared-name?tenant_id=tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var got2 map[string]interface{}
	json.Unmarshal(readBody(t, resp2), &got2)
	if got2["display_name"] != "tenant-b's entry" {
		t.Fatalf("expected tenant_id=tenant-b to disambiguate to tenant-b's entry, got %+v", got2)
	}
}

// TestAdminRegistry_TenantIDParamScopesVersionHistory is a regression
// test for the worse consequence of the same gap: without ?tenant_id=,
// ListRegistryEntryVersions under system context genuinely MERGES two
// different tenants' version histories into one list (no tenant filter
// at all, unlike Get's "arbitrary single match"). ?tenant_id= scopes it
// to one tenant's own history, and the admin response shape now exposes
// tenant_id per version so a merged (no-param) response is at least
// distinguishable after the fact.
func TestAdminRegistry_TenantIDParamScopesVersionHistory(t *testing.T) {
	env := newTestEnv(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")

	env.store.PublishRegistryEntry(ctxA, mkEntry("shared-name", "a-v1"))
	env.store.PublishRegistryEntry(ctxB, mkEntry("shared-name", "b-v1"))

	// Without ?tenant_id=: both tenants' histories are present (the
	// documented, still-ambiguous default for a single-tenant deployment
	// where this can't happen) -- but tenant_id is now visible per entry.
	resp, err := http.Get(env.srv.URL + "/admin-api/registry/shared-name/versions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, 200)
	var merged []map[string]interface{}
	json.Unmarshal(readBody(t, resp), &merged)
	if len(merged) != 2 {
		t.Fatalf("expected merged history (2 entries) without ?tenant_id=, got %d: %+v", len(merged), merged)
	}
	if merged[0]["tenant_id"] == nil || merged[0]["tenant_id"] == "" {
		t.Fatal("expected tenant_id to be present on each admin version entry")
	}

	// With ?tenant_id=tenant-a: only tenant-a's own version, correctly scoped.
	scopedResp, err := http.Get(env.srv.URL + "/admin-api/registry/shared-name/versions?tenant_id=tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	defer scopedResp.Body.Close()
	var scoped []map[string]interface{}
	json.Unmarshal(readBody(t, scopedResp), &scoped)
	if len(scoped) != 1 || scoped[0]["display_name"] != "a-v1" {
		t.Fatalf("expected exactly tenant-a's own version with ?tenant_id=tenant-a, got %+v", scoped)
	}
}
