// Package qdrant implements vectorstore.VectorStore using Qdrant's REST
// API -- the project's non-SQL exemplar for vector
// backends, same role Mongo plays for state.Store -- proof the
// vectorstore.VectorStore interface is implementable against a real
// standalone vector database, not just pgvector's SQL extension).
//
// Plain net/http against Qdrant's REST API rather than the official
// gRPC client (github.com/qdrant/go-client): Qdrant's REST surface is
// small, stable, and documented directly as JSON -- pulling in a
// generated gRPC client for four HTTP calls would be a heavier
// dependency than the problem needs.
//
// One collection holds every tenant/namespace (not one collection per
// tenant or namespace): tenant_id and namespace are stored as payload
// fields and included in every filter, the same "always scope by
// tenant_id/namespace in the WHERE clause" shape pgvector uses. Qdrant
// point IDs must be an unsigned integer or a UUID -- never an arbitrary
// string -- so a caller's (tenant_id, namespace, id) is mapped to a
// deterministic UUID v5 (same triple always produces the same point ID,
// making upsert-by-id and delete-by-id idempotent) while the original
// three values are kept in the payload for retrieval.
package qdrant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

// pointNamespace is a fixed, arbitrary UUID used as the "namespace" for
// UUID v5 point-ID derivation (uuid.NewSHA1 needs one) -- any fixed UUID
// works here, it just has to be the same one on every run so the same
// (tenant_id, namespace, id) triple always maps to the same point ID.
var pointNamespace = uuid.MustParse("6f6a6f4e-2a6b-4b8e-9c1a-6f6a6f4e2a6b")

func pointID(tenantID, namespace, id string) string {
	return uuid.NewSHA1(pointNamespace, []byte(tenantID+"\x00"+namespace+"\x00"+id)).String()
}

// Store implements vectorstore.VectorStore against a Qdrant instance.
type Store struct {
	baseURL    string
	collection string
	dimensions int
	client     *http.Client
}

// New creates a Qdrant-backed store. dimensions fixes the collection's
// vector size at creation time, same fixed-width convention as
// pgvector's vector(N) column -- every item upserted must supply
// exactly this many floats, checked before any HTTP call is made.
func New(baseURL, collection string, dimensions int) (*Store, error) {
	if dimensions <= 0 {
		return nil, fmt.Errorf("qdrant: dimensions must be > 0, got %d", dimensions)
	}
	if collection == "" {
		collection = "vector_items"
	}
	return &Store{
		baseURL:    baseURL,
		collection: collection,
		dimensions: dimensions,
		// Bound every call: a hung/unreachable Qdrant must not hang the
		// control plane's /vectors/* handlers forever. 30s covers large
		// ANN searches without pretending the request can wait forever.
		client: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (s *Store) url(path string) string {
	return s.baseURL + "/collections/" + s.collection + path
}

func (s *Store) do(ctx context.Context, method, path string, body interface{}) (map[string]interface{}, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("qdrant: marshal request: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.url(path), reader)
	if err != nil {
		return nil, fmt.Errorf("qdrant: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qdrant: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("qdrant: read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("qdrant: %s %s returned %d: %s", method, path, resp.StatusCode, string(respBody))
	}
	if len(respBody) == 0 {
		return nil, nil
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("qdrant: parse response: %w", err)
	}
	return parsed, nil
}

// Init creates the collection if it doesn't already exist. Safe to call
// on every startup -- Qdrant's create-collection endpoint 409s on an
// existing collection, which is treated as success (same idempotent,
// non-versioned convention as every other backend's Init).
//
// Known limitation, same shape as pgvector's: the collection's vector
// size is fixed the first time it's created. Changing
// vector_store.dimensions in langgraph.json after the collection
// already exists does not migrate it -- Upsert starts failing loudly
// with a clear Qdrant dimension-mismatch error until the collection is
// manually dropped or recreated with the new size.
func (s *Store) Init(ctx context.Context) error {
	body := map[string]interface{}{
		"vectors": map[string]interface{}{
			"size":     s.dimensions,
			"distance": "Cosine",
		},
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.baseURL+"/collections/"+s.collection, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("qdrant: build create-collection request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("qdrant: create collection: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	// 200 = created; 409 = already exists, both fine. Anything else is
	// a real failure (e.g. Qdrant unreachable, bad request).
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("qdrant: create collection returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (s *Store) Close() error {
	// No persistent connection to close -- s.client is a plain
	// *http.Client, not a pool.
	return nil
}

// Upsert's created_at is a known, stated limitation: Qdrant has no
// built-in insert-vs-update distinction the way Postgres's ON CONFLICT
// DO UPDATE does, so pinning down "first write time" would need a read
// before every write to check whether the point already exists.
// created_at is set to "now" on every Upsert call, including a
// re-index of an already-existing id -- correct for a fresh item, but
// re-indexing an existing document resets its created_at rather than
// preserving the original. Upgrade path: a GET on the point's id before
// the upsert, reusing its existing created_at when present.
func (s *Store) Upsert(ctx context.Context, item *models.VectorItem) error {
	if len(item.Embedding) != s.dimensions {
		return fmt.Errorf("qdrant: embedding has %d dimensions, store configured for %d", len(item.Embedding), s.dimensions)
	}
	tenantID := tenant.FromContext(ctx)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	payload := map[string]interface{}{
		"tenant_id":  tenantID,
		"namespace":  item.Namespace,
		"id":         item.ID,
		"content":    item.Content,
		"created_at": now,
		"updated_at": now,
	}
	for k, v := range item.Metadata {
		payload["meta_"+k] = v
	}

	body := map[string]interface{}{
		"points": []map[string]interface{}{
			{
				"id":      pointID(tenantID, item.Namespace, item.ID),
				"vector":  item.Embedding,
				"payload": payload,
			},
		},
	}
	_, err := s.do(ctx, http.MethodPut, "/points?wait=true", body)
	return err
}

func (s *Store) Delete(ctx context.Context, namespace, id string) error {
	tenantID := tenant.FromContext(ctx)
	body := map[string]interface{}{
		"points": []string{pointID(tenantID, namespace, id)},
	}
	_, err := s.do(ctx, http.MethodPost, "/points/delete?wait=true", body)
	return err
}

func (s *Store) Search(ctx context.Context, req *models.VectorSearchRequest) ([]*models.VectorSearchResult, error) {
	if len(req.Embedding) != s.dimensions {
		return nil, fmt.Errorf("qdrant: query embedding has %d dimensions, store configured for %d", len(req.Embedding), s.dimensions)
	}
	topK := req.TopK
	if topK <= 0 {
		topK = 10
	}

	must := []map[string]interface{}{
		{"key": "tenant_id", "match": map[string]interface{}{"value": tenant.FromContext(ctx)}},
		{"key": "namespace", "match": map[string]interface{}{"value": req.Namespace}},
	}
	for k, v := range req.Filter {
		must = append(must, map[string]interface{}{"key": "meta_" + k, "match": map[string]interface{}{"value": v}})
	}

	body := map[string]interface{}{
		"vector":       req.Embedding,
		"limit":        topK,
		"filter":       map[string]interface{}{"must": must},
		"with_payload": true,
	}
	resp, err := s.do(ctx, http.MethodPost, "/points/search", body)
	if err != nil {
		return nil, err
	}

	rawResults, _ := resp["result"].([]interface{})
	// Non-nil so a no-results search JSON-encodes to [] rather than
	// null -- SDK clients call .map() on it unconditionally (see
	// internal/state/conformance's empty_list_results_are_not_nil for
	// the same contract enforced on the state store side).
	results := []*models.VectorSearchResult{}
	for _, raw := range rawResults {
		hit, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		payload, _ := hit["payload"].(map[string]interface{})
		item := &models.VectorItem{
			Namespace: stringField(payload, "namespace"),
			ID:        stringField(payload, "id"),
			Content:   stringField(payload, "content"),
			CreatedAt: timeField(payload, "created_at"),
			UpdatedAt: timeField(payload, "updated_at"),
			Metadata:  map[string]interface{}{},
		}
		for k, v := range payload {
			if rest, ok := trimMetaPrefix(k); ok {
				item.Metadata[rest] = v
			}
		}
		score, _ := hit["score"].(float64)
		results = append(results, &models.VectorSearchResult{Item: item, Score: score})
	}
	return results, nil
}

func stringField(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func timeField(m map[string]interface{}, key string) time.Time {
	if v, ok := m[key].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

const metaPrefix = "meta_"

func trimMetaPrefix(key string) (string, bool) {
	if len(key) > len(metaPrefix) && key[:len(metaPrefix)] == metaPrefix {
		return key[len(metaPrefix):], true
	}
	return "", false
}
