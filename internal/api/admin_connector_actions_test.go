package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func connectorActionAdminHandler(s *Server) http.Handler {
	provider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"fixture-admin":  {Name: "operator", Permissions: []string{"admin"}},
		"fixture-reader": {Name: "reader", Permissions: []string{"read"}},
		"fixture-empty":  {Name: "unscoped"},
	})
	return auth.MiddlewareWithOpts(provider, nil, nil, auth.MiddlewareOpts{StrictPermissions: true}, s.Handler())
}

func connectorActionAdminGet(handler http.Handler, path, credential string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func seedForeignConnectorAction(t *testing.T, env *actionAPIEnv) string {
	t.Helper()
	ctx := tenant.WithContext(context.Background(), "beta")
	now := time.Now().UTC()
	if err := env.store.CreateThread(ctx, &models.Thread{ThreadID: "beta-thread", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.CreateRun(ctx, &models.Run{RunID: "beta-run", ThreadID: "beta-thread", AgentID: "other-agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if accepted, err := env.store.BeginRunAttempt(ctx, "beta-run", 1); err != nil || !accepted {
		t.Fatalf("start foreign fixture: accepted=%v error=%v", accepted, err)
	}
	_, revision := fixtureTargetRegistry(t, "other-connector")
	action, err := env.store.CreateConnectorAction(ctx, &models.ConnectorAction{
		ID: uuid.NewString(), TenantID: "beta", RunID: "beta-run", ThreadID: "beta-thread", AgentID: "other-agent",
		Generation: 1, Connector: "other-connector", Tool: "read", ArgsDigest: strings.Repeat("a", 64),
		TargetRevision: &revision,
	}, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.RecordConnectorActionDecision(ctx, action.ID, action.Version, models.ConnectorActionDecision{Effect: "deny", AuthorizationSource: "policy"}, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	return action.ID
}

func TestAdminConnectorActionsRoutesRequireExplicitAdmin(t *testing.T) {
	env := newActionAPIEnv(t, "json", "policy")
	actionID := env.call(t).Header().Get(headerConnectorActionID)
	handler := connectorActionAdminHandler(env.s)
	for _, path := range []string{"/admin-api/connector-actions", "/admin-api/connector-actions/" + actionID} {
		for _, tc := range []struct {
			key  string
			want int
		}{
			{"", http.StatusUnauthorized},
			{"wrong", http.StatusUnauthorized},
			{"fixture-reader", http.StatusForbidden},
			{"fixture-empty", http.StatusForbidden},
			{"fixture-admin", http.StatusOK},
		} {
			response := connectorActionAdminGet(handler, path, tc.key)
			if response.Code != tc.want {
				t.Fatalf("route=%s credential=%q code=%d want=%d", path, tc.key, response.Code, tc.want)
			}
		}
	}
}

func TestAdminConnectorActionsFiltersCursorAndLookahead(t *testing.T) {
	env := newActionAPIEnv(t, "json", "policy")
	ids := make([]string, 3)
	for i := range ids {
		ids[i] = env.call(t).Header().Get(headerConnectorActionID)
	}
	foreignID := seedForeignConnectorAction(t, env)
	handler := connectorActionAdminHandler(env.s)
	decode := func(path string) (*httptest.ResponseRecorder, []*models.ConnectorAction) {
		t.Helper()
		response := connectorActionAdminGet(handler, path, "fixture-admin")
		var rows []*models.ConnectorAction
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &rows) != nil {
			t.Fatalf("list failed: code=%d body=%s", response.Code, response.Body.String())
		}
		return response, rows
	}
	page1, first := decode("/admin-api/connector-actions?tenant_id=acme&limit=2")
	cursor := page1.Header().Get(pagecursor.HeaderNextCursor)
	if len(first) != 2 || first[0].ID != ids[2] || first[1].ID != ids[1] || cursor == "" {
		t.Fatalf("unexpected first page: rows=%+v cursor=%q", first, cursor)
	}
	page2, second := decode("/admin-api/connector-actions?tenant_id=acme&limit=2&cursor=" + url.QueryEscape(cursor))
	if len(second) != 1 || second[0].ID != ids[0] || page2.Header().Get(pagecursor.HeaderNextCursor) != "" {
		t.Fatal("cursor repeated or skipped an action, or claimed a nonexistent next page")
	}
	exact, rows := decode("/admin-api/connector-actions?tenant_id=acme&limit=3")
	if len(rows) != 3 || exact.Header().Get(pagecursor.HeaderNextCursor) != "" {
		t.Fatal("exactly full terminal page advertised a nonexistent next page")
	}
	for _, tc := range []struct {
		query string
		count int
		id    string
	}{
		{"tenant_id=beta", 1, foreignID},
		{"run_id=beta-run", 1, foreignID},
		{"agent_id=other-agent", 1, foreignID},
		{"connector=other-connector", 1, foreignID},
		{"state=blocked", 1, foreignID},
		{"tenant_id=acme&state=blocked", 0, ""},
		{"tenant_id=beta&run_id=run", 0, ""},
		{"tenant_id=missing", 0, ""},
	} {
		_, rows := decode("/admin-api/connector-actions?" + tc.query)
		if len(rows) != tc.count || tc.id != "" && rows[0].ID != tc.id {
			t.Fatalf("filters not reapplied: query=%q count=%d", tc.query, len(rows))
		}
	}
}

func TestAdminConnectorActionsRejectInvalidCursorAndState(t *testing.T) {
	env := newActionAPIEnv(t, "json", "policy")
	handler := connectorActionAdminHandler(env.s)
	for _, query := range []string{
		"cursor=not-a-cursor", "state=completed", "cursor=" + strings.Repeat("a", 4097),
		"cursor=" + url.QueryEscape(pagecursor.EncodeKey("a", "b")),
		"cursor=" + url.QueryEscape(pagecursor.EncodeTime(time.Now(), "id")) + "&offset=1",
	} {
		response := connectorActionAdminGet(handler, "/admin-api/connector-actions?"+query, "fixture-admin")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid action query accepted: code=%d query=%q", response.Code, query)
		}
	}
}

func TestAdminConnectorActionDetailScopeAndMetadataOnly(t *testing.T) {
	env := newActionAPIEnv(t, "json", "policy")
	actionID := env.call(t).Header().Get(headerConnectorActionID)
	handler := connectorActionAdminHandler(env.s)
	path := "/admin-api/connector-actions/" + actionID
	for _, query := range []string{"?tenant_id=beta", "?run_id=other-run", "?tenant_id=ACME"} {
		if response := connectorActionAdminGet(handler, path+query, "fixture-admin"); response.Code != http.StatusNotFound {
			t.Fatalf("wrong-scope detail accepted: code=%d query=%q", response.Code, query)
		}
	}
	response := connectorActionAdminGet(handler, path+"?tenant_id=acme&run_id=run", "fixture-admin")
	var detail adminConnectorActionDetail
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &detail) != nil || detail.Action == nil || detail.Action.ID != actionID || !detail.RunAvailable || len(detail.Events) != 4 {
		t.Fatalf("incorrect detail: code=%d body=%s", response.Code, response.Body.String())
	}
	for _, forbidden := range []string{"synthetic-sensitive", "127.0.0.1", `"arguments"`, `"args"`, `"access_token"`, `"credentials"`} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("detail retained prohibited payload or endpoint content: %q", forbidden)
		}
	}
	if missing := connectorActionAdminGet(handler, "/admin-api/connector-actions/"+uuid.NewString(), "fixture-admin"); missing.Code != http.StatusNotFound {
		t.Fatalf("missing action detail code=%d", missing.Code)
	}
}

func TestAdminConnectorActionHistorySurvivesDeletionAndIdenticalTimestampReuse(t *testing.T) {
	env := newActionAPIEnv(t, "json", "policy")
	actionID := env.call(t).Header().Get(headerConnectorActionID)
	original, err := env.store.GetRun(env.ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	handler := connectorActionAdminHandler(env.s)
	path := "/admin-api/connector-actions/" + actionID
	assertArchived := func() {
		t.Helper()
		response := connectorActionAdminGet(handler, path, "fixture-admin")
		var detail adminConnectorActionDetail
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &detail) != nil || detail.Action == nil || detail.RunAvailable || len(detail.Events) != 4 {
			t.Fatalf("archived evidence disappeared or linked a replacement run: code=%d body=%s", response.Code, response.Body.String())
		}
	}
	if err := env.store.DeleteRun(env.ctx, "run"); err != nil {
		t.Fatal(err)
	}
	assertArchived()
	// Preserve every former attribution field, including the stored timestamp.
	// A distinct DB-issued incarnation must distinguish this replacement.
	if err := env.store.CreateRun(env.ctx, &models.Run{RunID: original.RunID, ThreadID: original.ThreadID, AgentID: original.AgentID, Status: models.RunStatusPending, Metadata: original.Metadata, CreatedAt: original.CreatedAt, UpdatedAt: original.UpdatedAt}); err != nil {
		t.Fatal(err)
	}
	assertArchived()
}

type actionUnsupportedStore struct{ state.Store }

func TestAdminConnectorActionHistoryUnsupportedBackendReturns501(t *testing.T) {
	env := newActionAPIEnv(t, "json", "policy")
	// Hide the optional capability while retaining the ordinary run-store API,
	// matching a backend such as Mongo which does not implement action evidence.
	s := NewServer(actionUnsupportedStore{Store: env.store}, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	handler := connectorActionAdminHandler(s)
	for _, path := range []string{"/admin-api/connector-actions", "/admin-api/connector-actions/" + uuid.NewString()} {
		response := connectorActionAdminGet(handler, path, "fixture-admin")
		if response.Code != http.StatusNotImplemented {
			t.Fatalf("unsupported backend code=%d path=%s", response.Code, path)
		}
	}
}
