// Vector/semantic store HTTP surface. Mirrors store.go's shape
// (PUT/DELETE/POST-search over a simple
// resource), and the Store Dual Mode convention: the exact same handlers
// are mounted again under /internal/vectors/* (see server.go) so a
// non-Python or POSTGRES_DSN-less runner can reach it with its runner
// token instead of a client credential it may not have.
package api

import (
	"net/http"

	"github.com/getrunkite/runkite/internal/models"
)

// writeVectorStoreDisabled responds 501 (not 404) when no vector_store is
// configured -- "this feature isn't turned on" is a more actionable
// signal than "this route doesn't exist" for something opt-in.
func (s *Server) writeVectorStoreDisabled(w http.ResponseWriter) bool {
	if s.vectors == nil {
		writeError(w, http.StatusNotImplemented, "vector store not configured (see vector_store in langgraph.json)")
		return true
	}
	return false
}

// PUT /vectors/items
func (s *Server) handleUpsertVectorItem(w http.ResponseWriter, r *http.Request) {
	if s.writeVectorStoreDisabled(w) {
		return
	}
	var req models.VectorUpsertRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Namespace == "" || req.ID == "" {
		writeError(w, http.StatusBadRequest, "namespace and id are required")
		return
	}
	if len(req.Embedding) == 0 {
		writeError(w, http.StatusBadRequest, "embedding is required")
		return
	}

	item := &models.VectorItem{
		Namespace: req.Namespace,
		ID:        req.ID,
		Content:   req.Content,
		Embedding: req.Embedding,
		Metadata:  req.Metadata,
	}
	if item.Metadata == nil {
		item.Metadata = map[string]interface{}{}
	}
	if err := s.vectors.Upsert(r.Context(), item); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /vectors/items
func (s *Server) handleDeleteVectorItem(w http.ResponseWriter, r *http.Request) {
	if s.writeVectorStoreDisabled(w) {
		return
	}
	var req models.VectorDeleteRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Namespace == "" || req.ID == "" {
		writeError(w, http.StatusBadRequest, "namespace and id are required")
		return
	}
	if err := s.vectors.Delete(r.Context(), req.Namespace, req.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /vectors/search
func (s *Server) handleSearchVectors(w http.ResponseWriter, r *http.Request) {
	if s.writeVectorStoreDisabled(w) {
		return
	}
	var req models.VectorSearchRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Namespace == "" {
		writeError(w, http.StatusBadRequest, "namespace is required")
		return
	}
	if len(req.Embedding) == 0 {
		writeError(w, http.StatusBadRequest, "embedding is required")
		return
	}

	results, err := s.vectors.Search(r.Context(), &req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if results == nil {
		results = []*models.VectorSearchResult{}
	}
	writeJSON(w, http.StatusOK, models.VectorSearchResponse{Results: results})
}
