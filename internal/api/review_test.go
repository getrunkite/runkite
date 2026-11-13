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

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
	"github.com/google/uuid"
)

type reviewFixture struct {
	server  *Server
	store   *sqlite.SQLiteStore
	handler http.Handler
	pending map[string][]string
	actions map[string][]string
}

func newReviewFixture(t *testing.T) *reviewFixture {
	t.Helper()
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := NewServer(db, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	registry, targetRevision := fixtureTargetRegistry(t, "files")
	s.SetConnectorRegistry(registry)
	s.SetPolicyEngine(policy.New(policy.Config{ForceEnable: true, DefaultEffect: policy.EffectAllow, AuditRequired: true, Auditor: policy.NewStoreAuditor(db)}))
	provider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"read-a":    {Name: "reader-a", TenantID: "a", Permissions: []string{auth.GovernanceReadPermission}},
		"approve-a": {Name: "approver-a", TenantID: "a", Permissions: []string{auth.GovernanceApprovePermission}},
		"approve-b": {Name: "approver-b", TenantID: "b", Permissions: []string{auth.GovernanceApprovePermission}},
		"admin-a":   {Name: "admin-a", TenantID: "a", Permissions: []string{"admin"}},
		"unscoped":  {Name: "unscoped", Permissions: []string{auth.GovernanceApprovePermission}},
		"ordinary":  {Name: "ordinary", TenantID: "a", Permissions: []string{"read"}},
	})
	sessions := auth.NewReviewSessionStore(0)
	s.SetReviewSessions(&auth.ReviewSessionHandlers{Store: sessions, Provider: provider})
	out := &reviewFixture{server: s, store: db, pending: map[string][]string{}, actions: map[string][]string{}}
	out.handler = auth.MiddlewareWithOpts(provider, nil, nil, auth.MiddlewareOpts{StrictPermissions: true, ReviewSessions: sessions}, s.Handler())
	for _, scope := range []string{"a", "b"} {
		ctx := tenant.ScopedContext(t.Context(), scope)
		binding := &auth.RunBinding{RunID: "run-" + scope, ThreadID: "thread-" + scope, TenantID: scope, AgentID: "agent", Generation: 1}
		seedManagedPendingFixture(t, db, binding)
		for i := 0; i < 3; i++ {
			_, digest, args := policy.BindArgs(json.RawMessage(`{"amount":123,"apiKey":"synthetic-credential"}`))
			row := &models.PendingAction{ID: "pending-v2-" + uuid.NewString(), TenantID: scope, AgentID: "agent", RunID: binding.RunID, Generation: 1, Connector: "files", Tool: "write", ArgsDigest: digest, Args: args, Reason: "synthetic-sensitive-freeform", ReasonCode: "mandatory_hitl", TargetRevision: targetRevision}
			// Distinct operations avoid the existing open-action deduplication key.
			row.Tool += strings.Repeat("x", i)
			if err := db.CreatePendingAction(ctx, row); err != nil {
				t.Fatal(err)
			}
			out.pending[scope] = append(out.pending[scope], row.ID)
			action, err := db.CreateConnectorAction(ctx, &models.ConnectorAction{ID: uuid.NewString(), TenantID: scope, AgentID: "agent", RunID: binding.RunID, ThreadID: binding.ThreadID, Generation: 1, Connector: "files", Tool: row.Tool, ArgsDigest: digest, TargetRevision: &targetRevision}, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.RecordConnectorActionDecision(ctx, action.ID, action.Version, models.ConnectorActionDecision{Effect: "allow", AuthorizationSource: "policy"}, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			out.actions[scope] = append(out.actions[scope], action.ID)
		}
	}
	return out
}

func reviewRequest(handler http.Handler, method, path, key, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set(auth.HeaderCSRF, csrf)
	}
	r.Header.Set("X-Tenant-ID", "b")
	// A stale internal marker must not grant cross-tenant authority.
	r = r.WithContext(tenant.SystemContext(r.Context()))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestReviewPendingRolesScopeAndProjection(t *testing.T) {
	env := newReviewFixture(t)
	for _, key := range []string{"read-a", "approve-a", "admin-a"} {
		w := reviewRequest(env.handler, "GET", "/review-api/pending-actions", key, "", nil, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), "synthetic-sensitive-freeform") || strings.Contains(w.Body.String(), "synthetic-credential") {
			t.Fatalf("projection key=%s code=%d body=%s", key, w.Code, w.Body.String())
		}
		var rows []models.PendingAction
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 3 {
			t.Fatalf("rows=%+v err=%v", rows, err)
		}
		for _, a := range rows {
			if a.TenantID != "a" || (key == "read-a" && a.Args != nil) || (key != "read-a" && a.Args == nil) {
				t.Fatalf("incorrect role projection: %+v", a)
			}
		}
	}
	for _, key := range []string{"read-a", "approve-a"} {
		for _, path := range []string{"/review-api/pending-actions/" + env.pending["b"][0], "/review-api/connector-actions/" + env.actions["b"][0], "/review-api/pending-actions/missing"} {
			w := reviewRequest(env.handler, "GET", path, key, "", nil, "")
			if w.Code != 404 {
				t.Fatalf("foreign lookup %s: %d %s", path, w.Code, w.Body.String())
			}
		}
		for _, path := range []string{"/review-api/pending-actions?tenant_id=b", "/review-api/connector-actions?tenant_id=a&tenant_id=b"} {
			w := reviewRequest(env.handler, "GET", path, key, "", nil, "")
			if w.Code != 403 {
				t.Fatalf("foreign query accepted: %d %s", w.Code, w.Body.String())
			}
		}
	}
	for _, operation := range []string{"approve", "deny"} {
		path := "/review-api/pending-actions/" + env.pending["a"][0] + "/" + operation
		w := reviewRequest(env.handler, "POST", path, "read-a", "", nil, "")
		if w.Code != 403 {
			t.Fatal("reader mutated pending action")
		}
		path = "/review-api/pending-actions/" + env.pending["b"][0] + "/" + operation
		w = reviewRequest(env.handler, "POST", path, "approve-a", "", nil, "")
		if w.Code != 404 {
			t.Fatalf("foreign mutation: %d %s", w.Code, w.Body.String())
		}
	}
	for _, key := range []string{"", "bad", "ordinary", "unscoped"} {
		w := reviewRequest(env.handler, "GET", "/review-api/pending-actions", key, "", nil, "")
		if w.Code != 401 && w.Code != 403 {
			t.Fatalf("unqualified key %q got %d", key, w.Code)
		}
	}
	for _, path := range []string{"/admin-api/pending-actions", "/admin-api/connector-actions", "/admin-api/runs", "/threads/thread-a", "/review-api/audit-events", "/review-api/runs"} {
		w := reviewRequest(env.handler, "GET", path, "approve-a", "", nil, "")
		if w.Code != 403 {
			t.Fatalf("review authority escaped to %s: %d", path, w.Code)
		}
	}
	// Global Admin remains an explicitly different surface.
	w := reviewRequest(env.handler, "GET", "/admin-api/pending-actions", "admin-a", "", nil, "")
	var global []models.PendingAction
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &global) != nil || len(global) != 6 {
		t.Fatalf("global Admin changed: %d %s", w.Code, w.Body.String())
	}
}

func TestReviewPagingValidationAndActionPrivacy(t *testing.T) {
	env := newReviewFixture(t)
	for _, resource := range []string{"pending-actions", "connector-actions"} {
		path := "/review-api/" + resource
		first := reviewRequest(env.handler, "GET", path+"?limit=2", "read-a", "", nil, "")
		cursor := first.Header().Get(pagecursor.HeaderNextCursor)
		if first.Code != 200 || cursor == "" {
			t.Fatalf("no first page: %d %s", first.Code, first.Body.String())
		}
		last := reviewRequest(env.handler, "GET", path+"?limit=2&cursor="+url.QueryEscape(cursor), "read-a", "", nil, "")
		var tail []map[string]any
		if last.Code != 200 || json.Unmarshal(last.Body.Bytes(), &tail) != nil || len(tail) != 1 || last.Header().Get(pagecursor.HeaderNextCursor) != "" {
			t.Fatalf("bad terminal page: %d %s", last.Code, last.Body.String())
		}
		exact := reviewRequest(env.handler, "GET", path+"?limit=3", "read-a", "", nil, "")
		if exact.Code != 200 || exact.Header().Get(pagecursor.HeaderNextCursor) != "" {
			t.Fatal("full terminal page advertised a next page")
		}
		for _, q := range []string{"cursor=bad", "cursor=" + strings.Repeat("x", 4097), "limit=0", "limit=bad", "offset=-1", "cursor=" + url.QueryEscape(cursor) + "&offset=1"} {
			w := reviewRequest(env.handler, "GET", path+"?"+q, "read-a", "", nil, "")
			if w.Code != 400 {
				t.Fatalf("invalid query %q: %d", q, w.Code)
			}
		}
		// A cursor obtained in the other tenant remains only an ordering filter.
		foreign := reviewRequest(env.handler, "GET", path+"?limit=1", "approve-b", "", nil, "")
		w := reviewRequest(env.handler, "GET", path+"?cursor="+url.QueryEscape(foreign.Header().Get(pagecursor.HeaderNextCursor)), "read-a", "", nil, "")
		var rows []map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rows) != nil {
			t.Fatal("foreign cursor failed")
		}
		for _, row := range rows {
			if row["tenant_id"] != "a" {
				t.Fatal("foreign cursor escaped tenant scope")
			}
		}
	}
	for _, q := range []string{"/review-api/pending-actions?status=madeup", "/review-api/connector-actions?state=madeup"} {
		if w := reviewRequest(env.handler, "GET", q, "read-a", "", nil, ""); w.Code != 400 {
			t.Fatal("invalid state accepted")
		}
	}
	w := reviewRequest(env.handler, "GET", "/review-api/connector-actions/"+env.actions["a"][0], "read-a", "", nil, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "run_available") || strings.Contains(w.Body.String(), `"args"`) || strings.Contains(w.Body.String(), "synthetic-") {
		t.Fatalf("action privacy: %d %s", w.Code, w.Body.String())
	}
	var detail reviewConnectorActionDetail
	if json.Unmarshal(w.Body.Bytes(), &detail) != nil || len(detail.Events) != 2 {
		t.Fatal("missing action timeline")
	}
}

func TestReviewApproveDenyBodyAndOriginalSafety(t *testing.T) {
	env := newReviewFixture(t)
	id := env.pending["a"][0]
	path := "/review-api/pending-actions/" + id + "/approve"
	for _, body := range []string{`{"tenant_id":"b"}`, `{"role":"admin"}`, `{"args":{"amount":1}}`, strings.Repeat("x", 1025)} {
		if w := reviewRequest(env.handler, "POST", path, "approve-a", body, nil, ""); w.Code != 400 {
			t.Fatal("decision accepted authority fields")
		}
	}
	w := reviewRequest(env.handler, "POST", path, "approve-a", "{}", nil, "")
	var approved models.PendingAction
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &approved) != nil || approved.Status != models.PendingStatusApproved || approved.DecidedBy != "approver-a" || approved.Reason != "" {
		t.Fatalf("approval: %d %s", w.Code, w.Body.String())
	}
	ctx := tenant.ScopedContext(t.Context(), "a")
	if got, err := env.store.ConsumeApprovedAction(ctx, "a", "agent", "run-a", 1, "files", "write", "wrong", approved.TargetRevision); err != nil || got != "" {
		t.Fatalf("mismatched digest consumed: %s %v", got, err)
	}
	if got, err := env.store.ConsumeApprovedAction(ctx, "a", "agent", "run-a", 2, "files", "write", approved.ArgsDigest, approved.TargetRevision); err != nil || got != "" {
		t.Fatalf("other generation consumed approval: %s %v", got, err)
	}
	for i := 0; i < 2; i++ {
		got, err := env.store.ConsumeApprovedAction(ctx, "a", "agent", "run-a", 1, "files", "write", approved.ArgsDigest, approved.TargetRevision)
		if err != nil || (i == 0 && got != id) || (i == 1 && got != "") {
			t.Fatalf("one-shot i=%d id=%s err=%v", i, got, err)
		}
	}
	denyPath := "/review-api/pending-actions/" + env.pending["a"][1] + "/deny"
	if w := reviewRequest(env.handler, "POST", denyPath, "approve-a", "", nil, ""); w.Code != 200 || strings.Contains(w.Body.String(), "synthetic-sensitive-freeform") {
		t.Fatalf("denial: %d %s", w.Code, w.Body.String())
	}
	env.server.SetPolicyEngine(policy.New(policy.Config{ForceEnable: true, DefaultEffect: policy.EffectDeny, AuditRequired: true, Auditor: policy.NewStoreAuditor(env.store)}))
	blocked := reviewRequest(env.handler, "POST", "/review-api/pending-actions/"+env.pending["a"][2]+"/approve", "approve-a", "", nil, "")
	if blocked.Code != 409 {
		t.Fatalf("explicit deny bypassed: %d %s", blocked.Code, blocked.Body.String())
	}
}

func TestReviewCookieCSRFAndIsolation(t *testing.T) {
	env := newReviewFixture(t)
	login := reviewRequest(env.handler, "POST", "/review-api/session", "", `{"credential":"approve-a"}`, nil, "")
	var result struct {
		CSRF   string `json:"csrf_token"`
		Tenant string `json:"tenant_id"`
	}
	if login.Code != 200 || json.Unmarshal(login.Body.Bytes(), &result) != nil || result.CSRF == "" || result.Tenant != "a" || len(login.Result().Cookies()) != 1 {
		t.Fatalf("login: %d %s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	if w := reviewRequest(env.handler, "GET", "/review-api/pending-actions", "", "", cookie, ""); w.Code != 200 {
		t.Fatal("cookie read failed")
	}
	path := "/review-api/pending-actions/" + env.pending["a"][0] + "/approve"
	if w := reviewRequest(env.handler, "POST", path, "", "", cookie, ""); w.Code != 403 {
		t.Fatal("cookie mutation without CSRF succeeded")
	}
	if w := reviewRequest(env.handler, "POST", path, "", "", cookie, result.CSRF); w.Code != 200 {
		t.Fatalf("cookie approval failed: %d %s", w.Code, w.Body.String())
	}
	if w := reviewRequest(env.handler, "GET", "/admin-api/pending-actions", "", "", cookie, ""); w.Code != 401 {
		t.Fatalf("review cookie authenticated Admin: %d", w.Code)
	}
	if w := reviewRequest(env.handler, "DELETE", "/review-api/session", "", "", cookie, result.CSRF); w.Code != 204 {
		t.Fatal("review logout failed")
	}
	if w := reviewRequest(env.handler, "GET", "/review-api/pending-actions", "", "", cookie, ""); w.Code != 401 {
		t.Fatal("revoked review session remained usable")
	}
}

func TestReviewNoAuthAndDirectHandlerFailClosed(t *testing.T) {
	env := newReviewFixture(t)
	handler := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{}, env.server.Handler())
	if w := reviewRequest(handler, "GET", "/review-api/pending-actions", "", "", nil, ""); w.Code != 401 {
		t.Fatalf("review default-open: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/review-api/pending-actions", nil).WithContext(tenant.SystemContext(context.Background()))
	w := httptest.NewRecorder()
	env.server.handleReviewListPendingActions(w, r)
	if w.Code != 403 {
		t.Fatal("direct handler inherited global context")
	}
}

func TestReviewApprovalRetainsExpiryLegacyAuditAndIncarnationChecks(t *testing.T) {
	for _, mode := range []string{"expired", "legacy", "audit-failed", "replaced-run"} {
		t.Run(mode, func(t *testing.T) {
			env := newReviewFixture(t)
			ctx := tenant.ScopedContext(t.Context(), "a")
			id := env.pending["a"][0]
			switch mode {
			case "expired", "legacy":
				past := time.Now().UTC().Add(-time.Hour)
				row := &models.PendingAction{ID: "pending-v2-" + uuid.NewString(), RunID: "independent-operation", TenantID: "a", AgentID: "agent", Connector: "files", Tool: mode, ArgsDigest: "exact-digest"}
				if mode == "expired" {
					row.ExpiresAt = &past
				} else {
					row.TargetRevision = currentFixtureTargetRevision(t, env.server, "files")
				}
				if err := env.store.CreatePendingAction(ctx, row); err != nil {
					t.Fatal(err)
				}
				id = row.ID
			case "audit-failed":
				auditor := &switchablePolicyAuditor{store: policy.NewStoreAuditor(env.store)}
				auditor.failed.Store(true)
				env.server.SetPolicyEngine(policy.New(policy.Config{ForceEnable: true, DefaultEffect: policy.EffectAllow, AuditRequired: true, Auditor: auditor}))
			case "replaced-run":
				old, err := env.store.GetRun(ctx, "run-a")
				if err != nil {
					t.Fatal(err)
				}
				if err := env.store.DeleteRun(ctx, old.RunID); err != nil {
					t.Fatal(err)
				}
				old.IncarnationID = ""
				if err := env.store.CreateRun(ctx, old); err != nil {
					t.Fatal(err)
				}
				if accepted, err := env.store.BeginRunAttempt(ctx, old.RunID, 1); err != nil || !accepted {
					t.Fatalf("replacement begin: %v %v", accepted, err)
				}
			}
			w := reviewRequest(env.handler, "POST", "/review-api/pending-actions/"+id+"/approve", "approve-a", "", nil, "")
			if w.Code != 409 {
				t.Fatalf("%s approval accepted: %d %s", mode, w.Code, w.Body.String())
			}
			row, err := env.store.GetPendingAction(ctx, id)
			if err != nil || row.Status != models.PendingStatusPending {
				t.Fatalf("failed approval changed row: %+v %v", row, err)
			}
		})
	}
}

func TestReviewConcurrentApprovalsAndUnsupportedBackend(t *testing.T) {
	env := newReviewFixture(t)
	path := "/review-api/pending-actions/" + env.pending["a"][0] + "/approve"
	results := make(chan int, 12)
	for i := 0; i < 12; i++ {
		go func() { results <- reviewRequest(env.handler, "POST", path, "approve-a", "", nil, "").Code }()
	}
	successful := 0
	for i := 0; i < 12; i++ {
		status := <-results
		if status == 200 {
			successful++
		} else if status != 409 {
			t.Fatalf("unexpected concurrent status=%d", status)
		}
	}
	if successful != 1 {
		t.Fatalf("concurrent approvals=%d", successful)
	}
	env.server.store = actionUnsupportedStore{Store: env.store}
	for _, path := range []string{"/review-api/pending-actions", "/review-api/pending-actions/unknown", "/review-api/connector-actions", "/review-api/connector-actions/unknown"} {
		if w := reviewRequest(env.handler, "GET", path, "read-a", "", nil, ""); w.Code != 501 {
			t.Fatalf("unsupported %s: %d", path, w.Code)
		}
	}
}
