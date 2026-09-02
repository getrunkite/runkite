package api

import (
	"net/http"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

// GET /admin-api/usage/summary?tenant_id=&agent_id=&from=&to=
func (s *Server) handleAdminUsageSummary(w http.ResponseWriter, r *http.Request) {
	store, ok := s.usageEvents()
	if !ok {
		writeError(w, http.StatusNotImplemented, "usage summary requires a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	q := r.URL.Query()
	req := &models.UsageSummaryRequest{
		TenantID: q.Get("tenant_id"),
		AgentID:  q.Get("agent_id"),
	}
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid from (want RFC3339)")
			return
		}
		utc := t.UTC()
		req.From = &utc
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid to (want RFC3339)")
			return
		}
		utc := t.UTC()
		req.To = &utc
	}
	rows, err := store.SearchUsageSummary(tenant.SystemContext(r.Context()), req)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if rows == nil {
		rows = []models.UsageSummaryRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}
