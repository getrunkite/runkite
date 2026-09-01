package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
)

// Opaque checkpoint proxy (runner-protocol §6.2). Framework-owned bytes;
// the control plane never parses the body.

const headerCheckpointFramework = "X-Runkite-Checkpoint-Framework"

// assertBoundThread returns false (and writes 4xx) when the call is not
// allowed for this path thread_id. Fail-closed: a missing run binding is
// rejected (same convention as connector session/MCP handlers), not
// allowed through. Production always attaches binding via middleware
// before these routes; tests that hit the handler directly must inject
// auth.WithRunBinding (see withTestCheckpointRunBinding).
func assertBoundThread(w http.ResponseWriter, r *http.Request, threadID string) bool {
	b := auth.RunBindingFromContext(r.Context())
	if b == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"message":     "run binding required",
			"reason_code": auth.ReasonRunBindingRequired,
		})
		return false
	}
	if b.ThreadID == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"message":     "run binding has empty thread_id",
			"reason_code": auth.ReasonRunThreadMismatch,
		})
		return false
	}
	if b.ThreadID != threadID {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"message":     "thread_id does not match run binding",
			"reason_code": auth.ReasonRunThreadMismatch,
		})
		return false
	}
	return true
}

func etagFromVersion(v int64) string {
	return fmt.Sprintf(`"%d"`, v)
}

// parseIfMatch returns (version, present, valid).
//   - absent / empty / "*" → present=false (unconditional PUT)
//   - strong "N" → present=true, valid=true
//   - weak W/"N" or garbage → present=true, valid=false (caller MUST 400;
//     treating these as absent would silently disable CAS)
func parseIfMatch(h string) (int64, bool, bool) {
	h = strings.TrimSpace(h)
	if h == "" || h == "*" {
		return 0, false, true
	}
	if len(h) >= 2 && (h[0] == 'W' || h[0] == 'w') && h[1] == '/' {
		return 0, true, false
	}
	h = strings.Trim(h, `"`)
	v, err := strconv.ParseInt(h, 10, 64)
	if err != nil || v < 1 {
		return 0, true, false
	}
	return v, true, true
}

// PUT /internal/checkpoints/{threadID}/{checkpointID}
func (s *Server) handlePutOpaqueCheckpoint(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")
	checkpointID := r.PathValue("checkpointID")
	if threadID == "" || checkpointID == "" {
		writeError(w, http.StatusBadRequest, "thread_id and checkpoint_id are required")
		return
	}
	if !assertBoundThread(w, r, threadID) {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, int64(models.MaxOpaqueCheckpointBytes)+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	if len(body) > models.MaxOpaqueCheckpointBytes {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("checkpoint exceeds %d byte limit", models.MaxOpaqueCheckpointBytes))
		return
	}

	if checkpointID == "latest" {
		// GET .../latest is a literal route; storing id "latest" would be
		// unreachable. LangGraph uses UUIDs — reject rather than shadow.
		writeError(w, http.StatusBadRequest, `checkpoint_id "latest" is reserved`)
		return
	}

	framework := strings.TrimSpace(r.Header.Get(headerCheckpointFramework))
	var ifMatch *int64
	ifNone := strings.TrimSpace(r.Header.Get("If-None-Match"))
	if v, present, valid := parseIfMatch(r.Header.Get("If-Match")); present {
		if !valid {
			writeError(w, http.StatusBadRequest, "If-Match must be a strong numeric ETag")
			return
		}
		if ifNone == "*" {
			writeError(w, http.StatusBadRequest, "If-Match and If-None-Match:* cannot both be set")
			return
		}
		ifMatch = &v
	} else if ifNone == "*" {
		// Create-only: used by proxy aput_writes when LangGraph records
		// pending writes for a checkpoint_id that aput has not created yet.
		z := state.OpaqueCreateOnly
		ifMatch = &z
	}

	ver, err := s.store.PutOpaqueCheckpoint(r.Context(), threadID, checkpointID, body, framework, ifMatch)
	if err != nil {
		if strings.Contains(err.Error(), "exceeds") {
			writeError(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		var conflict *state.ErrConflict
		if errors.As(err, &conflict) {
			// RFC 7232: failed If-Match / If-None-Match → 412.
			writeJSON(w, http.StatusPreconditionFailed, map[string]string{
				"message":     "checkpoint version mismatch",
				"reason_code": "checkpoint_version_mismatch",
			})
			return
		}
		handleStoreError(w, err)
		return
	}
	w.Header().Set("ETag", etagFromVersion(ver))
	w.WriteHeader(http.StatusNoContent)
}

// GET /internal/checkpoints/{threadID}/{checkpointID}
func (s *Server) handleGetOpaqueCheckpoint(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")
	checkpointID := r.PathValue("checkpointID")
	if threadID == "" || checkpointID == "" {
		writeError(w, http.StatusBadRequest, "thread_id and checkpoint_id are required")
		return
	}
	if !assertBoundThread(w, r, threadID) {
		return
	}

	cp, err := s.store.GetOpaqueCheckpoint(r.Context(), threadID, checkpointID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeOpaqueBlob(w, cp)
}

// GET /internal/checkpoints/{threadID}/latest?ns=
// One round-trip "newest blob for this namespace" for proxy aget_tuple when
// checkpoint_id is absent (avoids list + N get).
func (s *Server) handleGetLatestOpaqueCheckpoint(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")
	if threadID == "" {
		writeError(w, http.StatusBadRequest, "thread_id is required")
		return
	}
	if !assertBoundThread(w, r, threadID) {
		return
	}
	ns := r.URL.Query().Get("ns")

	cp, err := s.store.GetLatestOpaqueCheckpoint(r.Context(), threadID, ns)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	// Percent-encode: checkpoint_id may contain \x1f (ns separator) which is
	// illegal in raw HTTP header values.
	w.Header().Set("X-Runkite-Checkpoint-Id", url.PathEscape(cp.CheckpointID))
	writeOpaqueBlob(w, cp)
}

func writeOpaqueBlob(w http.ResponseWriter, cp *models.OpaqueCheckpoint) {
	if cp.Framework != "" {
		w.Header().Set(headerCheckpointFramework, cp.Framework)
	}
	w.Header().Set("ETag", etagFromVersion(cp.Version))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(cp.Data)
}

// GET /internal/checkpoints/{threadID}
func (s *Server) handleListOpaqueCheckpoints(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")
	if threadID == "" {
		writeError(w, http.StatusBadRequest, "thread_id is required")
		return
	}
	if !assertBoundThread(w, r, threadID) {
		return
	}

	limit := 0
	if lim := r.URL.Query().Get("limit"); lim != "" {
		if n, err := strconv.Atoi(lim); err == nil && n > 0 {
			limit = n
		}
	}

	items, err := s.store.ListOpaqueCheckpoints(r.Context(), threadID, limit)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	out := make([]map[string]interface{}, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]interface{}{
			"checkpoint_id": it.CheckpointID,
			"created_at":    it.CreatedAt.UTC().Format(time.RFC3339Nano),
			"framework":     it.Framework,
			"size_bytes":    it.SizeBytes,
			"version":       it.Version,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /internal/checkpoints/{threadID}/{checkpointID}
func (s *Server) handleDeleteOpaqueCheckpoint(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")
	checkpointID := r.PathValue("checkpointID")
	if threadID == "" || checkpointID == "" {
		writeError(w, http.StatusBadRequest, "thread_id and checkpoint_id are required")
		return
	}
	if !assertBoundThread(w, r, threadID) {
		return
	}

	if err := s.store.DeleteOpaqueCheckpoint(r.Context(), threadID, checkpointID); err != nil {
		var nf *state.ErrNotFound
		if errors.As(err, &nf) {
			writeError(w, http.StatusNotFound, "checkpoint not found")
			return
		}
		handleStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
