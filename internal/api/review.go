package api

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// reviewContext is a second authorization boundary for handlers. Neither query
// filters nor a previously inherited SystemContext can widen this scope.
func reviewContext(w http.ResponseWriter, r *http.Request, approve bool) (context.Context, auth.ReviewCapabilities, bool) {
	result := auth.FromContext(r.Context())
	caps, ok := auth.ReviewCapabilitiesFromResult(result)
	if !ok || (approve && !caps.CanApprove) {
		writeError(w, http.StatusForbidden, "insufficient scoped review permissions")
		return nil, caps, false
	}
	for _, selected := range r.URL.Query()["tenant_id"] {
		if selected != "" && selected != result.TenantID {
			writeError(w, http.StatusForbidden, "review tenant is fixed by authenticated identity")
			return nil, caps, false
		}
	}
	return tenant.ScopedContext(r.Context(), result.TenantID), caps, true
}

func reviewListPaging(r *http.Request) (adminPaging, error) {
	p := adminPaging{Limit: 50}
	q := r.URL.Query()
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 {
			return p, &pendingDecisionError{http.StatusBadRequest, "invalid limit"}
		}
		p.Limit = min(n, 100)
	}
	if q.Has("offset") {
		n, err := strconv.Atoi(q.Get("offset"))
		if err != nil || n < 0 {
			return p, &pendingDecisionError{http.StatusBadRequest, "invalid offset"}
		}
		p.Offset = n
	}
	p.Cursor = q.Get("cursor")
	if len(p.Cursor) > 4096 {
		return p, &pendingDecisionError{http.StatusBadRequest, "invalid cursor"}
	}
	if p.Cursor != "" && q.Has("offset") {
		return p, &pendingDecisionError{http.StatusBadRequest, "cursor and offset cannot be combined"}
	}
	if _, err := pagecursor.DecodeTime(p.Cursor); err != nil {
		return p, &pendingDecisionError{http.StatusBadRequest, "invalid cursor"}
	}
	return p, nil
}

// reviewPendingAction is an allowlist. New storage fields are not automatically
// exposed to reviewers, even when the model later gains sensitive metadata.
type reviewPendingAction struct {
	ID               string         `json:"id"`
	RunID            string         `json:"run_id"`
	RunIncarnationID string         `json:"run_incarnation_id,omitempty"`
	Generation       int64          `json:"generation"`
	TenantID         string         `json:"tenant_id"`
	AgentID          string         `json:"agent_id"`
	Connector        string         `json:"connector"`
	Tool             string         `json:"tool"`
	RuleID           string         `json:"rule_id,omitempty"`
	ReasonCode       string         `json:"reason_code,omitempty"`
	Status           string         `json:"status"`
	ArgsDigest       string         `json:"args_digest,omitempty"`
	Args             map[string]any `json:"args,omitempty"`
	DecidedBy        string         `json:"decided_by,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	ExpiresAt        *time.Time     `json:"expires_at,omitempty"`
}

func reviewPendingProjection(action *models.PendingAction, caps auth.ReviewCapabilities) reviewPendingAction {
	out := reviewPendingAction{
		ID: action.ID, RunID: action.RunID, RunIncarnationID: action.RunIncarnationID,
		Generation: action.Generation, TenantID: action.TenantID, AgentID: action.AgentID,
		Connector: action.Connector, Tool: action.Tool, RuleID: action.RuleID, ReasonCode: action.ReasonCode,
		Status: action.Status, ArgsDigest: action.ArgsDigest, DecidedBy: action.DecidedBy,
		CreatedAt: action.CreatedAt, UpdatedAt: action.UpdatedAt, ExpiresAt: action.ExpiresAt,
	}
	if caps.CanApprove {
		out.Args = action.Args
	}
	return out
}

func (s *Server) handleReviewListPendingActions(w http.ResponseWriter, r *http.Request) {
	ctx, caps, ok := reviewContext(w, r, false)
	if !ok {
		return
	}
	store, ok := s.pendingActions()
	if !ok {
		writeError(w, http.StatusNotImplemented, "pending actions require a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	paging, err := reviewListPaging(r)
	if err != nil {
		writePendingDecisionError(w, err)
		return
	}
	q := r.URL.Query()
	status := q.Get("status")
	switch status {
	case "", models.PendingStatusPending, models.PendingStatusApproved, models.PendingStatusDenied, models.PendingStatusConsumed:
	default:
		writeError(w, http.StatusBadRequest, "invalid pending action status")
		return
	}
	rows, err := store.SearchPendingActions(ctx, &models.PendingActionSearchRequest{Status: status, RunID: q.Get("run_id"), Connector: q.Get("connector"), Limit: paging.Limit + 1, Offset: paging.Offset, Cursor: paging.Cursor})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	next := ""
	if len(rows) > paging.Limit {
		rows = rows[:paging.Limit]
		last := rows[len(rows)-1]
		next = pagecursor.EncodeTime(last.CreatedAt, last.ID)
	}
	out := make([]reviewPendingAction, 0, len(rows))
	for _, action := range rows {
		if action.TenantID != tenant.FromContext(ctx) {
			writeError(w, http.StatusInternalServerError, "invalid review scope")
			return
		}
		out = append(out, reviewPendingProjection(action, caps))
	}
	writeAdminListJSON(w, http.StatusOK, out, next)
}

func (s *Server) handleReviewGetPendingAction(w http.ResponseWriter, r *http.Request) {
	ctx, caps, ok := reviewContext(w, r, false)
	if !ok {
		return
	}
	store, ok := s.pendingActions()
	if !ok {
		writeError(w, http.StatusNotImplemented, "pending actions require a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	a, err := store.GetPendingAction(ctx, r.PathValue("id"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if a.TenantID != tenant.FromContext(ctx) {
		writeError(w, http.StatusNotFound, "pending action not found")
		return
	}
	writeJSON(w, http.StatusOK, reviewPendingProjection(a, caps))
}

// Decisions accept no client-selected authority, identity, or tenant fields.
func reviewEmptyDecisionBody(w http.ResponseWriter, r *http.Request) bool {
	if r.Body == nil {
		return true
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid review decision body")
		return false
	}
	value := strings.TrimSpace(string(body))
	if value != "" && value != "{}" {
		writeError(w, http.StatusBadRequest, "review decisions accept no body fields")
		return false
	}
	return true
}

func (s *Server) handleReviewApprovePendingAction(w http.ResponseWriter, r *http.Request) {
	ctx, caps, ok := reviewContext(w, r, true)
	if !ok || !reviewEmptyDecisionBody(w, r) {
		return
	}
	a, err := s.approvePendingAction(ctx, r.PathValue("id"), pendingActor(r))
	if err != nil {
		writePendingDecisionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reviewPendingProjection(a, caps))
}

func (s *Server) handleReviewDenyPendingAction(w http.ResponseWriter, r *http.Request) {
	ctx, caps, ok := reviewContext(w, r, true)
	if !ok || !reviewEmptyDecisionBody(w, r) {
		return
	}
	a, err := s.denyPendingAction(ctx, r.PathValue("id"))
	if err != nil {
		writePendingDecisionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reviewPendingProjection(a, caps))
}

func (s *Server) handleReviewListConnectorActions(w http.ResponseWriter, r *http.Request) {
	ctx, _, ok := reviewContext(w, r, false)
	if !ok {
		return
	}
	store, ok := s.store.(state.ProtectedActionStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "connector action history requires Postgres, MySQL or SQLite")
		return
	}
	paging, err := reviewListPaging(r)
	if err != nil {
		writePendingDecisionError(w, err)
		return
	}
	q := r.URL.Query()
	status := q.Get("state")
	if !validConnectorActionState(status) {
		writeError(w, http.StatusBadRequest, "invalid connector action state")
		return
	}
	rows, err := store.SearchConnectorActions(ctx, &models.ConnectorActionSearchRequest{RunID: q.Get("run_id"), AgentID: q.Get("agent_id"), Connector: q.Get("connector"), State: status, Limit: paging.Limit + 1, Offset: paging.Offset, Cursor: paging.Cursor})
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
	for _, action := range rows {
		if action.TenantID != tenant.FromContext(ctx) {
			writeError(w, http.StatusInternalServerError, "invalid review scope")
			return
		}
	}
	w.Header().Set("X-Runkite-Action-Evidence", strconv.FormatBool(s.policy.ActionEvidenceEnabled()))
	writeAdminListJSON(w, http.StatusOK, rows, next)
}

type reviewConnectorActionDetail struct {
	Action          *models.ConnectorAction        `json:"action"`
	Events          []*models.ConnectorActionEvent `json:"events"`
	EventsTruncated bool                           `json:"events_truncated,omitempty"`
}

func (s *Server) handleReviewGetConnectorAction(w http.ResponseWriter, r *http.Request) {
	ctx, _, ok := reviewContext(w, r, false)
	if !ok {
		return
	}
	store, ok := s.store.(state.ProtectedActionStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "connector action history requires Postgres, MySQL or SQLite")
		return
	}
	action, err := store.GetConnectorAction(ctx, r.PathValue("actionID"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if action.TenantID != tenant.FromContext(ctx) {
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
	for _, event := range events {
		if event.TenantID != tenant.FromContext(ctx) {
			writeError(w, http.StatusInternalServerError, "invalid review scope")
			return
		}
	}
	writeJSON(w, http.StatusOK, reviewConnectorActionDetail{Action: action, Events: events, EventsTruncated: len(events) == 200 && action.Version > int64(len(events))})
}
