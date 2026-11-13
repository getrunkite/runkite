package api

import (
	"context"
	"net/http"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// pendingActionStore is the SQL Admin + one-shot capability surface.
type pendingActionStore interface {
	// CreatePendingAction returns the canonical scoped action ID through a.ID.
	CreatePendingAction(ctx context.Context, a *models.PendingAction) error
	GetPendingAction(ctx context.Context, id string) (*models.PendingAction, error)
	SearchPendingActions(ctx context.Context, req *models.PendingActionSearchRequest) ([]*models.PendingAction, error)
	SetPendingActionStatus(ctx context.Context, id, fromStatus, toStatus string) error
	SetPendingActionApproved(ctx context.Context, id, decidedBy string) error
	ConsumeApprovedAction(ctx context.Context, tenantID, agentID, runID string, generation int64, connector, tool, argsDigest, targetRevision string) (string, error)
	// ExpireGovernancePending denies embedded rows whose expires_at has
	// passed. Connector rows (nil expires_at) stay pending.
	ExpireGovernancePending(ctx context.Context, now time.Time) error
}

func (s *Server) pendingActions() (pendingActionStore, bool) {
	pg, ok := s.store.(pendingActionStore)
	return pg, ok
}

// GET /admin-api/pending-actions
func (s *Server) handleAdminListPendingActions(w http.ResponseWriter, r *http.Request) {
	store, ok := s.pendingActions()
	if !ok {
		writeError(w, http.StatusNotImplemented, "pending actions require a SQL state backend (Postgres, MySQL, or SQLite)")
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
		writeError(w, http.StatusNotImplemented, "pending actions require a SQL state backend (Postgres, MySQL, or SQLite)")
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
func (s *Server) handleAdminApprovePendingAction(w http.ResponseWriter, r *http.Request) {
	a, err := s.approvePendingAction(tenant.SystemContext(r.Context()), r.PathValue("id"), pendingActor(r))
	if err != nil {
		writePendingDecisionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// POST /admin-api/pending-actions/{id}/deny
func (s *Server) handleAdminDenyPendingAction(w http.ResponseWriter, r *http.Request) {
	a, err := s.denyPendingAction(tenant.SystemContext(r.Context()), r.PathValue("id"))
	if err != nil {
		writePendingDecisionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

type pendingDecisionError struct {
	status  int
	message string
}

func (e *pendingDecisionError) Error() string { return e.message }

func writePendingDecisionError(w http.ResponseWriter, err error) {
	if e, ok := err.(*pendingDecisionError); ok {
		writeError(w, e.status, e.message)
		return
	}
	handleStoreError(w, err)
}

func pendingActor(r *http.Request) string {
	if result := auth.FromContext(r.Context()); result != nil {
		return result.Identity
	}
	return ""
}

// pendingActionForDecision accepts only a trusted system or tenant context.
// Both approval surfaces use this core and retain the same policy and binding checks.
func (s *Server) pendingActionForDecision(ctx context.Context, id string) (pendingActionStore, *models.PendingAction, error) {
	store, ok := s.pendingActions()
	if !ok {
		return nil, nil, &pendingDecisionError{http.StatusNotImplemented, "pending actions require a SQL state backend (Postgres, MySQL, or SQLite)"}
	}
	a, err := store.GetPendingAction(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if !tenant.IsSystem(ctx) && a.TenantID != tenant.FromContext(ctx) {
		return nil, nil, &state.ErrNotFound{Resource: "pending_action", ID: id}
	}
	if a.Status != models.PendingStatusPending {
		return nil, nil, &pendingDecisionError{http.StatusConflict, "pending action is not awaiting approval (status=" + a.Status + ")"}
	}
	return store, a, nil
}

// approvePendingAction mints the existing one-shot exact-argument capability.
// The caller chooses an authenticated tenant context or explicit global Admin context.
func (s *Server) approvePendingAction(ctx context.Context, id, actor string) (*models.PendingAction, error) {
	store, a, err := s.pendingActionForDecision(ctx, id)
	if err != nil {
		return nil, err
	}
	if !s.policy.Enabled() {
		return nil, &pendingDecisionError{http.StatusConflict, "policy engine is not enabled"}
	}
	if a.ExpiresAt == nil && !models.ValidConnectorTargetRevision(a.TargetRevision) {
		return nil, &pendingDecisionError{http.StatusConflict, "legacy connector target binding; resubmit the operation for fresh approval"}
	}
	if !currentPendingBinding(a) {
		return nil, &pendingDecisionError{http.StatusConflict, "legacy argument binding; resubmit the operation for fresh approval"}
	}
	if err := s.checkPendingTargetRevision(a); err != nil {
		return nil, err
	}
	dec := s.policy.Decide(ctx, policy.PolicyInput{
		Stage: policy.StageToolCall, TenantID: a.TenantID, AgentID: a.AgentID,
		RunID: a.RunID, Generation: a.Generation, Connector: a.Connector, Tool: a.Tool,
		Args: a.Args, ArgsDigest: a.ArgsDigest, ArgsMeta: a.Args, TargetRevision: a.TargetRevision,
	})
	if dec.Effect != policy.EffectAllow && dec.Effect != policy.EffectPending {
		return nil, &pendingDecisionError{http.StatusConflict, "policy still denies this tool call; approve refused"}
	}
	// A webhook or audit write may have delayed the decision. Recheck before
	// issuing authority; consumption still matches the exact pinned revision.
	if err := s.checkPendingTargetRevision(a); err != nil {
		return nil, err
	}
	if err := store.SetPendingActionApproved(ctx, id, actor); err != nil {
		return nil, err
	}
	return store.GetPendingAction(ctx, id)
}

func (s *Server) checkPendingTargetRevision(a *models.PendingAction) error {
	if a.ExpiresAt != nil {
		return nil
	}
	if s.connectors != nil {
		current, err := s.connectors.TargetRevision(a.Connector)
		if err == nil && current == a.TargetRevision {
			return nil
		}
	}
	return &pendingDecisionError{http.StatusConflict, "connector target changed or is unavailable; resubmit the operation for fresh approval"}
}

func (s *Server) denyPendingAction(ctx context.Context, id string) (*models.PendingAction, error) {
	store, _, err := s.pendingActionForDecision(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := store.SetPendingActionStatus(ctx, id, models.PendingStatusPending, models.PendingStatusDenied); err != nil {
		return nil, err
	}
	return store.GetPendingAction(ctx, id)
}
