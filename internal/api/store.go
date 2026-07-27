package api

import (
	"net/http"
	"strings"

	"github.com/runkite/runkite/internal/models"
)

// parseNamespaceParam parses the namespace query parameter. The LangGraph SDK
// sends dot-separated ("sdk.test"), while the native format uses commas
// ("sdk,test"). Accept both: if the value contains commas, split on commas
// (native); otherwise split on dots (SDK compat).
//
// ponytail: known limitation — a single-segment namespace containing a literal
// dot (e.g. ["v1.2.3"]) sent as namespace=v1.2.3 is ambiguous and gets split
// into three segments. The SDK itself forbids dots in labels, so this only
// bites non-SDK callers using dots in segment names. Fix path: accept
// namespace as a JSON-encoded query param, or always require commas.
func parseNamespaceParam(s string) []string {
	if strings.Contains(s, ",") {
		return strings.Split(s, ",")
	}
	return strings.Split(s, ".")
}

// PUT /store/items
func (s *Server) handlePutItem(w http.ResponseWriter, r *http.Request) {
	var req models.StorePutRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	item := &models.StoreItem{
		Namespace:  req.Namespace,
		Key:        req.Key,
		Value:      req.Value,
		TTLMinutes: req.TTLMinutes,
	}
	if item.Namespace == nil {
		item.Namespace = []string{}
	}

	if err := s.store.PutItem(r.Context(), item); err != nil {
		handleStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /store/items?namespace=...&key=...&refresh_ttl=...
func (s *Server) handleGetItem(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "key query parameter is required")
		return
	}

	var namespace []string
	nsParam := r.URL.Query().Get("namespace")
	if nsParam != "" {
		namespace = parseNamespaceParam(nsParam)
	}

	// Default true to match LangGraph BaseStore's own TTLConfig default
	// (refresh_on_read=True) -- only an explicit "false" opts out.
	refreshTTL := r.URL.Query().Get("refresh_ttl") != "false"

	item, err := s.store.GetItem(r.Context(), namespace, key, refreshTTL)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// DELETE /store/items
func (s *Server) handleDeleteItem(w http.ResponseWriter, r *http.Request) {
	var req models.StoreDeleteRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	if err := s.store.DeleteItem(r.Context(), req.Namespace, req.Key); err != nil {
		handleStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /store/items/search
func (s *Server) handleSearchItems(w http.ResponseWriter, r *http.Request) {
	var req models.StoreSearchRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}

	items, err := s.store.SearchItems(r.Context(), &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, models.StoreSearchResponse{Items: items})
}

// POST /store/namespaces
func (s *Server) handleListNamespaces(w http.ResponseWriter, r *http.Request) {
	var req models.StoreListNamespacesRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Limit <= 0 {
		req.Limit = 100
	}

	namespaces, err := s.store.ListNamespaces(r.Context(), &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if namespaces == nil {
		namespaces = [][]string{}
	}
	writeJSON(w, http.StatusOK, namespaces)
}
