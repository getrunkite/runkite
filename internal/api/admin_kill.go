package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/tenant"
)

// killDrainPageSize is the SearchRuns page size while draining. Always
// re-query from offset 0: cancelled runs leave pending/running, so the
// next page is whatever remains.
const killDrainPageSize = 200

// killDrainMaxPages caps work per status so a stuck cancel (status never
// leaves the filter) cannot loop forever. 200 * 1000 = 200k runs/status.
const killDrainMaxPages = 1000

// GET /admin-api/kill-switches
func (s *Server) handleAdminListKillSwitches(w http.ResponseWriter, r *http.Request) {
	store, ok := s.killSwitches()
	if !ok {
		writeError(w, http.StatusNotImplemented, "kill switches require a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	ctx := tenant.SystemContext(r.Context())
	paging, err := adminListPaging(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if paging.Cursor != "" {
		if _, err := pagecursor.DecodeKey(paging.Cursor); err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
	}
	q := r.URL.Query()
	req := models.KillSwitchSearchRequest{
		Limit:    paging.Limit,
		Offset:   paging.Offset,
		Cursor:   paging.Cursor,
		TenantID: q.Get("tenant_id"),
		AgentID:  q.Get("agent_id"),
	}
	rows, err := store.SearchKillSwitches(ctx, &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if rows == nil {
		rows = []*models.KillSwitch{}
	}
	next := ""
	if len(rows) == paging.Limit && len(rows) > 0 {
		last := rows[len(rows)-1]
		next = pagecursor.EncodeKey(last.TenantID, last.ID)
	}
	writeAdminListJSON(w, http.StatusOK, rows, next)
}

// GET /admin-api/kill-switches/{id}
func (s *Server) handleAdminGetKillSwitch(w http.ResponseWriter, r *http.Request) {
	store, ok := s.killSwitches()
	if !ok {
		writeError(w, http.StatusNotImplemented, "kill switches require a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	k, err := store.GetKillSwitch(tenant.SystemContext(r.Context()), r.PathValue("id"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, k)
}

type killSwitchBody struct {
	ID        string `json:"id,omitempty"`
	TenantID  string `json:"tenant_id"`
	AgentID   string `json:"agent_id,omitempty"`
	PauseOnly bool   `json:"pause_only"`
	Reason    string `json:"reason,omitempty"`
}

// POST /admin-api/kill-switches — upsert flag; unless pause_only, cancel
// non-terminal runs in scope (tenant or tenant+agent).
func (s *Server) handleAdminCreateKillSwitch(w http.ResponseWriter, r *http.Request) {
	store, ok := s.killSwitches()
	if !ok {
		writeError(w, http.StatusNotImplemented, "kill switches require a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	var body killSwitchBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	body.TenantID = strings.TrimSpace(body.TenantID)
	body.AgentID = strings.TrimSpace(body.AgentID)
	if body.TenantID == "" {
		writeError(w, http.StatusBadRequest, "tenant_id is required")
		return
	}
	id := strings.TrimSpace(body.ID)
	if id == "" {
		if body.AgentID != "" {
			id = "kill-" + body.TenantID + "-" + body.AgentID
		} else {
			id = "kill-" + body.TenantID
		}
	}
	createdBy := ""
	if ar := auth.FromContext(r.Context()); ar != nil {
		createdBy = ar.Identity
	}
	k := &models.KillSwitch{
		ID:        id,
		TenantID:  body.TenantID,
		AgentID:   body.AgentID,
		PauseOnly: body.PauseOnly,
		Reason:    body.Reason,
		CreatedBy: createdBy,
	}
	ctx := tenant.SystemContext(r.Context())
	if err := store.UpsertKillSwitch(ctx, k); err != nil {
		handleStoreError(w, err)
		return
	}
	cancelled := 0
	if !body.PauseOnly {
		cancelled = s.drainKillScope(ctx, body.TenantID, body.AgentID)
	}
	out, err := store.GetKillSwitch(ctx, id)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	s.writeSecurityAudit(ctx, &models.AuditEvent{
		TenantID:     body.TenantID,
		Actor:        createdBy,
		Action:       "kill.activate",
		ResourceType: "kill_switch",
		ResourceID:   id,
		Decision:     policy.EffectAllow,
		ReasonCode:   policy.ReasonKillActivate,
		RuleID:       id,
		AgentID:      body.AgentID,
		Attrs: map[string]interface{}{
			"reason":     body.Reason,
			"pause_only": body.PauseOnly,
			"cancelled":  cancelled,
		},
	})
	slog.Info("kill switch activated",
		"id", id, "tenant_id", body.TenantID, "agent_id", body.AgentID,
		"pause_only", body.PauseOnly, "cancelled", cancelled)
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"kill_switch": out,
		"cancelled":   cancelled,
	})
}

// DELETE /admin-api/kill-switches/{id}
func (s *Server) handleAdminDeleteKillSwitch(w http.ResponseWriter, r *http.Request) {
	store, ok := s.killSwitches()
	if !ok {
		writeError(w, http.StatusNotImplemented, "kill switches require a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	ctx := tenant.SystemContext(r.Context())
	id := r.PathValue("id")
	existing, err := store.GetKillSwitch(ctx, id)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if err := store.DeleteKillSwitch(ctx, id); err != nil {
		handleStoreError(w, err)
		return
	}
	actor := ""
	if ar := auth.FromContext(r.Context()); ar != nil {
		actor = ar.Identity
	}
	s.writeSecurityAudit(ctx, &models.AuditEvent{
		TenantID:     existing.TenantID,
		Actor:        actor,
		Action:       "kill.clear",
		ResourceType: "kill_switch",
		ResourceID:   id,
		Decision:     policy.EffectAllow,
		ReasonCode:   policy.ReasonKillClear,
		RuleID:       id,
		AgentID:      existing.AgentID,
		Attrs: map[string]interface{}{
			"reason": existing.Reason,
		},
	})
	w.WriteHeader(http.StatusNoContent)
}

// drainKillScope cancels every pending/running run for tenant (and
// optional agent). Interrupted is already terminal (cancel's outcome) —
// searching it would double-count and is not needed for blast-radius
// drain. Pages until the filter is empty (offset always 0 because
// cancelled rows leave pending/running).
func (s *Server) drainKillScope(ctx context.Context, tenantID, agentID string) int {
	tenantCtx := tenant.WithContext(ctx, tenantID)
	// Only statuses that are still executing / queued. Interrupted,
	// success, error, timeout are already terminal (see isTerminalStatus).
	statuses := []models.RunStatus{
		models.RunStatusPending,
		models.RunStatusRunning,
	}
	cancelled := 0
	for _, st := range statuses {
		st := st
		for page := 0; page < killDrainMaxPages; page++ {
			runs, err := s.store.SearchRuns(tenantCtx, &models.RunSearchRequest{
				Status:  &st,
				AgentID: agentID,
				Limit:   killDrainPageSize,
				Offset:  0, // cancelled runs leave this filter; never advance offset
			})
			if err != nil {
				slog.Warn("kill switch: search runs failed", "tenant_id", tenantID, "status", st, "error", err)
				break
			}
			if len(runs) == 0 {
				break
			}
			pageCancelled := 0
			for _, run := range runs {
				if run == nil {
					continue
				}
				if isTerminalStatus(run.Status) {
					continue
				}
				if _, err := s.cancelRunSingle(tenantCtx, run, false); err != nil {
					slog.Warn("kill switch: cancel failed", "run_id", run.RunID, "error", err)
					continue
				}
				cancelled++
				pageCancelled++
			}
			if pageCancelled == 0 {
				// Nothing left the filter — stop rather than spin.
				slog.Warn("kill switch: drain stalled", "tenant_id", tenantID, "status", st, "remaining", len(runs))
				break
			}
			if len(runs) < killDrainPageSize {
				break
			}
		}
	}
	return cancelled
}
