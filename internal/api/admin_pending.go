package api

import (
	"context"
	"net/http"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/tenant"
)

// pendingActionStore is the Postgres-only Admin + one-shot capability surface.
type pendingActionStore interface {
	CreatePendingAction(ctx context.Context, a *models.PendingAction) error
	GetPendingAction(ctx context.Context, id string) (*models.PendingAction, error)
	SearchPendingActions(ctx context.Context, req *models.PendingActionSearchRequest) ([]*models.PendingAction, error)
	SetPendingActionStatus(ctx context.Context, id, fromStatus, toStatus string) error
	FindOpenPendingAction(ctx context.Context, runID string, generation int64, connector, tool string) (*models.PendingAction, error)
	ConsumeApprovedAction(ctx context.Context, runID string, generation int64, connector, tool string) (string, error)
}

func (s *Server) pendingActions() (pendingActionStore, bool) {
	pg, ok := s.store.(pendingActionStore)
	return pg, ok
}

// GET /admin-api/pending-actions
func (s *Server) handleAdminListPendingActions(w http.ResponseWriter, r *http.Request) {
	store, ok := s.pendingActions()
	if !ok {
		writeError(w, http.StatusNotImplemented, "pending actions require Postgres state backend (Supported profile)")
		return
	}
	ctx := tenant.SystemContext(r.Context())
	paging, err := adminListPaging(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if paging.Cursor != "" {
		if _, err := pagecursor.DecodeTime(paging.Cursor); err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
	}
	q := r.URL.Query()
	req := models.PendingActionSearchRequest{
		Limit:     paging.Limit,
		Offset:    paging.Offset,
		Cursor:    paging.Cursor,
		TenantID:  q.Get("tenant_id"),
		Status:    q.Get("status"),
		RunID:     q.Get("run_id"),
		Connector: q.Get("connector"),
	}
	actions, err := store.SearchPendingActions(ctx, &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if actions == nil {
		actions = []*models.PendingAction{}
	}
	next := ""
	if len(actions) == paging.Limit && len(actions) > 0 {
		last := actions[len(actions)-1]
		next = pagecursor.EncodeTime(last.CreatedAt, last.ID)
	}
	writeAdminListJSON(w, http.StatusOK, actions, next)
}

// GET /admin-api/pending-actions/{id}
func (s *Server) handleAdminGetPendingAction(w http.ResponseWriter, r *http.Request) {
	store, ok := s.pendingActions()
	if !ok {
		writeError(w, http.StatusNotImplemented, "pending actions require Postgres state backend (Supported profile)")
		return
	}
	a, err := store.GetPendingAction(tenant.SystemContext(r.Context()), r.PathValue("id"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// POST /admin-api/pending-actions/{id}/approve
// Re-evaluates policy: hard deny refuses approve; allow/pending mints a
// one-shot capability (status=approved) for the next matching tools/call.
func (s *Server) handleAdminApprovePendingAction(w http.ResponseWriter, r *http.Request) {
	store, ok := s.pendingActions()
	if !ok {
		writeError(w, http.StatusNotImplemented, "pending actions require Postgres state backend (Supported profile)")
		return
	}
	if !s.policy.Enabled() {
		writeError(w, http.StatusConflict, "policy engine is not enabled")
		return
	}
	ctx := tenant.SystemContext(r.Context())
	id := r.PathValue("id")
	a, err := store.GetPendingAction(ctx, id)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if a.Status != models.PendingStatusPending {
		writeError(w, http.StatusConflict, "pending action is not awaiting approval (status="+a.Status+")")
		return
	}
	dec := s.policy.Decide(ctx, policy.PolicyInput{
		Stage:      policy.StageToolCall,
		TenantID:   a.TenantID,
		AgentID:    a.AgentID,
		RunID:      a.RunID,
		Generation: a.Generation,
		Connector:  a.Connector,
		Tool:       a.Tool,
	})
	if dec.Effect == policy.EffectDeny {
		writeError(w, http.StatusConflict, "policy still denies this tool call; approve refused")
		return
	}
	if err := store.SetPendingActionStatus(ctx, id, models.PendingStatusPending, models.PendingStatusApproved); err != nil {
		handleStoreError(w, err)
		return
	}
	a, err = store.GetPendingAction(ctx, id)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// POST /admin-api/pending-actions/{id}/deny
func (s *Server) handleAdminDenyPendingAction(w http.ResponseWriter, r *http.Request) {
	store, ok := s.pendingActions()
	if !ok {
		writeError(w, http.StatusNotImplemented, "pending actions require Postgres state backend (Supported profile)")
		return
	}
	ctx := tenant.SystemContext(r.Context())
	id := r.PathValue("id")
	a, err := store.GetPendingAction(ctx, id)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if a.Status != models.PendingStatusPending {
		writeError(w, http.StatusConflict, "pending action is not awaiting approval (status="+a.Status+")")
		return
	}
	if err := store.SetPendingActionStatus(ctx, id, models.PendingStatusPending, models.PendingStatusDenied); err != nil {
		handleStoreError(w, err)
		return
	}
	a, err = store.GetPendingAction(ctx, id)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}
