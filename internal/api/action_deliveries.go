package api

import (
	"net/http"
	"strconv"

	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func (s *Server) SetActionExportSinkID(id string) { s.actionExportSinkID = id }
func (s *Server) handleAdminActionDeliveries(w http.ResponseWriter, r *http.Request) {
	store, ok := s.store.(state.ActionDeliveryStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "action delivery metadata requires SQL")
		return
	}
	t := r.URL.Query().Get("tenant_id")
	if t == "" {
		writeError(w, http.StatusBadRequest, "explicit tenant_id is required")
		return
	}
	q := r.URL.Query()
	limit, offset := 50, 0
	var err error
	if q.Get("limit") != "" {
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil {
			writeError(w, 400, "invalid limit")
			return
		}
	}
	if q.Get("offset") != "" {
		offset, err = strconv.Atoi(q.Get("offset"))
		if err != nil {
			writeError(w, 400, "invalid offset")
			return
		}
	}
	if limit < 1 || limit > 100 || offset < 0 || offset > 10000 {
		writeError(w, 400, "invalid pagination")
		return
	}
	rows, err := store.SearchActionDeliveries(tenant.ScopedContext(r.Context(), t), &state.ActionDeliverySearchRequest{TenantID: t, SinkID: q.Get("sink_id"), Status: q.Get("status"), Limit: limit, Offset: offset})
	if err != nil {
		writeError(w, 400, "action delivery search unavailable or invalid")
		return
	}
	writeJSON(w, 200, rows)
}
func (s *Server) handleAdminRetryActionDelivery(w http.ResponseWriter, r *http.Request) {
	store, ok := s.store.(state.ActionDeliveryStore)
	if !ok {
		writeError(w, 501, "action delivery metadata requires SQL")
		return
	}
	t := r.URL.Query().Get("tenant_id")
	if t == "" {
		writeError(w, 400, "explicit tenant_id is required")
		return
	}
	if !reviewEmptyDecisionBody(w, r) {
		return
	}
	if err := store.RetryActionDelivery(tenant.ScopedContext(r.Context(), t), t, r.PathValue("id")); err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "pending", "message": "Evidence delivery queued; no tool operation was retried"})
}
