package api

import (
	"encoding/csv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/finops"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
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

// GET /admin-api/usage/alerts?tenant_id=&agent_id=&limit=
// Returns recent budget_soft / budget_exceeded / budget_alert / budget_kill / budget_route audit rows.
func (s *Server) handleAdminUsageAlerts(w http.ResponseWriter, r *http.Request) {
	store, ok := s.store.(auditEventStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "usage alerts require a SQL state backend")
		return
	}
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	ctx := tenant.SystemContext(r.Context())
	codes := []string{policy.ReasonBudgetSoft, policy.ReasonBudgetExceeded, policy.ReasonBudgetAlert, policy.ReasonBudgetKill, "budget_route"}
	var out []*models.AuditEvent
	for _, code := range codes {
		req := models.AuditSearchRequest{
			TenantID:   q.Get("tenant_id"),
			AgentID:    q.Get("agent_id"),
			Action:     policy.StageRunCreate,
			ReasonCode: code,
			Limit:      limit,
		}
		rows, err := store.SearchAuditEvents(ctx, &req)
		if err != nil {
			handleStoreError(w, err)
			return
		}
		out = append(out, rows...)
	}
	// newest first (merge is rough; sort by TS desc)
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
	if len(out) > limit {
		out = out[:limit]
	}
	if out == nil {
		out = []*models.AuditEvent{}
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /admin-api/usage/export?tenant_id=&agent_id=&from=&to=&format=csv|json
func (s *Server) handleAdminUsageExport(w http.ResponseWriter, r *http.Request) {
	store, ok := s.usageEvents()
	if !ok {
		writeError(w, http.StatusNotImplemented, "usage export requires a SQL state backend")
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
	format := strings.ToLower(q.Get("format"))
	if format == "" {
		accept := r.Header.Get("Accept")
		if strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/csv") {
			format = "json"
		} else {
			format = "csv"
		}
	}
	if format == "json" {
		w.Header().Set("Content-Disposition", `attachment; filename="usage-export.json"`)
		writeJSON(w, http.StatusOK, rows)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="usage-export.csv"`)
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"day", "tenant_id", "agent_id", "tokens_in", "tokens_out", "usd_estimate", "run_count"})
	for _, row := range rows {
		_ = cw.Write([]string{
			row.Day,
			row.TenantID,
			row.AgentID,
			strconv.FormatInt(row.TokensIn, 10),
			strconv.FormatInt(row.TokensOut, 10),
			strconv.FormatFloat(row.USDEstimate, 'f', 6, 64),
			strconv.FormatInt(row.RunCount, 10),
		})
	}
	cw.Flush()
}

// GET /admin-api/usage/holds?tenant_id=&agent_id=
func (s *Server) handleAdminUsageHolds(w http.ResponseWriter, r *http.Request) {
	hs, ok := s.usageHolds()
	if !ok {
		writeError(w, http.StatusNotImplemented, "usage holds require a SQL state backend")
		return
	}
	q := r.URL.Query()
	tenantID := q.Get("tenant_id")
	agentID := q.Get("agent_id")
	since, until := finops.UTCDayWindow(time.Now().UTC())
	usd, tokens, count, err := hs.SumUsageHolds(tenant.SystemContext(r.Context()), tenantID, agentID, since, until)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, models.UsageHoldsSummary{Count: count, USDHold: usd, TokensHold: tokens})
}
