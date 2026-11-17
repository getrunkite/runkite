package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func newCronTenantServer(t *testing.T) (*Server, state.Store, *inprocess.Queue) {
	t.Helper()
	store, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	queue := inprocess.NewQueue()
	return NewServer(store, queue, inprocess.NewBroker(), inprocess.NewCancelBus()), store, queue
}

func cronTenantThread(t *testing.T, store state.Store, ctx context.Context, id string) {
	t.Helper()
	now := time.Now().UTC()
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: id, Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
}

func cronTenantAgent(t *testing.T, store state.Store, ctx context.Context) {
	t.Helper()
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "agent", Name: "agent"}); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalCronThreadIDExactFramingAndStableVersion(t *testing.T) {
	const golden = "cron-tenant:v1:360876f9f5f7e6138d83bfc0e4ffb0ea2ced7c8b881e18f7de4815ccd74e8d21"
	if got := canonicalCronThreadID("tenant-a", "daily"); got != golden {
		t.Fatalf("canonical routing changed: %q", got)
	}
	seen := map[string]bool{}
	for _, pair := range [][2]string{
		{"a:b", "c"}, {"a", "b:c"}, {"tenant", "daily"}, {" tenant ", "daily"},
		{"tenant", " daily "}, {"租户", "每日"}, {"租", "户每日"},
		{strings.Repeat("租", 300), strings.Repeat("每日", 300)},
	} {
		id := canonicalCronThreadID(pair[0], pair[1])
		if len(id) > 255 || !strings.HasPrefix(id, "cron-tenant:v1:") || strings.HasPrefix(id, "cron:") || seen[id] {
			t.Fatalf("canonical ID aliased a pair, legacy namespace or store limit: %q", id)
		}
		seen[id] = true
	}
}

func TestCronTenantDispatchIndependentDurableConversations(t *testing.T) {
	s, store, queue := newCronTenantServer(t)
	threadIDs := map[string]bool{}
	for _, tid := range []string{tenant.DefaultTenant, "tenant-a", " tenant-a "} {
		ctx := tenant.ScopedContext(context.Background(), tid)
		cronTenantAgent(t, store, ctx)
		if err := store.UpsertCronSchedule(ctx, &models.CronSchedule{Name: "daily", AgentID: "agent", Expression: "0 9 * * *"}); err != nil {
			t.Fatal(err)
		}
		// Inherited system authority must not broaden either resolution or run creation.
		run, err := s.DispatchScheduledRun(tenant.SystemContext(ctx), "daily", "agent", json.RawMessage(`{}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		wantID := canonicalCronThreadID(tid, "daily")
		if tid == tenant.DefaultTenant {
			wantID = "cron:daily"
		}
		if run.ThreadID != wantID || threadIDs[run.ThreadID] {
			t.Fatalf("same-name schedule reused another tenant's conversation: %q", run.ThreadID)
		}
		threadIDs[run.ThreadID] = true
		assignment, err := queue.Dequeue(context.Background(), "python-langgraph", time.Second)
		if err != nil || assignment == nil || assignment.RunID != run.RunID || assignment.ThreadID != wantID || assignment.TenantID != tid {
			t.Fatal("queued assignment lost exact tenant/thread identity")
		}
		s.StatusCallback()(run.RunID, "success", "")
		if _, err := store.UpdateThread(ctx, wantID, &models.ThreadPatch{Values: map[string]interface{}{"owner": tid}}); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveCheckpoint(ctx, wantID, &models.ThreadState{
			Values: map[string]interface{}{"owner": tid}, Checkpoint: models.ThreadCheckpoint{ThreadID: wantID, CheckpointID: wantID + ":state"},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PutOpaqueCheckpoint(ctx, wantID, "checkpoint", []byte(tid), "fixture", nil); err != nil {
			t.Fatal(err)
		}
		again, err := s.DispatchScheduledRun(ctx, "daily", "agent", json.RawMessage(`{}`), nil)
		if err != nil || again.ThreadID != wantID {
			t.Fatalf("second fire did not continue the conversation: %v", err)
		}
		if _, err := queue.Dequeue(ctx, "python-langgraph", time.Second); err != nil {
			t.Fatal(err)
		}
		thread, err := store.GetThread(ctx, wantID)
		if err != nil || thread.TenantID != tid || thread.Values["owner"] != tid {
			t.Fatal("conversation values did not survive another fire")
		}
		checkpoint, err := store.GetLatestCheckpoint(ctx, wantID)
		if err != nil || checkpoint.Values["owner"] != tid {
			t.Fatal("retained checkpoint did not survive another fire")
		}
		blob, err := store.GetOpaqueCheckpoint(ctx, wantID, "checkpoint")
		if err != nil || string(blob.Data) != tid || blob.Version != 1 {
			t.Fatal("opaque checkpoint identity changed across fires")
		}
		foreign := tenant.ScopedContext(context.Background(), "foreign")
		if _, err := store.GetThread(foreign, wantID); err == nil {
			t.Fatal("foreign tenant could read a scheduled conversation")
		}
	}
	rows := cronTenantHistory(t, s)
	for _, row := range rows {
		wantID := canonicalCronThreadID(row.TenantID, "daily")
		if row.TenantID == tenant.DefaultTenant {
			wantID = "cron:daily"
		}
		if row.ThreadID != wantID || !row.ThreadAvailable || row.LatestRun == nil {
			t.Fatal("Admin did not resolve the same retained conversation as dispatch")
		}
	}
	if len(rows) != 3 {
		t.Fatal("Admin lost a same-name tenant schedule")
	}
}

func TestCronTenantFramedIdentifiersDispatchThroughSQLite(t *testing.T) {
	s, store, queue := newCronTenantServer(t)
	seen := map[string]bool{}
	for _, pair := range [][2]string{
		{"a:b", "c"}, {"a", "b:c"}, {"租户", "每日"},
		{strings.Repeat("租", 300), strings.Repeat("每日", 300)},
	} {
		ctx := tenant.ScopedContext(context.Background(), pair[0])
		cronTenantAgent(t, store, ctx)
		run, err := s.DispatchScheduledRun(ctx, pair[1], "agent", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if seen[run.ThreadID] || run.ThreadID != canonicalCronThreadID(pair[0], pair[1]) || len(run.ThreadID) > 255 {
			t.Fatal("framed identity did not retain a distinct bounded thread")
		}
		seen[run.ThreadID] = true
		assignment, err := queue.Dequeue(ctx, "python-langgraph", time.Second)
		if err != nil || assignment == nil || assignment.TenantID != pair[0] || assignment.ThreadID != run.ThreadID {
			t.Fatal("framed identity changed in the queued assignment")
		}
		retained, err := store.GetRun(ctx, run.RunID)
		if err != nil || retained.ThreadID != run.ThreadID || retained.TenantID != pair[0] {
			t.Fatal("framed identity changed in durable run state")
		}
	}
}

type cronTenantHistoryRow struct {
	TenantID string `json:"tenant_id"`
	adminCronHistoryView
}

func cronTenantHistory(t *testing.T, s *Server) []cronTenantHistoryRow {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin-api/cron?include_history=true", nil)
	s.handleAdminListCronSchedules(w, r.WithContext(tenant.SystemContext(r.Context())))
	var rows []cronTenantHistoryRow
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rows) != nil {
		t.Fatal("Admin history failed")
	}
	return rows
}

func TestCronTenantOwnedLegacyContinuityAndCanonicalPrecedence(t *testing.T) {
	for _, canonicalPresent := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "canonical"}[canonicalPresent], func(t *testing.T) {
			s, store, queue := newCronTenantServer(t)
			ctx := tenant.ScopedContext(context.Background(), "tenant-a")
			cronTenantAgent(t, store, ctx)
			cronTenantThread(t, store, ctx, "cron:daily")
			wantID := "cron:daily"
			if canonicalPresent {
				wantID = canonicalCronThreadID("tenant-a", "daily")
				cronTenantThread(t, store, ctx, wantID)
			}
			if _, err := store.PutOpaqueCheckpoint(ctx, wantID, "checkpoint", []byte("retained"), "fixture", nil); err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertCronSchedule(ctx, &models.CronSchedule{Name: "daily", AgentID: "agent", Expression: "0 9 * * *"}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				run, err := s.DispatchScheduledRun(ctx, "daily", "agent", nil, nil)
				if err != nil || run.ThreadID != wantID {
					t.Fatalf("selected a different retained conversation: %v", err)
				}
				if _, err := queue.Dequeue(ctx, "python-langgraph", time.Second); err != nil {
					t.Fatal(err)
				}
				s.StatusCallback()(run.RunID, "success", "")
			}
			blob, err := store.GetOpaqueCheckpoint(ctx, wantID, "checkpoint")
			if err != nil || string(blob.Data) != "retained" || blob.Version != 1 {
				t.Fatal("existing checkpoint conversation was replaced")
			}
			rows := cronTenantHistory(t, s)
			if len(rows) != 1 || rows[0].ThreadID != wantID || !rows[0].ThreadAvailable || rows[0].LatestRun == nil {
				t.Fatal("Admin did not preserve dispatch's selected conversation")
			}
			if !canonicalPresent {
				if _, err := store.GetThread(ctx, canonicalCronThreadID("tenant-a", "daily")); err == nil {
					t.Fatal("owned legacy history was silently migrated")
				}
			}
		})
	}
}

func TestCronTenantBusyCanonicalNeverFallsBackToIdleLegacy(t *testing.T) {
	s, store, _ := newCronTenantServer(t)
	ctx := tenant.ScopedContext(context.Background(), "tenant-a")
	cronTenantAgent(t, store, ctx)
	cronTenantThread(t, store, ctx, "cron:daily")
	canonicalID := canonicalCronThreadID("tenant-a", "daily")
	cronTenantThread(t, store, ctx, canonicalID)
	if _, err := s.DispatchScheduledRun(ctx, "daily", "agent", nil, nil); err != nil {
		t.Fatal(err)
	}
	_, err := s.DispatchScheduledRun(ctx, "daily", "agent", nil, nil)
	var conflict *state.ErrConflict
	if !errors.As(err, &conflict) {
		t.Fatal("busy canonical thread did not reject overlap")
	}
	legacy, err := store.GetThread(ctx, "cron:daily")
	if err != nil || legacy.Status != models.ThreadStatusIdle {
		t.Fatal("overlap fell back to the idle legacy conversation")
	}
	runs, err := store.SearchRuns(ctx, &models.RunSearchRequest{Limit: 10})
	if err != nil || len(runs) != 1 || runs[0].ThreadID != canonicalID {
		t.Fatal("overlap created a second scheduled run")
	}
}

func TestCronTenantLaterLegacyCannotRedirectCanonicalConversation(t *testing.T) {
	s, store, queue := newCronTenantServer(t)
	ctx := tenant.ScopedContext(context.Background(), "tenant-a")
	cronTenantAgent(t, store, ctx)
	first, err := s.DispatchScheduledRun(ctx, "daily", "agent", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Dequeue(ctx, "python-langgraph", time.Second); err != nil {
		t.Fatal(err)
	}
	s.StatusCallback()(first.RunID, "success", "")
	cronTenantThread(t, store, ctx, "cron:daily")
	next, err := s.DispatchScheduledRun(ctx, "daily", "agent", nil, nil)
	if err != nil || next.ThreadID != first.ThreadID || next.ThreadID != canonicalCronThreadID("tenant-a", "daily") {
		t.Fatal("later legacy row redirected an established canonical conversation")
	}
}

func TestCronTenantForeignCanonicalConflictDoesNotAdoptOrFallback(t *testing.T) {
	s, store, _ := newCronTenantServer(t)
	ctx := tenant.ScopedContext(context.Background(), "tenant-a")
	foreign := tenant.ScopedContext(context.Background(), "foreign")
	cronTenantAgent(t, store, ctx)
	id := canonicalCronThreadID("tenant-a", "daily")
	cronTenantThread(t, store, foreign, id)
	if _, err := s.DispatchScheduledRun(tenant.SystemContext(ctx), "daily", "agent", nil, nil); err == nil {
		t.Fatal("foreign canonical thread was adopted")
	}
	for _, candidate := range []string{id, "cron:daily"} {
		if _, err := store.GetThread(ctx, candidate); err == nil {
			t.Fatal("canonical creation conflict fell back or changed ownership")
		}
	}
	thread, err := store.GetThread(foreign, id)
	if err != nil || thread.Status != models.ThreadStatusIdle {
		t.Fatal("failed dispatch mutated a foreign thread")
	}
	runs, err := store.SearchRuns(ctx, &models.RunSearchRequest{Limit: 10})
	if err != nil || len(runs) != 0 {
		t.Fatal("failed canonical creation persisted a run")
	}
}

type cronThreadLookupFailureStore struct {
	state.Store
	failID               string
	err                  error
	reads                []string
	writes, historyReads int
}

func (s *cronThreadLookupFailureStore) GetThread(ctx context.Context, id string) (*models.Thread, error) {
	if tenant.IsSystem(ctx) || tenant.FromContext(ctx) != "tenant-a" {
		return nil, errors.New("incorrect fixture scope")
	}
	s.reads = append(s.reads, id)
	if id == s.failID {
		return nil, s.err
	}
	return s.Store.GetThread(ctx, id)
}

func (s *cronThreadLookupFailureStore) CreateThread(ctx context.Context, thread *models.Thread) error {
	s.writes++
	return s.Store.CreateThread(ctx, thread)
}

func (s *cronThreadLookupFailureStore) SearchRuns(ctx context.Context, req *models.RunSearchRequest) ([]*models.Run, error) {
	s.historyReads++
	return s.Store.SearchRuns(ctx, req)
}

func TestCronTenantLookupErrorsFailClosedBeforeDispatchAndHistory(t *testing.T) {
	for _, failedID := range []string{canonicalCronThreadID("tenant-a", "daily"), "cron:daily"} {
		t.Run(failedID, func(t *testing.T) {
			s, store, _ := newCronTenantServer(t)
			ctx := tenant.ScopedContext(context.Background(), "tenant-a")
			cronTenantAgent(t, store, ctx)
			if err := store.UpsertCronSchedule(ctx, &models.CronSchedule{Name: "daily", AgentID: "agent", Expression: "0 9 * * *"}); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("synthetic-private-backend-failure")
			wrapped := &cronThreadLookupFailureStore{Store: store, failID: failedID, err: failure}
			s.store = wrapped
			if _, err := s.DispatchScheduledRun(tenant.SystemContext(ctx), "daily", "agent", nil, nil); !errors.Is(err, failure) {
				t.Fatal("thread lookup failure was treated as absence")
			}
			rows := cronTenantHistory(t, s)
			if len(rows) != 1 || rows[0].HistoryStatus != "unavailable" || rows[0].ThreadAvailable || rows[0].LatestRun != nil || wrapped.writes != 0 || wrapped.historyReads != 0 {
				t.Fatal("failed resolution created a thread or queried unrelated history")
			}
			if failedID != "cron:daily" {
				for _, id := range wrapped.reads {
					if id != failedID {
						t.Fatal("canonical lookup error fell back to legacy")
					}
				}
			}
		})
	}
}
