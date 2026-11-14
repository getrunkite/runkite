package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/getrunkite/runkite/internal/credentials"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *Server) SetCredentialManager(m *credentials.Manager) { s.credentialManager = m }

// CheckCredentialOwner checks the durable action and current assignment. No
// owner, provider path or raw lease handle is accepted from an HTTP body.
func (s *Server) CheckCredentialOwner(ctx context.Context, o credentials.Owner, bindingID string) error {
	if s.credentialManager == nil || s.queue == nil || s.connectors == nil || s.policy == nil || !s.policy.AuditRequired() || !s.policy.ActionEvidenceEnabled() {
		return credentials.ErrUnavailable
	}
	ctx = tenant.ScopedContext(ctx, o.TenantID)
	live, err := s.queue.LookupInflight(ctx, o.RunID)
	if err != nil || live == nil || o.RunIncarnationID == "" {
		return credentials.ErrStaleOwner
	}
	liveTenant := live.TenantID
	if liveTenant == "" {
		liveTenant = tenant.DefaultTenant
	}
	if liveTenant != o.TenantID || live.Generation != o.Generation || live.RunIncarnationID != o.RunIncarnationID {
		return credentials.ErrStaleOwner
	}
	run, err := s.store.GetRun(ctx, o.RunID)
	if err != nil || run.IncarnationID != o.RunIncarnationID || run.AgentID != live.GraphID || run.ThreadID != live.ThreadID {
		return credentials.ErrStaleOwner
	}
	if state.CheckRunCheckpointBinding(run, state.RunCheckpointBinding{RunID: o.RunID, Generation: o.Generation, ThreadID: live.ThreadID}) != nil {
		return credentials.ErrStaleOwner
	}
	principal := ""
	if live.User != nil {
		principal = live.User.Identity
	}
	if raw, ok := run.Metadata["run_manifest"]; ok {
		var manifest models.RunManifest
		encoded, err := json.Marshal(raw)
		if err != nil || json.Unmarshal(encoded, &manifest) != nil || manifest.TenantID != o.TenantID || manifest.AgentID != live.GraphID {
			return credentials.ErrStaleOwner
		}
		frozenPrincipal := ""
		if manifest.Principal != nil {
			frozenPrincipal = manifest.Principal.Identity
		}
		if frozenPrincipal != principal {
			return credentials.ErrStaleOwner
		}
	}
	store, ok := s.store.(state.ProtectedActionStore)
	if !ok {
		return credentials.ErrUnavailable
	}
	action, err := store.GetConnectorAction(ctx, o.ActionID)
	if err != nil || action.TenantID != o.TenantID || action.RunID != o.RunID || action.RunIncarnationID != o.RunIncarnationID || action.Generation != o.Generation || action.AgentID != live.GraphID || action.ThreadID != live.ThreadID || action.Principal != principal || action.Connector != o.ConsumerID || action.State != models.ConnectorActionDispatchIntent || action.DispatchDeadlineAt == nil || !action.DispatchDeadlineAt.After(time.Now()) || action.PolicyRevision == nil || *action.PolicyRevision != s.policy.Revision() {
		return credentials.ErrStaleOwner
	}
	expectedRef, refErr := s.credentialManager.ReferenceFor(o, bindingID)
	if refErr != nil || action.CredentialRef == nil || *action.CredentialRef != expectedRef {
		return credentials.ErrStaleOwner
	}
	target, _, err := s.connectors.BindMCPRequestTarget(ctx, o.ConsumerID)
	if err != nil {
		return credentials.ErrUnavailable
	}
	currentID, revision, err := s.connectors.BoundCredentialBinding(target, o.ConsumerID)
	expected, managerErr := s.credentialManager.BindingRevision(bindingID)
	if err != nil || managerErr != nil || currentID != bindingID || revision != expected {
		return credentials.ErrUnavailable
	}
	return nil
}

func (s *Server) credentialReadTenant(w http.ResponseWriter, r *http.Request, review bool) (context.Context, string, bool) {
	if s.credentialManager == nil {
		writeError(w, http.StatusNotImplemented, "credential lifecycle is not configured")
		return nil, "", false
	}
	if review {
		ctx, _, ok := reviewContext(w, r, false)
		if !ok {
			return nil, "", false
		}
		return ctx, tenant.FromContext(ctx), true
	}
	t := r.URL.Query().Get("tenant_id")
	if t == "" {
		writeError(w, http.StatusBadRequest, "explicit tenant_id is required for credential metadata")
		return nil, "", false
	}
	return tenant.ScopedContext(r.Context(), t), t, true
}

func (s *Server) credentialList(w http.ResponseWriter, r *http.Request, review bool) {
	ctx, t, ok := s.credentialReadTenant(w, r, review)
	if !ok {
		return
	}
	rows, err := s.credentialManager.Search(ctx, t, 100)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "credential metadata unavailable")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) credentialDetail(w http.ResponseWriter, r *http.Request, review bool) {
	ctx, t, ok := s.credentialReadTenant(w, r, review)
	if !ok {
		return
	}
	row, err := s.credentialManager.Get(ctx, t, r.PathValue("id"))
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, credentials.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, "credential lease metadata unavailable")
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleAdminCredentialLeases(w http.ResponseWriter, r *http.Request) {
	s.credentialList(w, r, false)
}
func (s *Server) handleAdminCredentialLease(w http.ResponseWriter, r *http.Request) {
	s.credentialDetail(w, r, false)
}
func (s *Server) handleReviewCredentialLeases(w http.ResponseWriter, r *http.Request) {
	s.credentialList(w, r, true)
}
func (s *Server) handleReviewCredentialLease(w http.ResponseWriter, r *http.Request) {
	s.credentialDetail(w, r, true)
}

func (s *Server) handleAdminCredentialMutation(w http.ResponseWriter, r *http.Request) {
	ctx, t, ok := s.credentialReadTenant(w, r, false)
	if !ok {
		return
	}
	if !reviewEmptyDecisionBody(w, r) {
		return
	}
	row, err := s.credentialManager.Get(ctx, t, r.PathValue("id"))
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, credentials.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, "credential lease metadata unavailable")
		return
	}
	var result *credentials.Record
	switch r.PathValue("operation") {
	case "renew":
		result, err = s.credentialManager.Renew(ctx, row.Owner, row.ID)
	case "revoke":
		result, err = s.credentialManager.Revoke(ctx, row.Owner, row.ID)
	default:
		writeError(w, http.StatusNotFound, "unknown credential operation")
		return
	}
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, credentials.ErrUnavailable) {
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, "credential operation is not confirmed; inspect current metadata")
		return
	}
	writeJSON(w, http.StatusOK, result)
}
