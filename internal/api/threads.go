package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/runkite/runkite/internal/models"
	"github.com/runkite/runkite/internal/state"
)

// POST /threads
func (s *Server) handleCreateThread(w http.ResponseWriter, r *http.Request) {
	var req models.ThreadCreate
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.ThreadID == "" {
		req.ThreadID = uuid.New().String()
	}
	if req.IfExists == "" {
		req.IfExists = "raise"
	}

	now := time.Now().UTC()
	thread := &models.Thread{
		ThreadID:  req.ThreadID,
		Status:    models.ThreadStatusIdle,
		Metadata:  req.Metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if thread.Metadata == nil {
		thread.Metadata = map[string]interface{}{}
	}

	err := s.store.CreateThread(r.Context(), thread)
	if err != nil {
		// Handle if_exists=do_nothing
		if req.IfExists == "do_nothing" {
			existing, getErr := s.store.GetThread(r.Context(), req.ThreadID)
			if getErr == nil {
				writeJSON(w, http.StatusOK, existing)
				return
			}
		}
		handleStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, thread)
}

// GET /threads/{threadID}
func (s *Server) handleGetThread(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")

	thread, err := s.store.GetThread(r.Context(), threadID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, thread)
}

// DELETE /threads/{threadID}
func (s *Server) handleDeleteThread(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")

	if err := s.store.DeleteThread(r.Context(), threadID); err != nil {
		handleStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PATCH /threads/{threadID}
func (s *Server) handlePatchThread(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")

	var patch models.ThreadPatch
	if err := readJSON(r, &patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	updated, err := s.store.UpdateThread(r.Context(), threadID, &patch)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	// AP-019: updating values creates a new history entry, same as
	// POST /threads/{id}/state.
	if len(patch.Values) > 0 {
		s.saveRunCheckpoint(r.Context(), threadID, updated.Values)
	}

	writeJSON(w, http.StatusOK, updated)
}

// POST /threads/search
func (s *Server) handleSearchThreads(w http.ResponseWriter, r *http.Request) {
	var req models.ThreadSearchRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}

	threads, err := s.store.SearchThreads(r.Context(), &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if threads == nil {
		threads = []*models.Thread{}
	}
	writeJSON(w, http.StatusOK, threads)
}

// POST /threads/{threadID}/copy
func (s *Server) handleCopyThread(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")

	original, err := s.store.GetThread(r.Context(), threadID)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	now := time.Now().UTC()
	copy := &models.Thread{
		ThreadID:  uuid.New().String(),
		Status:    original.Status,
		Metadata:  original.Metadata,
		Values:    original.Values,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.store.CreateThread(r.Context(), copy); err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, copy)
}

// GET /threads/{threadID}/state
func (s *Server) handleGetThreadState(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")

	ts, err := s.store.GetLatestCheckpoint(r.Context(), threadID)
	if err != nil {
		// No checkpoints yet — return empty state from thread values
		var notFound *state.ErrNotFound
		if errors.As(err, &notFound) {
			thread, threadErr := s.store.GetThread(r.Context(), threadID)
			if threadErr != nil {
				handleStoreError(w, threadErr)
				return
			}
			empty := &models.ThreadState{
				Values: thread.Values,
				Next:   []string{},
				Checkpoint: models.ThreadCheckpoint{
					ThreadID:     threadID,
					CheckpointNS: "",
					CheckpointID: "",
				},
				Metadata:   thread.Metadata,
				Tasks:      []interface{}{},
				Interrupts: []interface{}{},
			}
			writeJSON(w, http.StatusOK, empty)
			return
		}
		handleStoreError(w, err)
		return
	}
	ensureNonNilSlices(ts)
	writeJSON(w, http.StatusOK, ts)
}

// POST /threads/{threadID}/state
func (s *Server) handleUpdateThreadState(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")

	var req struct {
		Values       map[string]interface{} `json:"values"`
		AsNode       string                 `json:"as_node,omitempty"`
		CheckpointID string                 `json:"checkpoint_id,omitempty"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Verify thread exists
	thread, err := s.store.GetThread(r.Context(), threadID)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	// Merge new values into existing thread values
	merged := make(map[string]interface{})
	for k, v := range thread.Values {
		merged[k] = v
	}
	for k, v := range req.Values {
		merged[k] = v
	}

	// Find parent checkpoint
	var parentCP *models.ThreadCheckpoint
	latest, latestErr := s.store.GetLatestCheckpoint(r.Context(), threadID)
	if latestErr == nil {
		cp := latest.Checkpoint
		parentCP = &cp
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	cpID := uuid.New().String()
	ts := &models.ThreadState{
		Values: merged,
		Next:   []string{},
		Checkpoint: models.ThreadCheckpoint{
			ThreadID:     threadID,
			CheckpointNS: "",
			CheckpointID: cpID,
		},
		Metadata:         thread.Metadata,
		CreatedAt:        &now,
		ParentCheckpoint: parentCP,
		Tasks:            []interface{}{},
		Interrupts:       []interface{}{},
	}

	if err := s.store.SaveCheckpoint(r.Context(), threadID, ts); err != nil {
		handleStoreError(w, err)
		return
	}

	// Update thread values
	s.store.UpdateThread(r.Context(), threadID, &models.ThreadPatch{Values: merged})

	writeJSON(w, http.StatusOK, models.ThreadUpdateStateResponse{
		Checkpoint: ts.Checkpoint,
	})
}

// GET /threads/{threadID}/history  (backwards compat, supports ?limit=&before=)
// POST /threads/{threadID}/history (SDK uses POST with JSON body)
func (s *Server) handleGetThreadHistory(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")

	limit := 10
	var before string

	if r.Method == http.MethodPost && r.Body != nil {
		// POST: limit/before in JSON body
		var req models.ThreadHistoryRequest
		if readJSON(r, &req) == nil {
			if req.Limit > 0 {
				limit = req.Limit
			}
			if cp, ok := req.Before["checkpoint_id"]; ok {
				if s, ok := cp.(string); ok {
					before = s
				}
			}
		}
	} else {
		// GET: limit/before as query params
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		before = r.URL.Query().Get("before")
	}

	states, err := s.store.ListCheckpoints(r.Context(), threadID, limit, before)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if states == nil {
		states = []*models.ThreadState{}
	}
	for _, ts := range states {
		ensureNonNilSlices(ts)
	}
	writeJSON(w, http.StatusOK, states)
}

// ensureNonNilSlices makes sure JSON marshals [] not null for slice fields.
func ensureNonNilSlices(ts *models.ThreadState) {
	if ts.Next == nil {
		ts.Next = []string{}
	}
	if ts.Tasks == nil {
		ts.Tasks = []interface{}{}
	}
	if ts.Interrupts == nil {
		ts.Interrupts = []interface{}{}
	}
	if ts.Values == nil {
		ts.Values = map[string]interface{}{}
	}
	if ts.Metadata == nil {
		ts.Metadata = map[string]interface{}{}
	}
}
