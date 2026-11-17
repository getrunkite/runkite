package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// runObservationReference identifies a durable row, not its changing status. It is a
// nonsecret reference and never supplies authorization or permanent deduplication.
func runObservationReference(run *models.Run) string {
	if run.IncarnationID == "" {
		return ""
	}
	tid := run.TenantID
	if tid == "" {
		tid = tenant.DefaultTenant
	}
	raw, _ := json.Marshal([]string{tid, run.RunID, run.IncarnationID})
	sum := sha256.Sum256(raw)
	return "runkite-run-v1-" + hex.EncodeToString(sum[:])
}

func (s *Server) publicRunObservation(w http.ResponseWriter, r *http.Request, wait bool) (*models.Run, bool) {
	run, err := s.store.GetRun(r.Context(), r.PathValue("runID"))
	if err != nil {
		handleStoreError(w, err)
		return nil, false
	}
	if !auth.CanObserveAgent(auth.FromContext(r.Context()), run.AgentID) {
		writeError(w, http.StatusForbidden, "not authorized to observe this agent's runs")
		return nil, false
	}
	reference := runObservationReference(run)
	scoped := !auth.CanReadAllRuns(auth.FromContext(r.Context()))
	if scoped && reference == "" {
		writeError(w, http.StatusServiceUnavailable, "scoped observation requires a durable SQL run incarnation")
		return nil, false
	}
	supplied := r.Header.Values("Runkite-Expected-Run-Reference")
	if len(supplied) > 1 || (len(supplied) == 1 && (reference == "" || supplied[0] != reference)) {
		writeError(w, http.StatusPreconditionFailed, "retained run reference changed")
		return nil, false
	}
	if wait && scoped && len(supplied) == 0 {
		writeError(w, http.StatusPreconditionRequired, "read the retained run and supply its Runkite-Run-Reference as Runkite-Expected-Run-Reference")
		return nil, false
	}
	if reference != "" {
		w.Header().Set("Runkite-Run-Reference", reference)
	}
	w.Header().Set("Cache-Control", "no-store")
	return run, true
}

// getObservedRun refuses deletion/replacement throughout a wait. An internal
// backend record identity does not enable public SQL observation references.
func (s *Server) getObservedRun(ctx context.Context, original *models.Run) (*models.Run, error) {
	latest, err := s.store.GetRun(ctx, original.RunID)
	if err != nil {
		return nil, err
	}
	if state.RunIdentity(latest) != state.RunIdentity(original) || latest.AgentID != original.AgentID || latest.ThreadID != original.ThreadID ||
		(latest.TenantID != "" && latest.TenantID != tenant.FromContext(ctx)) {
		return nil, &state.ErrConflict{Resource: "run", ID: original.RunID, Reason: "retained run incarnation changed"}
	}
	return latest, nil
}
