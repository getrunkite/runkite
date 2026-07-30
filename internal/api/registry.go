// Package api: Agent marketplace / registry -- publish, discover, and
// deploy agent definitions. Deliberately scoped to a "minimal viable
// registry": a metadata catalog with publish/search/get/version-history,
// no security review workflow, and no automatic clone-and-execute
// deploy pipeline. A RegistryEntry's source_ref (a git URL, a plain
// URL, or an inline langgraph.json snippet) is where a human -- or
// their own separate tooling -- goes to actually deploy it, same as a
// package registry's listing page versus its install step. Running
// arbitrary fetched code is a fundamentally different trust/sandboxing
// problem than a searchable catalog, and out of scope here.
package api

import (
	"net/http"
	"strconv"

	"github.com/sharanharsoor/runkite/internal/models"
)

// PUT /registry/entries/{name} -- publish a new entry or a new version
// of an existing one. Same version-bump-on-actual-change semantics as
// UpsertAgent (see models.RegistryEntry's doc comment).
func (s *Server) handlePublishRegistryEntry(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var entry models.RegistryEntry
	if err := readJSON(r, &entry); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	entry.Name = name // path is authoritative, not whatever (if anything) the body claims
	if entry.SourceType == "" || entry.SourceRef == "" {
		writeError(w, http.StatusBadRequest, "source_type and source_ref are required")
		return
	}

	if err := s.store.PublishRegistryEntry(r.Context(), &entry); err != nil {
		handleStoreError(w, err)
		return
	}
	published, err := s.store.GetRegistryEntry(r.Context(), name)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, published)
}

// GET /registry/entries/{name}
func (s *Server) handleGetRegistryEntry(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	entry, err := s.store.GetRegistryEntry(r.Context(), name)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// DELETE /registry/entries/{name}
func (s *Server) handleDeleteRegistryEntry(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.store.DeleteRegistryEntry(r.Context(), name); err != nil {
		handleStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /registry/search
func (s *Server) handleSearchRegistryEntries(w http.ResponseWriter, r *http.Request) {
	var req models.RegistrySearchRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Limit = clampSearchLimit(req.Limit, 10)
	entries, err := s.store.SearchRegistryEntries(r.Context(), &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// GET /registry/entries/{name}/versions
func (s *Server) handleListRegistryEntryVersions(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	versions, err := s.store.ListRegistryEntryVersions(r.Context(), name)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, versions)
}

// GET /registry/entries/{name}/versions/{version}
func (s *Server) handleGetRegistryEntryVersion(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	version, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "version must be an integer")
		return
	}
	v, err := s.store.GetRegistryEntryVersion(r.Context(), name, version)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
