// Package pinecone implements vectorstore.VectorStore using Pinecone's
// REST API. Closes the last remaining "Tier 2, not yet built" vector
// backend -- see internal/vectorstore/qdrant's package doc for the
// shared design principles (plain HTTP rather than a generated client,
// no vendored SDK for a small documented surface) this package follows
// too, though Pinecone's own shape differs enough from Qdrant/Weaviate
// that several of their specific conventions don't carry over -- see
// below.
//
// Pinecone splits its API into a control plane (index lifecycle:
// create/describe/delete -- always at one fixed host) and a data plane
// (vectors/upsert, query, vectors/delete -- served at a PER-INDEX host
// only known after the index is created or described). Init resolves
// and caches that per-index host once; every data-plane call after that
// reuses it directly rather than re-resolving on every request.
//
// Unlike Qdrant and Weaviate, Pinecone needs neither a derived UUID
// point/object ID nor a JSON-encoded metadata workaround: vector IDs
// accept arbitrary strings, and metadata is natively a freeform
// key-value object (any type, no schema declaration), so a caller's
// Metadata map is stored directly rather than serialized into a single
// blob field. Pinecone also has first-class namespaces as a real
// partitioning primitive (unlike Qdrant/Weaviate, where "namespace" is
// just another payload/property field this package invented) -- a
// caller's Namespace maps directly onto Pinecone's own namespace
// parameter, not onto a metadata field. tenant_id still goes into
// metadata and into every query's filter (there's no second dimension
// of Pinecone-native partitioning to spend on it), and a vector's ID is
// tenantID+"\x00"+item.ID -- namespaces alone aren't enough to keep two
// tenants' same-namespace, same-id items from colliding, since both
// would otherwise map to the exact same (namespace, id) pair.
//
// One consequence of a genuinely freeform, natively-queryable metadata
// object: Search's Filter is pushed straight into Pinecone's own
// "$eq"-based filter alongside the tenant_id scope, unlike Weaviate's
// over-fetch-then-filter-in-Go workaround -- Pinecone can filter inside
// its own metadata object natively, there's no JSON-blob-string problem
// here to work around.
package pinecone

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

// Store implements vectorstore.VectorStore against a Pinecone index.
type Store struct {
	controlURL string
	apiKey     string
	apiVersion string
	indexName  string
	dimensions int
	cloud      string
	region     string
	client     *http.Client

	mu      sync.RWMutex
	dataURL string // resolved by Init, e.g. "http://localhost:5081" or "https://<index>.svc...pinecone.io"
}

// New creates a Pinecone-backed store. apiKey is sent on every request
// but is ignored entirely by Pinecone Local (any value works there,
// per Pinecone's own docs) -- a real Pinecone account requires a real
// key. cloud/region only matter for a real serverless index's actual
// placement; Pinecone Local accepts (and ignores) any value here too,
// but the field is structurally required by the create-index request
// either way. Both default to "aws"/"us-east-1" (Pinecone's own
// documented example values) if left empty.
func New(controlURL, apiKey, indexName string, dimensions int, cloud, region string) (*Store, error) {
	if dimensions <= 0 {
		return nil, fmt.Errorf("pinecone: dimensions must be > 0, got %d", dimensions)
	}
	if indexName == "" {
		indexName = "vector-items"
	}
	if cloud == "" {
		cloud = "aws"
	}
	if region == "" {
		region = "us-east-1"
	}
	return &Store{
		controlURL: strings.TrimSuffix(controlURL, "/"),
		apiKey:     apiKey,
		// 2025-10 is the version confirmed live against Pinecone Local's
		// own documented curl examples; not otherwise user-configurable
		// since this package targets one fixed request/response shape.
		apiVersion: "2025-10",
		indexName:  indexName,
		dimensions: dimensions,
		cloud:      cloud,
		region:     region,
		// Bound every call: a hung/unreachable Pinecone must not hang
		// the control plane's /vectors/* handlers forever. 30s covers
		// large ANN searches without pretending the request can wait
		// forever -- same bound and reasoning as Qdrant's/Weaviate's.
		client: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (s *Store) Close() error {
	// No persistent connection to close -- s.client is a plain
	// *http.Client, not a pool.
	return nil
}

// vectorID combines tenant and item ID -- see this package's own doc
// comment for why namespace alone (Pinecone's real partitioning
// primitive here) isn't enough to keep two tenants' same-namespace,
// same-id items apart.
func vectorID(tenantID, id string) string {
	return tenantID + "\x00" + id
}

// Init resolves and caches this store's per-index data-plane host,
// creating the index first if it doesn't already exist. Safe to call on
// every startup -- a 404 from describe is treated as "doesn't exist
// yet, create it," anything else describe returns (including a
// successful 200) is treated as already there, same idempotent,
// non-versioned convention as every other backend's Init.
//
// Known limitation, same shape as pgvector's/Qdrant's/Weaviate's: the
// index's dimension is fixed the first time it's actually created.
// Changing vector_store.dimensions in langgraph.json after the index
// already exists does not migrate it -- Upsert/Search start failing
// loudly with Pinecone's own dimension-mismatch error until the index
// is manually deleted or recreated at the new width.
func (s *Store) Init(ctx context.Context) error {
	host, err := s.describeIndexHost(ctx)
	if err != nil {
		if !isNotFoundErr(err) {
			return err
		}
		if err := s.createIndex(ctx); err != nil {
			return err
		}
		host, err = s.describeIndexHost(ctx)
		if err != nil {
			return fmt.Errorf("pinecone: describe index after create: %w", err)
		}
	}

	// The data-plane host Pinecone returns is bare (no scheme) --
	// inferred from the control-plane URL's own scheme, since a real
	// account (https://api.pinecone.io) and Pinecone Local
	// (http://localhost:5080) both need their data-plane host reached
	// the same way (TLS or not) as the control plane itself.
	scheme := "https"
	if strings.HasPrefix(s.controlURL, "http://") {
		scheme = "http"
	}
	s.mu.Lock()
	s.dataURL = scheme + "://" + host
	s.mu.Unlock()
	return nil
}

type notFoundErr struct{ status int }

func (e *notFoundErr) Error() string { return fmt.Sprintf("pinecone: not found (status %d)", e.status) }

func isNotFoundErr(err error) bool {
	_, ok := err.(*notFoundErr)
	return ok
}

func (s *Store) newRequest(ctx context.Context, method, url string, body interface{}) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("pinecone: marshal request: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("pinecone: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pinecone-Api-Version", s.apiVersion)
	if s.apiKey != "" {
		req.Header.Set("Api-Key", s.apiKey)
	}
	return req, nil
}

func (s *Store) describeIndexHost(ctx context.Context) (string, error) {
	req, err := s.newRequest(ctx, http.MethodGet, s.controlURL+"/indexes/"+s.indexName, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("pinecone: describe index: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("pinecone: read describe-index response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", &notFoundErr{status: resp.StatusCode}
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("pinecone: describe index returned %d: %s", resp.StatusCode, string(body))
	}
	var parsed struct {
		Host string `json:"host"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("pinecone: parse describe-index response: %w", err)
	}
	return parsed.Host, nil
}

func (s *Store) createIndex(ctx context.Context) error {
	body := map[string]interface{}{
		"name":        s.indexName,
		"vector_type": "dense",
		"dimension":   s.dimensions,
		"metric":      "cosine",
		"spec": map[string]interface{}{
			"serverless": map[string]interface{}{
				"cloud":  s.cloud,
				"region": s.region,
			},
		},
		"deletion_protection": "disabled",
	}
	req, err := s.newRequest(ctx, http.MethodPost, s.controlURL+"/indexes", body)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("pinecone: create index: %w", err)
	}
	defer resp.Body.Close()
	// 201 = created; 409 (already exists, a real account's own
	// response for a race with another process) is also fine -- the
	// caller (Init) only reaches here after describe already returned
	// not-found, but a concurrent creator elsewhere could still win the
	// race between that check and this call.
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusConflict {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pinecone: create index returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (s *Store) dataRequest(ctx context.Context, path string, body interface{}) (map[string]interface{}, error) {
	s.mu.RLock()
	dataURL := s.dataURL
	s.mu.RUnlock()
	if dataURL == "" {
		return nil, fmt.Errorf("pinecone: store not initialized -- call Init first")
	}
	req, err := s.newRequest(ctx, http.MethodPost, dataURL+path, body)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pinecone: request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("pinecone: read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("pinecone: %s returned %d: %s", path, resp.StatusCode, string(respBody))
	}
	// vectors/delete's success body is the literal JSON string `""`,
	// not an object -- confirmed live against Pinecone Local. Callers
	// that don't need a parsed body (Delete) never look at the
	// returned map, so a non-object success response is fine to treat
	// the same as an empty one.
	trimmed := bytes.TrimSpace(respBody)
	if len(trimmed) == 0 || string(trimmed) == `""` {
		return nil, nil
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("pinecone: parse response: %w", err)
	}
	return parsed, nil
}

// Upsert is a true upsert in one round trip -- Pinecone's own
// vectors/upsert contract overwrites an existing ID's record entirely,
// unlike Weaviate (needing delete-then-create) or Qdrant's own
// insert-vs-update ambiguity. Same known, stated limitation as both of
// those regardless: created_at is set to "now" on every call, including
// re-indexing an already-existing id, since there's no partial-update
// path here either without a read before every write.
func (s *Store) Upsert(ctx context.Context, item *models.VectorItem) error {
	if len(item.Embedding) != s.dimensions {
		return fmt.Errorf("pinecone: embedding has %d dimensions, store configured for %d", len(item.Embedding), s.dimensions)
	}
	tenantID := tenant.FromContext(ctx)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	metadata := map[string]interface{}{
		"tenant_id":  tenantID,
		"item_id":    item.ID,
		"content":    item.Content,
		"created_at": now,
		"updated_at": now,
	}
	for k, v := range item.Metadata {
		metadata[k] = v
	}

	body := map[string]interface{}{
		"namespace": item.Namespace,
		"vectors": []map[string]interface{}{
			{
				"id":       vectorID(tenantID, item.ID),
				"values":   item.Embedding,
				"metadata": metadata,
			},
		},
	}
	_, err := s.dataRequest(ctx, "/vectors/upsert", body)
	return err
}

func (s *Store) Delete(ctx context.Context, namespace, id string) error {
	body := map[string]interface{}{
		"namespace": namespace,
		"ids":       []string{vectorID(tenant.FromContext(ctx), id)},
	}
	_, err := s.dataRequest(ctx, "/vectors/delete", body)
	return err
}

func (s *Store) Search(ctx context.Context, req *models.VectorSearchRequest) ([]*models.VectorSearchResult, error) {
	if len(req.Embedding) != s.dimensions {
		return nil, fmt.Errorf("pinecone: query embedding has %d dimensions, store configured for %d", len(req.Embedding), s.dimensions)
	}
	topK := req.TopK
	if topK <= 0 {
		topK = 10
	}

	filter := map[string]interface{}{
		"tenant_id": map[string]interface{}{"$eq": tenant.FromContext(ctx)},
	}
	for k, v := range req.Filter {
		filter[k] = map[string]interface{}{"$eq": v}
	}

	body := map[string]interface{}{
		"namespace":       req.Namespace,
		"vector":          req.Embedding,
		"topK":            topK,
		"filter":          filter,
		"includeMetadata": true,
		"includeValues":   false,
	}
	resp, err := s.dataRequest(ctx, "/query", body)
	if err != nil {
		return nil, err
	}

	rawMatches, _ := resp["matches"].([]interface{})
	// Non-nil so a no-results search JSON-encodes to [] rather than
	// null -- SDK clients call .map() on it unconditionally (same
	// contract as Qdrant's/Weaviate's).
	results := []*models.VectorSearchResult{}
	for _, raw := range rawMatches {
		match, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		metadata, _ := match["metadata"].(map[string]interface{})
		if metadata == nil {
			metadata = map[string]interface{}{}
		}
		item := &models.VectorItem{
			Namespace: req.Namespace,
			ID:        stringField(metadata, "item_id"),
			Content:   stringField(metadata, "content"),
			CreatedAt: timeField(metadata, "created_at"),
			UpdatedAt: timeField(metadata, "updated_at"),
			Metadata:  userMetadata(metadata),
		}
		score, _ := match["score"].(float64)
		results = append(results, &models.VectorSearchResult{Item: item, Score: score})
	}
	return results, nil
}

// userMetadata strips the fields this package itself adds
// (tenant_id/item_id/content/created_at/updated_at) so a caller's own
// Metadata map round-trips without those internal bookkeeping keys
// leaking into it.
func userMetadata(m map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range m {
		switch k {
		case "tenant_id", "item_id", "content", "created_at", "updated_at":
			continue
		}
		out[k] = v
	}
	return out
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
