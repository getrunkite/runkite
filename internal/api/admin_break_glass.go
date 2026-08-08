package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/tenant"
)

// maxBreakGlassDuration is the hard ceiling for a single window mint.
const maxBreakGlassDuration = 24 * time.Hour

type breakGlassStore interface {
	CreateBreakGlassWindow(ctx context.Context, w *models.BreakGlassWindow) error
	GetBreakGlassWindow(ctx context.Context, id string) (*models.BreakGlassWindow, error)
	DeleteBreakGlassWindow(ctx context.Context, id string) error
	ListBreakGlassWindows(ctx context.Context) ([]*models.BreakGlassWindow, error)
	SearchBreakGlassWindows(ctx context.Context, req *models.BreakGlassSearchRequest) ([]*models.BreakGlassWindow, error)
	FindActiveBreakGlass(ctx context.Context, tenantID, agentID string) (*models.BreakGlassWindow, error)
}

type auditEventWriter interface {
	WriteAuditEvent(ctx context.Context, ev *models.AuditEvent) error
}

func (s *Server) breakGlass() (breakGlassStore, bool) {
	bg, ok := s.store.(breakGlassStore)
	return bg, ok
}

func (s *Server) auditWriter() (auditEventWriter, bool) {
	a, ok := s.store.(auditEventWriter)
	return a, ok
}

// GET /admin-api/break-glass
func (s *Server) handleAdminListBreakGlass(w http.ResponseWriter, r *http.Request) {
	store, ok := s.breakGlass()
	if !ok {
		writeError(w, http.StatusNotImplemented, "break-glass windows require a SQL state backend (Postgres, MySQL, or SQLite)")
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
	req := models.BreakGlassSearchRequest{
		Limit:    paging.Limit,
		Offset:   paging.Offset,
		Cursor:   paging.Cursor,
		TenantID: q.Get("tenant_id"),
		AgentID:  q.Get("agent_id"),
	}
	rows, err := store.SearchBreakGlassWindows(ctx, &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if rows == nil {
		rows = []*models.BreakGlassWindow{}
	}
	next := ""
	if len(rows) == paging.Limit && len(rows) > 0 {
		last := rows[len(rows)-1]
		next = pagecursor.EncodeKey(last.TenantID, last.ID)
	}
	writeAdminListJSON(w, http.StatusOK, rows, next)
}

// GET /admin-api/break-glass/{id}
func (s *Server) handleAdminGetBreakGlass(w http.ResponseWriter, r *http.Request) {
	store, ok := s.breakGlass()
	if !ok {
		writeError(w, http.StatusNotImplemented, "break-glass windows require a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	bg, err := store.GetBreakGlassWindow(tenant.SystemContext(r.Context()), r.PathValue("id"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bg)
}

type breakGlassBody struct {
	ID        string `json:"id,omitempty"`
	TenantID  string `json:"tenant_id"`
	AgentID   string `json:"agent_id,omitempty"`
	Reason    string `json:"reason"`
	StartsAt  string `json:"starts_at,omitempty"` // RFC3339; default now
	ExpiresAt string `json:"expires_at"`          // RFC3339; required
}

// POST /admin-api/break-glass — mint a time-bounded policy bypass.
func (s *Server) handleAdminCreateBreakGlass(w http.ResponseWriter, r *http.Request) {
	store, ok := s.breakGlass()
	if !ok {
		writeError(w, http.StatusNotImplemented, "break-glass windows require a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	var body breakGlassBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	body.TenantID = strings.TrimSpace(body.TenantID)
	body.AgentID = strings.TrimSpace(body.AgentID)
	body.Reason = strings.TrimSpace(body.Reason)
	if body.TenantID == "" {
		writeError(w, http.StatusBadRequest, "tenant_id is required")
		return
	}
	if body.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}
	now := time.Now().UTC()
	startsAt := now
	if strings.TrimSpace(body.StartsAt) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(body.StartsAt))
		if err != nil {
			writeError(w, http.StatusBadRequest, "starts_at must be RFC3339")
			return
		}
		startsAt = t.UTC()
	}
	if strings.TrimSpace(body.ExpiresAt) == "" {
		writeError(w, http.StatusBadRequest, "expires_at is required")
		return
	}
	expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(body.ExpiresAt))
	if err != nil {
		writeError(w, http.StatusBadRequest, "expires_at must be RFC3339")
		return
	}
	expiresAt = expiresAt.UTC()
	if !expiresAt.After(startsAt) {
		writeError(w, http.StatusBadRequest, "expires_at must be after starts_at")
		return
	}
	if expiresAt.Sub(startsAt) > maxBreakGlassDuration {
		writeError(w, http.StatusBadRequest, "break-glass window cannot exceed 24h")
		return
	}
	if !expiresAt.After(now) {
		writeError(w, http.StatusBadRequest, "expires_at must be in the future")
		return
	}
	id := strings.TrimSpace(body.ID)
	if id == "" {
		suffix, err := randomHex(8)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to mint break-glass id")
			return
		}
		id = "bg-" + suffix
	}
	createdBy := ""
	if ar := auth.FromContext(r.Context()); ar != nil {
		createdBy = ar.Identity
	}
	win := &models.BreakGlassWindow{
		ID:        id,
		TenantID:  body.TenantID,
		AgentID:   body.AgentID,
		Reason:    body.Reason,
		CreatedBy: createdBy,
		StartsAt:  startsAt,
		ExpiresAt: expiresAt,
	}
	ctx := tenant.SystemContext(r.Context())
	if err := store.CreateBreakGlassWindow(ctx, win); err != nil {
		handleStoreError(w, err)
		return
	}
	out, err := store.GetBreakGlassWindow(ctx, id)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	s.writeBreakGlassAdminAudit(ctx, "break_glass.mint", createdBy, out)
	slog.Info("break-glass window minted",
		"id", id, "tenant_id", body.TenantID, "agent_id", body.AgentID,
		"expires_at", expiresAt)
	writeJSON(w, http.StatusCreated, out)
}

// DELETE /admin-api/break-glass/{id}
func (s *Server) handleAdminDeleteBreakGlass(w http.ResponseWriter, r *http.Request) {
	store, ok := s.breakGlass()
	if !ok {
		writeError(w, http.StatusNotImplemented, "break-glass windows require a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	ctx := tenant.SystemContext(r.Context())
	id := r.PathValue("id")
	existing, err := store.GetBreakGlassWindow(ctx, id)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if err := store.DeleteBreakGlassWindow(ctx, id); err != nil {
		handleStoreError(w, err)
		return
	}
	actor := ""
	if ar := auth.FromContext(r.Context()); ar != nil {
		actor = ar.Identity
	}
	s.writeBreakGlassAdminAudit(ctx, "break_glass.revoke", actor, existing)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeBreakGlassAdminAudit(ctx context.Context, action, actor string, win *models.BreakGlassWindow) {
	aw, ok := s.auditWriter()
	if !ok || win == nil {
		return
	}
	suffix, err := randomHex(8)
	if err != nil {
		slog.Warn("break-glass admin audit id mint failed", "action", action, "id", win.ID, "error", err)
		return
	}
	ev := &models.AuditEvent{
		ID:           "bg-audit-" + suffix,
		TS:           time.Now().UTC(),
		TenantID:     win.TenantID,
		Actor:        actor,
		Action:       action,
		ResourceType: "break_glass",
		ResourceID:   win.ID,
		Decision:     policy.EffectAllow,
		ReasonCode:   policy.ReasonBreakGlass,
		RuleID:       win.ID,
		AgentID:      win.AgentID,
		Attrs: map[string]interface{}{
			"reason":     win.Reason,
			"starts_at":  win.StartsAt.UTC().Format(time.RFC3339),
			"expires_at": win.ExpiresAt.UTC().Format(time.RFC3339),
			"created_by": win.CreatedBy,
		},
	}
	if err := aw.WriteAuditEvent(ctx, ev); err != nil {
		slog.Warn("break-glass admin audit write failed", "action", action, "id", win.ID, "error", err)
	}
}
