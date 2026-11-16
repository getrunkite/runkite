package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
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

type adminCronScheduleView struct {
	*models.CronSchedule
	ThreadID        string            `json:"thread_id"`
	ThreadAvailable bool              `json:"thread_available"`
	HistoryStatus   string            `json:"history_status"`
	LatestRun       *adminCronRunView `json:"latest_run,omitempty"`
}

// GET /admin-api/cron?include_history=true. The schedule list is global Admin data, but every
// history lookup uses its schedule's tenant. A deterministic cron thread ID
// alone must never select another tenant's retained history.
func (s *Server) handleAdminListCronSchedules(w http.ResponseWriter, r *http.Request) {
	schedules, err := s.store.ListCronSchedules(tenant.SystemContext(r.Context()))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	// Existing consumers keep the definition-only shape and read cost.
	if r.URL.Query().Get("include_history") != "true" {
		writeJSON(w, http.StatusOK, schedules)
		return
	}
	views := make([]adminCronScheduleView, 0, len(schedules))
	for _, schedule := range schedules {
		tenantID := schedule.TenantID
		if tenantID == "" {
			tenantID = tenant.DefaultTenant
		}
		ctx := tenant.ScopedContext(r.Context(), tenantID)
		view := adminCronScheduleView{
			CronSchedule: schedule, ThreadID: "cron:" + schedule.Name, HistoryStatus: "available",
		}
		_, threadErr := s.store.GetThread(ctx, view.ThreadID)
		view.ThreadAvailable = threadErr == nil
		var missing *state.ErrNotFound
		if threadErr != nil && !errors.As(threadErr, &missing) {
			view.HistoryStatus = "unavailable"
			slog.Warn("cron history: thread lookup failed", "tenant", schedule.TenantID, "name", schedule.Name, "error", threadErr)
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
