package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

func adminIdentityRequest(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	// Exact selection must remove inherited system authority, and empty query
	// selection must replace the inherited tenant with the default tenant.
	r = r.WithContext(tenant.SystemContext(tenant.WithContext(r.Context(), "inherited-foreign")))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestAdminExactAgentAndRegistrySelectionMasksSystemScope(t *testing.T) {
	s, store := newLifecycleServer(t, nil)
	handler := s.Handler()
	for _, tid := range []string{"default", "tenant", " tenant "} {
		ctx := tenant.ScopedContext(context.Background(), tid)
		if err := store.UpsertAgent(ctx, &models.Agent{AgentID: " shared ", Name: tid}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertAgentSchema(ctx, &models.AgentSchema{AgentID: " shared ", InputSchema: map[string]interface{}{"scope": tid}}); err != nil {
			t.Fatal(err)
		}
		if err := store.PublishRegistryEntry(ctx, &models.RegistryEntry{Name: " entry ", Description: tid, SourceType: "inline", SourceRef: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, selection := range []struct{ query, tenant string }{
		{"?tenant_id=", "default"}, {"?tenant_id=default", "default"},
		{"?tenant_id=tenant", "tenant"}, {"?tenant_id=%20tenant%20", " tenant "},
	} {
		for _, path := range []string{"/admin-api/agents/%20shared%20", "/admin-api/registry/%20entry%20"} {
			t.Run(path+selection.query, func(t *testing.T) {
				response := adminIdentityRequest(handler, http.MethodGet, path+selection.query, "")
				var record struct {
					TenantID string `json:"tenant_id"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &record); err != nil || response.Code != 200 || record.TenantID != selection.tenant {
					t.Fatalf("wrong exact record: %d %s %v", response.Code, response.Body.String(), err)
				}
				versions := adminIdentityRequest(handler, http.MethodGet, path+"/versions"+selection.query, "")
				var history []struct {
					TenantID string `json:"tenant_id"`
				}
				if err := json.Unmarshal(versions.Body.Bytes(), &history); err != nil || versions.Code != 200 || len(history) != 1 || history[0].TenantID != selection.tenant {
					t.Fatalf("cross-tenant version history: %d %s %v", versions.Code, versions.Body.String(), err)
				}
			})
		}
		schema := adminIdentityRequest(handler, http.MethodGet, "/admin-api/agents/%20shared%20/schemas"+selection.query, "")
		var schemaRecord models.AgentSchema
		if err := json.Unmarshal(schema.Body.Bytes(), &schemaRecord); err != nil || schema.Code != 200 || schemaRecord.InputSchema["scope"] != selection.tenant {
			t.Fatalf("cross-tenant schema: %d %s %v", schema.Code, schema.Body.String(), err)
		}
	}
	for _, path := range []string{"/admin-api/agents/%20shared%20", "/admin-api/registry/%20entry%20"} {
		if response := adminIdentityRequest(handler, http.MethodGet, path+"?tenant_id=missing", ""); response.Code != 404 {
			t.Fatalf("missing tenant fell back globally: %d %s", response.Code, response.Body.String())
		}
		// Omitted tenant selection retains the global Admin history contract.
		response := adminIdentityRequest(handler, http.MethodGet, path+"/versions", "")
		var history []json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &history); err != nil || response.Code != 200 || len(history) != 3 {
			t.Fatalf("no-query global history changed: %d %s %v", response.Code, response.Body.String(), err)
		}
	}
}

func TestAdminExplicitEmptyTenantDoesNotFindForeignOnlyRecords(t *testing.T) {
	s, store := newLifecycleServer(t, nil)
	ctx := tenant.ScopedContext(context.Background(), "foreign")
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "only-foreign", Name: "foreign"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishRegistryEntry(ctx, &models.RegistryEntry{Name: "only-foreign", SourceType: "inline", SourceRef: "{}"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/admin-api/agents/only-foreign", "/admin-api/registry/only-foreign"} {
		if got := adminIdentityRequest(s.Handler(), http.MethodGet, path+"?tenant_id=", ""); got.Code != 404 {
			t.Fatalf("explicit default selection exposed foreign record: %d %s", got.Code, got.Body.String())
		}
		if got := adminIdentityRequest(s.Handler(), http.MethodGet, path, ""); got.Code != 200 {
			t.Fatalf("omitted selection lost global lookup: %d %s", got.Code, got.Body.String())
		}
	}
}

func TestAdminRegistryExactSaveAndDeleteLeaveCanonicalNeighborsUntouched(t *testing.T) {
	for _, selected := range []struct{ queryTenant, storedTenant, neighborTenant, name string }{
		{" tenant ", " tenant ", "tenant", " entry "},
		{"", "default", "foreign", "entry"},
		{"tenant", "tenant", "foreign", "new"},
	} {
		t.Run(selected.storedTenant, func(t *testing.T) {
			s, store := newLifecycleServer(t, nil)
			type identity struct{ tenant, name string }
			records := []identity{{selected.storedTenant, selected.name}, {selected.neighborTenant, selected.name}}
			if selected.name != strings.TrimSpace(selected.name) {
				records = append(records, identity{selected.storedTenant, strings.TrimSpace(selected.name)}, identity{selected.neighborTenant, strings.TrimSpace(selected.name)})
			}
			for _, record := range records {
				ctx := tenant.ScopedContext(context.Background(), record.tenant)
				if err := store.PublishRegistryEntry(ctx, &models.RegistryEntry{Name: record.name, Description: "original", SourceType: "inline", SourceRef: "{}"}); err != nil {
					t.Fatal(err)
				}
			}
			path := "/admin-api/registry/" + url.PathEscape(selected.name) + "?tenant_id=" + url.QueryEscape(selected.queryTenant)
			response := adminIdentityRequest(s.Handler(), http.MethodPut, path, `{"name":"entry","description":"visible edit","source_type":"inline","source_ref":"{}"}`)
			if response.Code != 200 {
				t.Fatalf("exact save: %d %s", response.Code, response.Body.String())
			}
			for index, record := range records {
				got, err := store.GetRegistryEntry(tenant.ScopedContext(context.Background(), record.tenant), record.name)
				want := "original"
				if index == 0 {
					want = "visible edit"
				}
				if err != nil || got.Description != want {
					t.Fatalf("save modified the wrong identity %+v: %+v %v", record, got, err)
				}
			}
			if response := adminIdentityRequest(s.Handler(), http.MethodDelete, path, ""); response.Code != 204 {
				t.Fatalf("exact delete: %d %s", response.Code, response.Body.String())
			}
			for _, record := range records[1:] {
				if _, err := store.GetRegistryEntry(tenant.ScopedContext(context.Background(), record.tenant), record.name); err != nil {
					t.Fatalf("delete crossed tenant/name selection %+v: %v", record, err)
				}
			}
		})
	}
}
