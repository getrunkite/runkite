package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/pagecursor"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/tenant"
)

type mandatoryHITLStore interface {
	ListMandatoryHITLRules(ctx context.Context) ([]*models.MandatoryHITLRule, error)
	SearchMandatoryHITLRules(ctx context.Context, req *models.MandatoryHITLSearchRequest) ([]*models.MandatoryHITLRule, error)
	UpsertMandatoryHITLRule(ctx context.Context, r *models.MandatoryHITLRule) error
	GetMandatoryHITLRule(ctx context.Context, id string) (*models.MandatoryHITLRule, error)
	DeleteMandatoryHITLRule(ctx context.Context, id string) error
}

func (s *Server) mandatoryHITL() (mandatoryHITLStore, bool) {
	m, ok := s.store.(mandatoryHITLStore)
	return m, ok
}

// ReloadMandatoryHITL loads durable SQL rules into the in-process engine.
// Called after Admin CRUD on the writing replica, and by the overlay poll.
func (s *Server) ReloadMandatoryHITL(ctx context.Context) {
	if !s.policy.Enabled() {
		return
	}
	store, ok := s.mandatoryHITL()
	if !ok {
		return
	}
	rows, err := store.ListMandatoryHITLRules(ctx)
	if err != nil {
		slog.Warn("policy: reload mandatory_hitl failed", "error", err)
		return
	}
	s.policy.ReplaceMandatoryHITL(modelMandatoryHITLToPolicy(rows))
}

// GET /admin-api/mandatory-hitl
func (s *Server) handleAdminListMandatoryHITL(w http.ResponseWriter, r *http.Request) {
	store, ok := s.mandatoryHITL()
	if !ok {
		writeError(w, http.StatusNotImplemented, "mandatory HITL CRUD requires a SQL state backend (Postgres, MySQL, or SQLite)")
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
	req := models.MandatoryHITLSearchRequest{
		Limit:     paging.Limit,
		Offset:    paging.Offset,
		Cursor:    paging.Cursor,
		TenantID:  q.Get("tenant_id"),
		AgentID:   q.Get("agent_id"),
		Connector: q.Get("connector"),
	}
	rows, err := store.SearchMandatoryHITLRules(ctx, &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if rows == nil {
		rows = []*models.MandatoryHITLRule{}
	}
	next := ""
	if len(rows) == paging.Limit && len(rows) > 0 {
		last := rows[len(rows)-1]
		next = pagecursor.EncodeKey(last.TenantID, last.ID)
	}
	writeAdminListJSON(w, http.StatusOK, rows, next)
}

// GET /admin-api/mandatory-hitl/{id}
func (s *Server) handleAdminGetMandatoryHITL(w http.ResponseWriter, r *http.Request) {
	store, ok := s.mandatoryHITL()
	if !ok {
		writeError(w, http.StatusNotImplemented, "mandatory HITL CRUD requires a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	row, err := store.GetMandatoryHITLRule(tenant.SystemContext(r.Context()), r.PathValue("id"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// POST /admin-api/mandatory-hitl
func (s *Server) handleAdminCreateMandatoryHITL(w http.ResponseWriter, r *http.Request) {
	store, ok := s.mandatoryHITL()
	if !ok {
		writeError(w, http.StatusNotImplemented, "mandatory HITL CRUD requires a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	if !s.policy.Enabled() {
		writeError(w, http.StatusConflict, "policy engine is not enabled (configure policy section first)")
		return
	}
	var body models.MandatoryHITLRule
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validateMandatoryHITL(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.ID) == "" {
		id, err := randomHex(8)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to allocate rule id")
			return
		}
		body.ID = "mhitl-" + id
	}
	ctx := tenant.SystemContext(r.Context())
	if err := store.UpsertMandatoryHITLRule(ctx, &body); err != nil {
		handleStoreError(w, err)
		return
	}
	s.ReloadMandatoryHITL(ctx)
	out, err := store.GetMandatoryHITLRule(ctx, body.ID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// PUT /admin-api/mandatory-hitl/{id}
func (s *Server) handleAdminUpdateMandatoryHITL(w http.ResponseWriter, r *http.Request) {
	store, ok := s.mandatoryHITL()
	if !ok {
		writeError(w, http.StatusNotImplemented, "mandatory HITL CRUD requires a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	if !s.policy.Enabled() {
		writeError(w, http.StatusConflict, "policy engine is not enabled (configure policy section first)")
		return
	}
	id := r.PathValue("id")
	var body models.MandatoryHITLRule
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	body.ID = id
	if err := validateMandatoryHITL(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := tenant.SystemContext(r.Context())
	if _, err := store.GetMandatoryHITLRule(ctx, id); err != nil {
		handleStoreError(w, err)
		return
	}
	if err := store.UpsertMandatoryHITLRule(ctx, &body); err != nil {
		handleStoreError(w, err)
		return
	}
	s.ReloadMandatoryHITL(ctx)
	out, err := store.GetMandatoryHITLRule(ctx, id)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /admin-api/mandatory-hitl/{id}
func (s *Server) handleAdminDeleteMandatoryHITL(w http.ResponseWriter, r *http.Request) {
	store, ok := s.mandatoryHITL()
	if !ok {
		writeError(w, http.StatusNotImplemented, "mandatory HITL CRUD requires a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	ctx := tenant.SystemContext(r.Context())
	id := r.PathValue("id")
	if _, err := store.GetMandatoryHITLRule(ctx, id); err != nil {
		handleStoreError(w, err)
		return
	}
	if err := store.DeleteMandatoryHITLRule(ctx, id); err != nil {
		handleStoreError(w, err)
		return
	}
	s.ReloadMandatoryHITL(ctx)
	w.WriteHeader(http.StatusNoContent)
}

func validateMandatoryHITL(r *models.MandatoryHITLRule) error {
	if r == nil {
		return errString("body required")
	}
	if strings.TrimSpace(r.TenantID) == "" {
		return errString("tenant_id is required")
	}
	if strings.TrimSpace(r.Connector) == "" {
		return errString("connector is required")
	}
	return nil
}

func modelMandatoryHITLToPolicy(rows []*models.MandatoryHITLRule) []policy.MandatoryHITLRule {
	out := make([]policy.MandatoryHITLRule, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, policy.MandatoryHITLRule{
			ID:        r.ID,
			TenantID:  r.TenantID,
			AgentID:   r.AgentID,
			Connector: r.Connector,
			Tools:     append([]string(nil), r.Tools...),
		})
	}
	return out
}
