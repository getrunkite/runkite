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

	"github.com/getrunkite/runkite/internal/cronschedule"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

type cronPreviewHistoryRead struct {
	tenant, thread, method string
	system                 bool
}
type cronPreviewStore struct {
	state.Store
	schedules           []*models.CronSchedule
	listErr, historyErr error
	listReads           int
	reads               []cronPreviewHistoryRead
}

func (s *cronPreviewStore) ListCronSchedules(ctx context.Context) ([]*models.CronSchedule, error) {
	s.listReads++
	if !tenant.IsSystem(ctx) {
		return nil, errors.New("schedule list was not global Admin scope")
	}
	return s.schedules, s.listErr
}
func (s *cronPreviewStore) GetThread(ctx context.Context, id string) (*models.Thread, error) {
	s.reads = append(s.reads, cronPreviewHistoryRead{tenant: tenant.FromContext(ctx), thread: id, method: "thread", system: tenant.IsSystem(ctx)})
	if s.historyErr != nil {
		return nil, s.historyErr
	}
	return nil, &state.ErrNotFound{Resource: "thread", ID: id}
}
func (s *cronPreviewStore) SearchRuns(ctx context.Context, req *models.RunSearchRequest) ([]*models.Run, error) {
	s.reads = append(s.reads, cronPreviewHistoryRead{tenant: tenant.FromContext(ctx), thread: req.ThreadID, method: "runs", system: tenant.IsSystem(ctx)})
	if req.Limit != 1 {
		return nil, errors.New("cron history did not retain its narrow projection")
	}
	return nil, s.historyErr
}

func fixedCronPreviewTime() time.Time {
	return time.Date(2026, 11, 17, 15, 30, 0, 123456789, time.FixedZone("fixture-offset", 19800))
}
func previewRows(t *testing.T, s *Server, query string, now time.Time) []map[string]json.RawMessage {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin-api/cron"+query, nil)
	// Even an inherited system context must be masked for individual history.
	r = r.WithContext(tenant.SystemContext(tenant.WithContext(r.Context(), "unrelated-caller")))
	s.handleAdminListCronSchedulesAt(w, r, now)
	if w.Code != http.StatusOK {
		t.Fatalf("cron list status=%d", w.Code)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestAdminCronPreviewFlagsPreserveIndependentShapesAndHistoryCost(t *testing.T) {
	for _, tc := range []struct {
		name, query      string
		history, preview bool
	}{
		{"definitions", "", false, false},
		{"history", "?include_history=true", true, false},
		{"preview", "?include_preview=true", false, true},
		{"both", "?include_history=true&include_preview=true", true, true},
		{"literal_preview_only", "?include_history=TRUE&include_preview=true", false, true},
		{"literal_history_only", "?include_history=true&include_preview=TRUE", true, false},
		{"numeric_not_opt_in", "?include_preview=1", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &cronPreviewStore{schedules: []*models.CronSchedule{
				{Name: "same", TenantID: " tenant-a ", Expression: "0 * * * *", Timezone: "UTC", Enabled: true},
				{Name: "same", TenantID: "", Expression: "0 * * * *", Timezone: "UTC", Enabled: true},
			}}
			rows := previewRows(t, NewServer(store, nil, nil, nil), tc.query, fixedCronPreviewTime())
			if len(rows) != 2 || store.listReads != 1 {
				t.Fatal("preview changed schedule-list read count")
			}
			for _, row := range rows {
				_, hasPreview := row["next_fire_preview"]
				if hasPreview != tc.preview {
					t.Fatal("calendar preview did not follow its literal independent opt-in")
				}
				for _, field := range []string{"thread_id", "thread_available", "history_status"} {
					_, present := row[field]
					if present != tc.history {
						t.Fatalf("history field %s was fabricated or omitted", field)
					}
				}
				if _, present := row["latest_run"]; present {
					t.Fatal("empty retained history fabricated a run")
				}
			}
			wantReads := 0
			if tc.history {
				wantReads = 4
			}
			if len(store.reads) != wantReads {
				t.Fatalf("history reads=%d want=%d", len(store.reads), wantReads)
			}
			for index, read := range store.reads {
				wantTenant := []string{" tenant-a ", " tenant-a ", "default", "default"}[index]
				if read.system || read.tenant != wantTenant || read.thread != "cron:same" || read.method != []string{"thread", "runs"}[index%2] {
					t.Fatal("preview changed exact per-schedule history scope")
				}
			}
		})
	}
}

func TestAdminCronPreviewUsesOneExactUTCClockAcrossRows(t *testing.T) {
	store := &cronPreviewStore{schedules: []*models.CronSchedule{
		{Name: "utc", Expression: "0 * * * *", Timezone: "UTC", Enabled: true},
		{Name: "override", Expression: "CRON_TZ=Asia/Kolkata 0 17 * * *", Timezone: "invalid/configured-zone", Enabled: true},
	}}
	now := fixedCronPreviewTime()
	rows := previewRows(t, NewServer(store, nil, nil, nil), "?include_preview=true", now)
	for index, row := range rows {
		var preview cronschedule.PreviewResult
		if json.Unmarshal(row["next_fire_preview"], &preview) != nil || !preview.ComputedAt.Equal(now) || preview.ComputedAt.Location() != time.UTC {
			t.Fatal("preview did not retain the request-wide UTC computation instant")
		}
		wantNext := []string{"2026-11-17T11:00:00Z", "2026-11-17T11:30:00Z"}[index]
		if preview.Status != cronschedule.StatusScheduled || preview.NextFireAt == nil || preview.NextFireAt.Format(time.RFC3339) != wantNext || preview.NextFireAt.Location() != time.UTC {
			t.Fatalf("unexpected projected calendar boundary for row %d", index)
		}
		if preview.TimezoneFallback {
			t.Fatal("an explicit valid expression zone inherited a configured-zone fallback warning")
		}
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(row["next_fire_preview"], &raw)
		if _, present := raw["timezone_fallback"]; present {
			t.Fatal("false fallback field was not omitted")
		}
	}
	if string(rows[0]["next_fire_preview"]) == string(rows[1]["next_fire_preview"]) {
		t.Fatal("separate calendar definitions shared a preview result")
	}
}

func TestAdminCronPreviewStatusesAndOptionalFields(t *testing.T) {
	for _, tc := range []struct {
		name, expression, zone string
		enabled                bool
		status                 cronschedule.PreviewStatus
		timezone               string
		fallback               bool
	}{
		{"disabled", "invalid", "invalid/zone", false, cronschedule.StatusDisabled, "", false},
		{"malformed_prefix", "TZ=UTC", "UTC", true, cronschedule.StatusInvalidExpression, "", false},
		{"bad_expression_zone", "TZ=missing/zone 0 * * * *", "UTC", true, cronschedule.StatusInvalidExpression, "", false},
		{"impossible_date", "0 0 30 2 *", "UTC", true, cronschedule.StatusNoFutureFire, "UTC", false},
		{"relative", "@every 1h", "UTC", true, cronschedule.StatusRelativeIntervalUnavailable, "", false},
		{"zone_fallback", "0 * * * *", "missing/configured-zone", true, cronschedule.StatusScheduled, "UTC", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &cronPreviewStore{schedules: []*models.CronSchedule{{Name: tc.name, Expression: tc.expression, Timezone: tc.zone, Enabled: tc.enabled}}}
			start := time.Now()
			rows := previewRows(t, NewServer(store, nil, nil, nil), "?include_preview=true", fixedCronPreviewTime())
			if time.Since(start) > time.Second {
				t.Fatal("preview of an impossible or malformed calendar did not remain bounded")
			}
			var preview cronschedule.PreviewResult
			if json.Unmarshal(rows[0]["next_fire_preview"], &preview) != nil || preview.Status != tc.status || preview.EffectiveTimezone != tc.timezone || preview.TimezoneFallback != tc.fallback {
				t.Fatal("preview state or effective timezone differs from scheduler semantics")
			}
			var raw map[string]json.RawMessage
			_ = json.Unmarshal(rows[0]["next_fire_preview"], &raw)
			_, hasNext := raw["next_fire_at"]
			_, hasZone := raw["effective_timezone"]
			_, hasFallback := raw["timezone_fallback"]
			if hasNext != (tc.status == cronschedule.StatusScheduled) || hasZone != (tc.timezone != "") || hasFallback != tc.fallback {
				t.Fatal("preview fabricated optional timestamp/timezone fields")
			}
			if len(store.reads) != 0 {
				t.Fatal("preview acquired retained-history reads")
			}
		})
	}
}

func TestAdminCronPreviewDoesNotTurnHistoryFailureIntoEmptyHistory(t *testing.T) {
	store := &cronPreviewStore{schedules: []*models.CronSchedule{{Name: "fixture", Expression: "0 * * * *", Enabled: true}}, historyErr: errors.New("synthetic-private-storage-error")}
	rows := previewRows(t, NewServer(store, nil, nil, nil), "?include_history=true&include_preview=true", fixedCronPreviewTime())
	var status string
	_ = json.Unmarshal(rows[0]["history_status"], &status)
	if status != "unavailable" || rows[0]["next_fire_preview"] == nil || len(store.reads) != 2 {
		t.Fatal("preview masked history failure or changed history reads")
	}
	encoded, _ := json.Marshal(rows)
	if strings.Contains(string(encoded), "synthetic-private") {
		t.Fatal("preview response exposed storage diagnostics")
	}
}
