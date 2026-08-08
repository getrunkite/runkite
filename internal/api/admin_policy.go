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

// policyGrantStore is the SQL Admin CRUD surface for durable grants.
type policyGrantStore interface {
	ListPolicyGrants(ctx context.Context) ([]*models.PolicyGrant, error)
	SearchPolicyGrants(ctx context.Context, req *models.PolicyGrantSearchRequest) ([]*models.PolicyGrant, error)
	UpsertPolicyGrant(ctx context.Context, g *models.PolicyGrant) error
	GetPolicyGrant(ctx context.Context, id string) (*models.PolicyGrant, error)
	DeletePolicyGrant(ctx context.Context, id string) error
}

func (s *Server) policyGrants() (policyGrantStore, bool) {
	pg, ok := s.store.(policyGrantStore)
	return pg, ok
}

// ReloadPolicyOverlays loads durable SQL grants into the in-process engine.
// Called after Admin grant CRUD on the writing replica, and by the
// background overlay poll so sibling replicas converge without a restart.
// No-op when policy is off or the store has no grant table.
func (s *Server) ReloadPolicyOverlays(ctx context.Context) {
	if !s.policy.Enabled() {
		return
	}
	store, ok := s.policyGrants()
	if !ok {
		return
	}
	rows, err := store.ListPolicyGrants(ctx)
	if err != nil {
		slog.Warn("policy: reload overlays failed", "error", err)
		return
	}
	s.policy.ReplaceOverlays(modelGrantsToPolicy(rows))
}

// GET /admin-api/policy-grants
func (s *Server) handleAdminListPolicyGrants(w http.ResponseWriter, r *http.Request) {
	store, ok := s.policyGrants()
	if !ok {
		writeError(w, http.StatusNotImplemented, "policy grant CRUD requires a SQL state backend (Postgres, MySQL, or SQLite)")
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
	req := models.PolicyGrantSearchRequest{
		Limit:     paging.Limit,
		Offset:    paging.Offset,
		Cursor:    paging.Cursor,
		TenantID:  q.Get("tenant_id"),
		AgentID:   q.Get("agent_id"),
		Connector: q.Get("connector"),
	}
	grants, err := store.SearchPolicyGrants(ctx, &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if grants == nil {
		grants = []*models.PolicyGrant{}
	}
	next := ""
	if len(grants) == paging.Limit && len(grants) > 0 {
		last := grants[len(grants)-1]
		next = pagecursor.EncodeKey(last.TenantID, last.ID)
	}
	writeAdminListJSON(w, http.StatusOK, grants, next)
}

// GET /admin-api/policy-grants/{id}
func (s *Server) handleAdminGetPolicyGrant(w http.ResponseWriter, r *http.Request) {
	store, ok := s.policyGrants()
	if !ok {
		writeError(w, http.StatusNotImplemented, "policy grant CRUD requires a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	g, err := store.GetPolicyGrant(tenant.SystemContext(r.Context()), r.PathValue("id"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// POST /admin-api/policy-grants
func (s *Server) handleAdminCreatePolicyGrant(w http.ResponseWriter, r *http.Request) {
	store, ok := s.policyGrants()
	if !ok {
		writeError(w, http.StatusNotImplemented, "policy grant CRUD requires a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	if !s.policy.Enabled() {
		writeError(w, http.StatusConflict, "policy engine is not enabled (configure policy.grants or policy.webhook first)")
		return
	}
	var body models.PolicyGrant
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validatePolicyGrant(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.ID) == "" {
		id, err := randomHex(8)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to allocate grant id")
			return
		}
		body.ID = "grant-" + id
	}
	ctx := tenant.SystemContext(r.Context())
	if err := store.UpsertPolicyGrant(ctx, &body); err != nil {
		handleStoreError(w, err)
		return
	}
	s.ReloadPolicyOverlays(ctx)
	g, err := store.GetPolicyGrant(ctx, body.ID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

// PUT /admin-api/policy-grants/{id}
func (s *Server) handleAdminUpdatePolicyGrant(w http.ResponseWriter, r *http.Request) {
	store, ok := s.policyGrants()
	if !ok {
		writeError(w, http.StatusNotImplemented, "policy grant CRUD requires a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	if !s.policy.Enabled() {
		writeError(w, http.StatusConflict, "policy engine is not enabled (configure policy.grants or policy.webhook first)")
		return
	}
	id := r.PathValue("id")
	var body models.PolicyGrant
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	body.ID = id
	if err := validatePolicyGrant(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := tenant.SystemContext(r.Context())
	if _, err := store.GetPolicyGrant(ctx, id); err != nil {
		handleStoreError(w, err)
		return
	}
	if err := store.UpsertPolicyGrant(ctx, &body); err != nil {
		handleStoreError(w, err)
		return
	}
	s.ReloadPolicyOverlays(ctx)
	g, err := store.GetPolicyGrant(ctx, id)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// DELETE /admin-api/policy-grants/{id}
func (s *Server) handleAdminDeletePolicyGrant(w http.ResponseWriter, r *http.Request) {
	store, ok := s.policyGrants()
	if !ok {
		writeError(w, http.StatusNotImplemented, "policy grant CRUD requires a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	ctx := tenant.SystemContext(r.Context())
	id := r.PathValue("id")
	if _, err := store.GetPolicyGrant(ctx, id); err != nil {
		handleStoreError(w, err)
		return
	}
	if err := store.DeletePolicyGrant(ctx, id); err != nil {
		handleStoreError(w, err)
		return
	}
	s.ReloadPolicyOverlays(ctx)
	w.WriteHeader(http.StatusNoContent)
}

func validatePolicyGrant(g *models.PolicyGrant) error {
	if g == nil {
		return errString("body required")
	}
	if strings.TrimSpace(g.TenantID) == "" {
		return errString("tenant_id is required")
	}
	if strings.TrimSpace(g.AgentID) == "" {
		return errString("agent_id is required")
	}
	if strings.TrimSpace(g.Connector) == "" {
		return errString("connector is required")
	}
	return nil
}

type errString string

func (e errString) Error() string { return string(e) }

func modelGrantsToPolicy(rows []*models.PolicyGrant) []policy.Grant {
	out := make([]policy.Grant, 0, len(rows))
	for _, g := range rows {
		if g == nil {
			continue
		}
		pg := policy.Grant{
			ID: g.ID, TenantID: g.TenantID, AgentID: g.AgentID, Connector: g.Connector,
		}
		if g.Tools != nil {
			pg.Tools = &policy.ToolFilter{Allow: g.Tools.Allow, Deny: g.Tools.Deny}
		}
		out = append(out, pg)
	}
	return out
}
