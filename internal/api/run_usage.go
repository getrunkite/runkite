package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/jsonvalue"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
)

func (s *Server) runUsageBinding(w http.ResponseWriter, r *http.Request) (state.RunUsageStore, state.RunUsageBinding, bool) {
	b := auth.RunBindingFromContext(r.Context())
	if b == nil || b.RunID != r.PathValue("runID") || b.RunIncarnationID == "" {
		writeError(w, http.StatusForbidden, "current durable run binding required")
		return nil, state.RunUsageBinding{}, false
	}
	run, err := s.store.GetRun(r.Context(), b.RunID)
	if err != nil {
		handleStoreError(w, err)
		return nil, state.RunUsageBinding{}, false
	}
	var manifest struct {
		RunnerKind string `json:"runner_kind"`
	}
	raw, _ := json.Marshal(run.Metadata["run_manifest"])
	_ = json.Unmarshal(raw, &manifest)
	isolated := manifest.RunnerKind == "python-isolated"
	if !isolated && s.executionPools != nil {
		pool, found := s.executionPools.Lookup(manifest.RunnerKind)
		isolated = found && pool.RunnerKind == "python-isolated"
	}
	if !isolated {
		writeError(w, http.StatusForbidden, "durable workload reports require the configured isolated profile")
		return nil, state.RunUsageBinding{}, false
	}
	store, ok := s.store.(state.RunUsageStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "durable usage reports require a supported SQL store")
		return nil, state.RunUsageBinding{}, false
	}
	w.Header().Set("Cache-Control", "no-store")
	return store, state.RunUsageBinding{RunCheckpointBinding: state.RunCheckpointBinding{RunID: b.RunID, ThreadID: b.ThreadID, Generation: b.Generation}, TenantID: b.TenantID, RunIncarnationID: b.RunIncarnationID}, true
}

func runUsageError(w http.ResponseWriter, err error) {
	var fence *state.ErrRunCheckpointFence
	if errors.As(err, &fence) {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "current run authority required", "reason_code": fence.ReasonCode})
		return
	}
	handleStoreError(w, err)
}

func (s *Server) handleGetRunUsage(w http.ResponseWriter, r *http.Request) {
	store, binding, ok := s.runUsageBinding(w, r)
	if !ok {
		return
	}
	snapshot, err := store.GetRunUsage(r.Context(), binding)
	if err != nil {
		runUsageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"snapshot": snapshot})
}

func (s *Server) handleReportRunUsage(w http.ResponseWriter, r *http.Request) {
	store, binding, ok := s.runUsageBinding(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil || len(raw) > 4096 {
		writeError(w, http.StatusBadRequest, "usage report exceeds limit or is unreadable")
		return
	}
	decoded, err := jsonvalue.DecodeUnique(raw)
	fields, object := decoded.(map[string]interface{})
	allowed := map[string]bool{"schema_version": true, "sequence": true, "status": true, "complete": true, "prompt_tokens": true, "completion_tokens": true, "cost_usd": true, "model": true}
	valid := err == nil && object
	for field := range fields {
		valid = valid && allowed[field] && fields[field] != nil
	}
	for _, required := range []string{"schema_version", "sequence", "status", "complete"} {
		_, present := fields[required]
		valid = valid && present && fields[required] != nil
	}
	var report models.RunUsageReport
	if !valid || json.Unmarshal(raw, &report) != nil || report.Validate() != nil {
		writeError(w, http.StatusBadRequest, "invalid cumulative usage report")
		return
	}
	if report.Status == models.RunUsageUnknown && len(fields) != 4 {
		writeError(w, http.StatusBadRequest, "unknown usage cannot contain reported values")
		return
	}
	snapshot, applied, err := store.ReportRunUsage(r.Context(), binding, report)
	if err != nil {
		runUsageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"snapshot": snapshot, "applied": applied})
}
