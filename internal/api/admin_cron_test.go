package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

// Route regressions run through the real mux and SQLite without a listener.
func newCronHistoryEnv(t *testing.T) *testEnv {
	t.Helper()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	queue := inprocess.NewQueue()
	broker := inprocess.NewBroker()
	cancel := inprocess.NewCancelBus()
	return &testEnv{store: store, queue: queue, broker: broker, cancel: cancel, apiServer: api.NewServer(store, queue, broker, cancel)}
}

type cronHistoryReadStore struct {
	state.Store
	threadErr, runErr error
	historyReads      int
}

func (s *cronHistoryReadStore) GetThread(ctx context.Context, id string) (*models.Thread, error) {
	s.historyReads++
	if s.threadErr != nil {
		return nil, s.threadErr
	}
	return s.Store.GetThread(ctx, id)
}

func (s *cronHistoryReadStore) SearchRuns(ctx context.Context, req *models.RunSearchRequest) ([]*models.Run, error) {
	s.historyReads++
	if s.runErr != nil {
		return nil, s.runErr
	}
	return s.Store.SearchRuns(ctx, req)
}

type cronHistoryResponse struct {
	TenantID        string `json:"tenant_id"`
	Name            string `json:"name"`
	ThreadID        string `json:"thread_id"`
	ThreadAvailable bool   `json:"thread_available"`
	HistoryStatus   string `json:"history_status"`
	LatestRun       *struct {
		RunID         string           `json:"run_id"`
		IncarnationID string           `json:"incarnation_id"`
		Status        models.RunStatus `json:"status"`
	} `json:"latest_run"`
}

func TestAdminCronHistoryUsesScheduleTenantAndLatestRetainedRun(t *testing.T) {
	env := newCronHistoryEnv(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, tid := range []string{"tenant-a", "tenant-b"} {
		ctx := tenant.WithContext(context.Background(), tid)
		if err := env.store.UpsertCronSchedule(ctx, &models.CronSchedule{
			Name: "daily-report", AgentID: "new-agent", Expression: "0 9 * * *", Enabled: false,
		}); err != nil {
			t.Fatal(err)
		}
	}
	ctx := tenant.WithContext(context.Background(), "tenant-a")
	if err := env.store.CreateThread(ctx, &models.Thread{
		ThreadID: "cron:daily-report", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for i, status := range []models.RunStatus{models.RunStatusSuccess, models.RunStatusError} {
		created := now.Add(time.Duration(i) * time.Minute)
		if err := env.store.CreateRun(ctx, &models.Run{
			RunID: []string{"old-run", "latest-run"}[i], ThreadID: "cron:daily-report", AgentID: "previous-agent",
			Status: status, CreatedAt: created, UpdatedAt: created,
			Input: json.RawMessage(`{"secret":"private-run-input"}`), Output: json.RawMessage(`{"secret":"private-run-output"}`),
			Error: "private-provider-error", Metadata: map[string]interface{}{"private": "private-run-metadata"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin-api/cron?include_history=true", nil)
	env.apiServer.Handler().ServeHTTP(w, r.WithContext(tenant.SystemContext(r.Context())))
	if w.Code != http.StatusOK {
		t.Fatalf("history status=%d body=%s", w.Code, w.Body.String())
	}
	raw := w.Body.Bytes()
	if strings.Contains(string(raw), "private-") {
		t.Fatalf("schedule history exposed run payload: %s", raw)
	}
	var rows []cronHistoryResponse
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) != 2 {
		t.Fatalf("history decode=%v rows=%s", err, raw)
	}
	for _, row := range rows {
		if row.TenantID == "tenant-a" {
			if row.ThreadID != "cron:daily-report" || !row.ThreadAvailable || row.HistoryStatus != "available" || row.LatestRun == nil || row.LatestRun.RunID != "latest-run" || row.LatestRun.Status != models.RunStatusError || row.LatestRun.IncarnationID == "" {
				t.Fatalf("latest retained run missing after schedule agent changed: %+v", row)
			}
		} else if !strings.HasPrefix(row.ThreadID, "cron-tenant:v1:") || row.ThreadAvailable || row.HistoryStatus != "empty" || row.LatestRun != nil {
			t.Fatalf("same schedule name exposed another tenant's history: %+v", row)
		}
	}
}

func TestAdminCronHistoryPreservesReferenceAcrossActualRunIDReplacement(t *testing.T) {
	env := newCronHistoryEnv(t)
	ctxA := tenant.ScopedContext(context.Background(), "tenant-a")
	ctxB := tenant.ScopedContext(context.Background(), "tenant-b")
	now := time.Now().UTC()
	if err := env.store.UpsertCronSchedule(ctxA, &models.CronSchedule{Name: "daily", AgentID: "agent", Expression: "0 9 * * *"}); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		ctx context.Context
		id  string
	}{{ctxA, "cron:daily"}, {ctxB, "different-thread"}} {
		if err := env.store.CreateThread(fixture.ctx, &models.Thread{ThreadID: fixture.id, Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := env.store.CreateRun(ctxA, &models.Run{RunID: "reused", ThreadID: "cron:daily", AgentID: "agent", Status: models.RunStatusSuccess, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	env.apiServer.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin-api/cron?include_history=true", nil))
	var rows []cronHistoryResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rows) != nil || len(rows) != 1 || rows[0].LatestRun == nil || rows[0].LatestRun.IncarnationID == "" {
		t.Fatalf("missing retained link proof: %d %s", w.Code, w.Body.String())
	}
	reference := rows[0].LatestRun.IncarnationID
	if err := env.store.DeleteRun(ctxA, "reused"); err != nil {
		t.Fatal(err)
	}
	if err := env.store.CreateRun(ctxB, &models.Run{RunID: "reused", ThreadID: "different-thread", AgentID: "foreign", Status: models.RunStatusRunning, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	actual := httptest.NewRecorder()
	env.apiServer.Handler().ServeHTTP(actual, httptest.NewRequest(http.MethodGet, "/admin-api/runs/reused", nil))
	var replacement struct {
		TenantID      string `json:"tenant_id"`
		IncarnationID string `json:"incarnation_id"`
	}
	if actual.Code != http.StatusOK || json.Unmarshal(actual.Body.Bytes(), &replacement) != nil || replacement.TenantID != "tenant-b" || replacement.IncarnationID == "" || replacement.IncarnationID == reference {
		t.Fatalf("fixture failed to replace the actual stored record: %d %s", actual.Code, actual.Body.String())
	}
	if rows[0].TenantID != "tenant-a" || rows[0].LatestRun.IncarnationID != reference {
		t.Fatal("captured schedule history followed the replacement")
	}
}

func TestAdminCronHistoryKeepsAnEmptyRetainedThreadBrowsable(t *testing.T) {
	env := newCronHistoryEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := env.store.UpsertCronSchedule(ctx, &models.CronSchedule{Name: "pruned", AgentID: "agent", Expression: "0 9 * * *"}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.CreateThread(ctx, &models.Thread{ThreadID: "cron:pruned", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	env.apiServer.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin-api/cron?include_history=true", nil))
	var rows []cronHistoryResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rows) != nil || len(rows) != 1 {
		t.Fatalf("history read failed: %d %s", w.Code, w.Body.String())
	}
	if !rows[0].ThreadAvailable || rows[0].HistoryStatus != "empty" || rows[0].LatestRun != nil {
		t.Fatalf("empty retained history was mistaken for a missing thread or dispatch failure: %s", w.Body.String())
	}
}

func TestAdminCronHistoryIsOptInAndReadFailuresAreNotEmptyHistory(t *testing.T) {
	for _, failedRead := range []string{"thread", "runs"} {
		t.Run(failedRead, func(t *testing.T) {
			env := newCronHistoryEnv(t)
			if err := env.store.UpsertCronSchedule(context.Background(), &models.CronSchedule{
				Name: "new-schedule", AgentID: "agent", Expression: "0 9 * * *", Enabled: true,
			}); err != nil {
				t.Fatal(err)
			}
			store := &cronHistoryReadStore{Store: env.store}
			if failedRead == "thread" {
				store.threadErr = errors.New("private storage failure")
			} else {
				store.runErr = errors.New("private storage failure")
			}
			handler := api.NewServer(store, env.queue, env.broker, env.cancel).Handler()
			plain := httptest.NewRecorder()
			handler.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/admin-api/cron", nil))
			if plain.Code != http.StatusOK || store.historyReads != 0 || strings.Contains(plain.Body.String(), "history_status") {
				t.Fatalf("definition-only list acquired history reads or fields: %d %s", store.historyReads, plain.Body.String())
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin-api/cron?include_history=true", nil))
			var rows []cronHistoryResponse
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rows) != nil || len(rows) != 1 {
				t.Fatalf("partial read failure lost definitions: code=%d %s", w.Code, w.Body.String())
			}
			if rows[0].HistoryStatus != "unavailable" || strings.Contains(w.Body.String(), "private storage failure") {
				t.Fatalf("read failure was labelled empty or leaked details: %s", w.Body.String())
			}
		})
	}
}

func TestAdminCronThreadOperationsHonorExactTenantSelection(t *testing.T) {
	env := newCronHistoryEnv(t)
	ctx := tenant.WithContext(context.Background(), " tenant-a ")
	now := time.Now().UTC()
	const threadID = "cron:shared"
	if err := env.store.CreateThread(ctx, &models.Thread{ThreadID: threadID, Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.CreateRun(ctx, &models.Run{RunID: "tenant-a-run", ThreadID: threadID, AgentID: "agent", Status: models.RunStatusSuccess, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	handler := env.apiServer.Handler()
	for _, tid := range []string{"tenant-b", "tenant-a", ""} {
		query := "?tenant_id=" + url.QueryEscape(tid)
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(method, "/admin-api/threads/"+threadID+query, nil)
			// Even an inherited system marker cannot broaden an explicit tenant.
			r = r.WithContext(tenant.SystemContext(r.Context()))
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusNotFound {
				t.Fatalf("%s wrong tenant %q selected foreign thread: %d %s", method, tid, w.Code, w.Body.String())
			}
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin-api/threads/"+threadID+"/runs"+query, nil))
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
			t.Fatalf("wrong tenant %q selected foreign runs: %d %s", tid, w.Code, w.Body.String())
		}
	}
	for _, suffix := range []string{"", "/runs"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin-api/threads/"+threadID+suffix+"?tenant_id="+url.QueryEscape(" tenant-a "), nil))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), " tenant-a ") {
			t.Fatalf("exact tenant could not read its retained history: %d %s", w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/admin-api/threads/"+threadID+"?tenant_id="+url.QueryEscape(" tenant-a "), nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("exact tenant could not delete its thread: %d %s", w.Code, w.Body.String())
	}
}

func TestAdminRetainedRunReadGuardRejectsReplacementPayload(t *testing.T) {
	env := newCronHistoryEnv(t)
	ctxA := tenant.ScopedContext(context.Background(), " tenant-a ")
	ctxB := tenant.ScopedContext(context.Background(), "tenant-b")
	now := time.Now().UTC()
	for _, scope := range []struct {
		ctx context.Context
		id  string
	}{{ctxA, "cron:daily"}, {ctxB, "replacement"}} {
		if err := env.store.CreateThread(scope.ctx, &models.Thread{ThreadID: scope.id, Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	original := &models.Run{RunID: "reused", ThreadID: "cron:daily", AgentID: "agent", Status: models.RunStatusSuccess, CreatedAt: now, UpdatedAt: now}
	if err := env.store.CreateRun(ctxA, original); err != nil {
		t.Fatal(err)
	}
	selector := url.Values{"expected_tenant_id": {" tenant-a "}, "expected_incarnation_id": {original.IncarnationID}}.Encode()
	read := func(query string) *httptest.ResponseRecorder {
		out := httptest.NewRecorder()
		env.apiServer.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/admin-api/runs/reused?"+query, nil))
		return out
	}
	if out := read(selector); out.Code != http.StatusOK {
		t.Fatal("exact original reference rejected", out.Code, out.Body.String())
	}
	for _, query := range []string{"expected_tenant_id=tenant-a", "expected_incarnation_id=original", selector + "&expected_incarnation_id=duplicate"} {
		if out := read(query); out.Code != http.StatusBadRequest {
			t.Fatal("incomplete or ambiguous reference accepted", out.Code, out.Body.String())
		}
	}
	if err := env.store.DeleteRun(ctxA, original.RunID); err != nil {
		t.Fatal(err)
	}
	replacement := &models.Run{RunID: original.RunID, ThreadID: "replacement", AgentID: "foreign", Status: models.RunStatusSuccess, Metadata: map[string]interface{}{"secret": "replacement-private-output"}, CreatedAt: now, UpdatedAt: now}
	if err := env.store.CreateRun(ctxB, replacement); err != nil {
		t.Fatal(err)
	}
	if out := read(selector); out.Code != http.StatusConflict || strings.Contains(out.Body.String(), "replacement-private-output") || strings.Contains(out.Body.String(), "tenant-b") {
		t.Fatal("old reference exposed replacement", out.Code, out.Body.String())
	}
	if out := read(""); out.Code != http.StatusOK || !strings.Contains(out.Body.String(), "replacement-private-output") {
		t.Fatal("ordinary Admin read behavior changed", out.Code, out.Body.String())
	}
}
