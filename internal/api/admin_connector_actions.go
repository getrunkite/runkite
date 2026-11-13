package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// Admin remains global. Query filters select records; they do not create
// tenant-scoped reviewer permissions. Middleware requires Admin authorization.
func (s *Server) handleAdminListConnectorActions(w http.ResponseWriter, r *http.Request) {
	store, ok := s.store.(state.ProtectedActionStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "connector action history requires Postgres, MySQL or SQLite")
		return
	}
	paging, err := adminListPaging(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(paging.Cursor) > 4096 {
		writeError(w, http.StatusBadRequest, "invalid action cursor")
		return
	}
	if _, err := pagecursor.DecodeTime(paging.Cursor); err != nil {
		writeError(w, http.StatusBadRequest, "invalid action cursor")
		return
	}
	q := r.URL.Query()
	status := q.Get("state")
	if !validConnectorActionState(status) {
		writeError(w, http.StatusBadRequest, "invalid connector action state")
		return
	}
	rows, err := store.SearchConnectorActions(tenant.SystemContext(r.Context()), &models.ConnectorActionSearchRequest{
		TenantID: q.Get("tenant_id"), RunID: q.Get("run_id"), AgentID: q.Get("agent_id"),
		Connector: q.Get("connector"), State: status,
		Limit: paging.Limit + 1, Offset: paging.Offset, Cursor: paging.Cursor,
	})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if rows == nil {
		rows = []*models.ConnectorAction{}
	}
	next := ""
	if len(rows) > paging.Limit {
		rows = rows[:paging.Limit]
		last := rows[len(rows)-1]
		next = pagecursor.EncodeTime(last.CreatedAt, last.ID)
	}
	w.Header().Set("X-Runkite-Action-Evidence", strconv.FormatBool(s.policy.ActionEvidenceEnabled()))
	writeAdminListJSON(w, http.StatusOK, rows, next)
}

type adminConnectorActionDetail struct {
	Action          *models.ConnectorAction        `json:"action"`
	Events          []*models.ConnectorActionEvent `json:"events"`
	RunAvailable    bool                           `json:"run_available"`
	EventsTruncated bool                           `json:"events_truncated,omitempty"`
}

func (s *Server) handleAdminGetConnectorAction(w http.ResponseWriter, r *http.Request) {
	store, ok := s.store.(state.ProtectedActionStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "connector action history requires Postgres, MySQL or SQLite")
		return
	}
	ctx := tenant.SystemContext(r.Context())
	action, err := store.GetConnectorAction(ctx, r.PathValue("actionID"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if selected := r.URL.Query().Get("tenant_id"); selected != "" && selected != action.TenantID {
		writeError(w, http.StatusNotFound, "connector action not found")
		return
	}
	if selected := r.URL.Query().Get("run_id"); selected != "" && selected != action.RunID {
		writeError(w, http.StatusNotFound, "connector action not found")
		return
	}
	events, err := store.ListConnectorActionEvents(ctx, action.ID, 0, 200)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if events == nil {
		events = []*models.ConnectorActionEvent{}
	}
	// An old run ID can be reused. Link only to the exact persisted run
	// incarnation observed when the action was created, never to a new owner.
	runAvailable := false
	run, err := s.store.GetRun(ctx, action.RunID)
	if err == nil {
		runAvailable = action.RunIncarnationID != "" && run.IncarnationID == action.RunIncarnationID &&
			run.TenantID == action.TenantID && run.AgentID == action.AgentID &&
			run.ThreadID == action.ThreadID && run.CreatedAt.Equal(action.RunCreatedAt)
	} else {
		var missing *state.ErrNotFound
		if !errors.As(err, &missing) {
			handleStoreError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, adminConnectorActionDetail{
		Action: action, Events: events, RunAvailable: runAvailable,
		EventsTruncated: len(events) == 200 && action.Version > int64(len(events)),
	})
}

func validConnectorActionState(value string) bool {
	switch value {
	case "", models.ConnectorActionReceived, models.ConnectorActionBlocked,
		models.ConnectorActionApprovalRequired, models.ConnectorActionReady,
		models.ConnectorActionNotDispatched, models.ConnectorActionDispatchIntent,
		models.ConnectorActionResponseObserved, models.ConnectorActionOutcomeUnknown:
		return true
	default:
		return false
	}
}
