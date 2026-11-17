package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/getrunkite/runkite/internal/cronschedule"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

// Only retained run metadata belongs in the schedule list. Output and errors
// remain on the existing run detail surface.
type adminCronRunView struct {
	RunID         string           `json:"run_id"`
	IncarnationID string           `json:"incarnation_id,omitempty"`
	AgentID       string           `json:"agent_id"`
	Status        models.RunStatus `json:"status"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

type adminCronHistoryView struct {
	ThreadID        string            `json:"thread_id"`
	ThreadAvailable bool              `json:"thread_available"`
	HistoryStatus   string            `json:"history_status"`
	LatestRun       *adminCronRunView `json:"latest_run,omitempty"`
}

type adminCronScheduleView struct {
	*models.CronSchedule
	*adminCronHistoryView
	NextFirePreview *cronschedule.PreviewResult `json:"next_fire_preview,omitempty"`
}

// GET /admin-api/cron. History and calendar preview are independent opt-ins.
// The list is global Admin data; every history read uses its schedule's tenant.
func (s *Server) handleAdminListCronSchedules(w http.ResponseWriter, r *http.Request) {
	s.handleAdminListCronSchedulesAt(w, r, time.Now().UTC())
}

func (s *Server) handleAdminListCronSchedulesAt(w http.ResponseWriter, r *http.Request, now time.Time) {
	schedules, err := s.store.ListCronSchedules(tenant.SystemContext(r.Context()))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	includeHistory := r.URL.Query().Get("include_history") == "true"
	includePreview := r.URL.Query().Get("include_preview") == "true"
	// Existing definition-only consumers keep their shape and read cost.
	if !includeHistory && !includePreview {
		writeJSON(w, http.StatusOK, schedules)
		return
	}
	now = now.UTC()
	views := make([]adminCronScheduleView, 0, len(schedules))
	for _, schedule := range schedules {
		view := adminCronScheduleView{CronSchedule: schedule}
		if includePreview {
			preview := cronschedule.Preview(schedule.Expression, schedule.Timezone, schedule.Enabled, now)
			view.NextFirePreview = &preview
		}
		if includeHistory {
			tenantID := schedule.TenantID
			if tenantID == "" {
				tenantID = tenant.DefaultTenant
			}
			ctx := tenant.ScopedContext(r.Context(), tenantID)
			threadID, thread, threadErr := s.resolveCronThread(ctx, schedule.Name)
			view.adminCronHistoryView = &adminCronHistoryView{ThreadID: threadID, ThreadAvailable: thread != nil, HistoryStatus: "available"}
			if threadErr != nil {
				view.HistoryStatus = "unavailable"
				slog.Warn("cron history: thread lookup failed", "tenant", schedule.TenantID, "name", schedule.Name, "error", threadErr)
				views = append(views, view)
				continue
			}
			runs, runErr := s.store.SearchRuns(ctx, &models.RunSearchRequest{ThreadID: view.ThreadID, Limit: 1})
			if runErr != nil {
				view.HistoryStatus = "unavailable"
				slog.Warn("cron history: run lookup failed", "tenant", schedule.TenantID, "name", schedule.Name, "error", runErr)
			} else if len(runs) > 0 {
				run := runs[0]
				view.LatestRun = &adminCronRunView{
					RunID: run.RunID, IncarnationID: run.IncarnationID, AgentID: run.AgentID, Status: run.Status,
					CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
				}
			} else if view.HistoryStatus != "unavailable" {
				view.HistoryStatus = "empty"
			}
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, views)
}

// Explicit tenant selection narrows existing global Admin thread operations.
// Omitting it preserves their cross-tenant operator behavior.
func adminScopedThreadContext(r *http.Request) context.Context {
	if _, selected := r.URL.Query()["tenant_id"]; selected {
		return tenant.ScopedContext(r.Context(), r.URL.Query().Get("tenant_id"))
	}
	return tenant.SystemContext(r.Context())
}

func (s *Server) handleAdminDeleteThread(w http.ResponseWriter, r *http.Request) {
	s.handleDeleteThread(w, r.WithContext(adminScopedThreadContext(r)))
}
