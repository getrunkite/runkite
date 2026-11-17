package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
)

func schemaRecoveryAPIStore(t *testing.T) *sqlitestore.SQLiteStore {
	t.Helper()
	s, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func schemaRecoveryAPIStub(id string) *models.AgentSchema {
	return &models.AgentSchema{AgentID: id,
		InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
		StateSchema: map[string]interface{}{"type": "object"}, ConfigSchema: map[string]interface{}{"type": "object"},
	}
}

func schemaRecoveryAPIReport(id, label string) *models.AgentSchema {
	schema := schemaRecoveryAPIStub(id)
	schema.InputSchema["properties"] = map[string]interface{}{"value": map[string]interface{}{"type": "string"}}
	schema.OutputSchema["title"] = label
	return schema
}

func schemaRecoveryAPISeed(t *testing.T, s state.Store, id, kind string, schema *models.AgentSchema) {
	t.Helper()
	if err := s.UpsertAgent(t.Context(), &models.Agent{AgentID: id, Name: id, Metadata: map[string]interface{}{"runner_kind": kind}}); err != nil {
		t.Fatal(err)
	}
	if schema != nil {
		if err := s.UpsertAgentSchema(t.Context(), schema); err != nil {
			t.Fatal(err)
		}
	}
}

func schemaRecoveryAPICall(t *testing.T, handler http.Handler, method, path, kind, token string, schema *models.AgentSchema) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(auth.HeaderRunnerKind, kind)
	r.Header.Set(auth.HeaderRunnerToken, token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func schemaRecoveryAPIWant(t *testing.T, s state.Store, want *models.AgentSchema) {
	t.Helper()
	got, err := s.GetAgentSchema(t.Context(), want.AgentID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("stored schema=%#v err=%v, want %#v", got, err, want)
	}
}

func TestAgentSchemaReportingBindsAuthenticatedRunnerKind(t *testing.T) {
	t.Setenv("RUNNER_TOKEN_SCHEMA_OWNER", "synthetic-owner-token")
	t.Setenv("RUNNER_TOKEN_SCHEMA_OTHER", "synthetic-other-token")
	tokens := auth.LoadRunnerTokensFromEnv()
	for _, operation := range []struct{ method, suffix string }{{"PUT", "/schema"}, {"POST", "/schema/recover"}} {
		for _, tc := range []struct {
			name, configuredKind, claimedKind, token string
			status                                   int
		}{
			{"matching", "schema-owner", "schema-owner", "synthetic-owner-token", 204},
			{"mismatched", "schema-owner", "schema-other", "synthetic-other-token", 403},
			{"configured_kind_is_exact", " schema-owner ", "schema-owner", "synthetic-owner-token", 403},
			{"cross_kind_token", "schema-owner", "schema-owner", "synthetic-other-token", 401},
			{"missing_credentials", "schema-owner", "", "", 401},
		} {
			t.Run(operation.method+"/"+tc.name, func(t *testing.T) {
				s := schemaRecoveryAPIStore(t)
				stub := schemaRecoveryAPIStub("agent")
				schemaRecoveryAPISeed(t, s, "agent", tc.configuredKind, stub)
				handler := auth.Middleware(nil, nil, tokens, api.NewServer(s, nil, nil, nil).Handler())
				report := schemaRecoveryAPIReport("agent", "reported")
				w := schemaRecoveryAPICall(t, handler, operation.method, "/internal/agents/agent"+operation.suffix, tc.claimedKind, tc.token, report)
				if w.Code != tc.status {
					t.Fatalf("status=%d body=%s, want %d", w.Code, w.Body.String(), tc.status)
				}
				if tc.status == 204 {
					schemaRecoveryAPIWant(t, s, report)
				} else {
					schemaRecoveryAPIWant(t, s, stub)
				}
			})
		}
	}
}

func TestAgentSchemaLocalHeadersDoNotClaimAuthenticatedKind(t *testing.T) {
	for _, operation := range []struct{ method, suffix string }{{"PUT", "/schema"}, {"POST", "/schema/recover"}} {
		t.Run(operation.method, func(t *testing.T) {
			s := schemaRecoveryAPIStore(t)
			schemaRecoveryAPISeed(t, s, "agent", "schema-owner", schemaRecoveryAPIStub("agent"))
			handler := auth.Middleware(nil, nil, nil, api.NewServer(s, nil, nil, nil).Handler())
			report := schemaRecoveryAPIReport("agent", "local-report")
			w := schemaRecoveryAPICall(t, handler, operation.method, "/internal/agents/agent"+operation.suffix, "schema-other", "synthetic-unauthenticated-token", report)
			if w.Code != 204 {
				t.Fatalf("local compatibility changed: status=%d body=%s", w.Code, w.Body.String())
			}
			schemaRecoveryAPIWant(t, s, report)
		})
	}
}

func TestAgentSchemaRecoveryStubOnlyAndPathIdentity(t *testing.T) {
	s := schemaRecoveryAPIStore(t)
	const target = "agent/雪?#%"
	schemaRecoveryAPISeed(t, s, target, "schema-owner", schemaRecoveryAPIStub(target))
	foreign := schemaRecoveryAPIReport("body-agent", "foreign-retained")
	schemaRecoveryAPISeed(t, s, foreign.AgentID, "schema-owner", foreign)
	handler := auth.Middleware(nil, nil, nil, api.NewServer(s, nil, nil, nil).Handler())
	path := "/internal/agents/" + url.PathEscape(target) + "/schema"
	body := schemaRecoveryAPIReport(foreign.AgentID, "recovered")
	w := schemaRecoveryAPICall(t, handler, "POST", path+"/recover", "", "", body)
	if w.Code != 204 {
		t.Fatalf("encoded path recovery: status=%d body=%s", w.Code, w.Body.String())
	}
	want := *body
	want.AgentID = target
	schemaRecoveryAPIWant(t, s, &want)
	schemaRecoveryAPIWant(t, s, foreign)
	replacement := schemaRecoveryAPIReport(foreign.AgentID, "ordinary-publication")
	for _, tc := range []struct{ method, suffix string }{{"POST", "/recover"}, {"PUT", ""}, {"POST", "/recover"}} {
		w = schemaRecoveryAPICall(t, handler, tc.method, path+tc.suffix, "", "", replacement)
		if w.Code != 204 {
			t.Fatalf("%s returned %d: %s", tc.method, w.Code, w.Body.String())
		}
		if tc.method == "PUT" {
			want = *replacement
			want.AgentID = target
		}
		schemaRecoveryAPIWant(t, s, &want)
	}
	schemaRecoveryAPIWant(t, s, foreign)
	for _, tc := range []struct {
		id     string
		schema *models.AgentSchema
	}{
		{"stub", schemaRecoveryAPIStub("stub")},
		{"partial", schemaRecoveryAPIStub("partial")},
		{"missing-row", nil},
	} {
		if tc.id == "partial" {
			tc.schema.StateSchema["title"] = "retained partial report"
		}
		schemaRecoveryAPISeed(t, s, tc.id, "schema-owner", tc.schema)
		candidate := replacement
		if tc.id == "stub" {
			candidate = schemaRecoveryAPIStub(tc.id)
		}
		w = schemaRecoveryAPICall(t, handler, "POST", "/internal/agents/"+tc.id+"/schema/recover", "", "", candidate)
		if w.Code != 204 {
			t.Fatalf("%s no-op returned %d", tc.id, w.Code)
		}
		if tc.schema != nil {
			schemaRecoveryAPIWant(t, s, tc.schema)
		} else {
			_, err := s.GetAgentSchema(t.Context(), tc.id)
			var notFound *state.ErrNotFound
			if !errors.As(err, &notFound) {
				t.Fatalf("missing row was inserted: %v", err)
			}
		}
	}
	w = schemaRecoveryAPICall(t, handler, "POST", "/internal/agents/unregistered/schema/recover", "", "", replacement)
	if w.Code != 404 {
		t.Fatalf("unknown agent returned %d, want 404", w.Code)
	}
}

type schemaRecoveryUnsupportedStore struct{ state.Store }

type schemaRecoveryFailureStore struct{ state.Store }

func (s schemaRecoveryFailureStore) RecoverAgentSchema(context.Context, *models.AgentSchema) (bool, error) {
	return false, errors.New("synthetic recovery storage failure")
}

func TestAgentSchemaRecoveryOptionalStoreAndStorageFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wrap   func(state.Store) state.Store
		status int
	}{
		{"unsupported", func(s state.Store) state.Store { return schemaRecoveryUnsupportedStore{s} }, 501},
		{"storage_failure", func(s state.Store) state.Store { return schemaRecoveryFailureStore{s} }, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := schemaRecoveryAPIStore(t)
			stub := schemaRecoveryAPIStub("agent")
			schemaRecoveryAPISeed(t, s, "agent", "schema-owner", stub)
			handler := api.NewServer(tc.wrap(s), nil, nil, nil).Handler()
			report := schemaRecoveryAPIReport("agent", "reported")
			w := schemaRecoveryAPICall(t, handler, "POST", "/internal/agents/agent/schema/recover", "", "", report)
			if w.Code != tc.status || strings.Contains(w.Body.String(), "synthetic recovery storage failure") {
				t.Fatalf("recovery status=%d body=%s, want %d", w.Code, w.Body.String(), tc.status)
			}
			schemaRecoveryAPIWant(t, s, stub)
			w = schemaRecoveryAPICall(t, handler, "PUT", "/internal/agents/agent/schema", "", "", report)
			if w.Code != 204 {
				t.Fatalf("ordinary publication requires recovery extension: %d %s", w.Code, w.Body.String())
			}
			schemaRecoveryAPIWant(t, s, report)
		})
	}
}
