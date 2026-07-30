// Package weaviate implements vectorstore.VectorStore using Weaviate's
// REST + GraphQL APIs. Closes one of the two remaining "Tier 2, not yet
// built" vector backends alongside Pinecone -- see
// internal/vectorstore/qdrant's package doc for the shared design
// principles (one shared class for every tenant/namespace, deterministic
// UUID v5 object IDs, plain HTTP rather than a generated client) this
// package follows too.
//
// Plain net/http against Weaviate's REST API for writes (POST/PUT/DELETE
// /v1/objects, POST/GET /v1/schema) and its GraphQL API for vector search
// (nearVector + where -- Weaviate has no REST-level ANN search endpoint),
// rather than the official weaviate-go-client: same "small, documented
// HTTP surface, no generated client needed" reasoning as Qdrant's.
//
// Weaviate's schema is fixed-property (every property and its type is
// declared up front), unlike Qdrant's freeform JSON payload -- so
// arbitrary caller-supplied Metadata is stored as a single JSON-encoded
// "metadata_json" text property rather than one Weaviate property per
// key; the schema doesn't need to change every time a caller invents a
// new metadata key. The trade-off: Search's Filter (exact-match over
// Metadata) can't be pushed down into Weaviate's own "where" clause the
// way tenant_id/namespace are, since Weaviate can't query inside a
// JSON-encoded string field. Handled by over-fetching a larger candidate
// set (still tenant/namespace-scoped server-side, and still returned in
// nearVector's own similarity order) and applying the metadata filter in
// Go before truncating to TopK -- exact and correct, just not as
// index-efficient as a native filter would be for a very large namespace
// with a metadata filter applied.
package weaviate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// objectNamespace is a fixed, arbitrary UUID used as the "namespace" for
// UUID v5 object-ID derivation (uuid.NewSHA1 needs one) -- deliberately
// different from Qdrant's own fixed UUID (there's no reason the two
// backends need matching object IDs for the same logical item, and
// keeping them distinct avoids inviting an unintended coupling).
var objectNamespace = uuid.MustParse("8f2c9e6a-3d1b-4c7e-9a5f-2b6d8e1a4c9f")

func objectID(tenantID, namespace, id string) string {
	return uuid.NewSHA1(objectNamespace, []byte(tenantID+"\x00"+namespace+"\x00"+id)).String()
}

// overFetchMultiplier and overFetchMax bound the "fetch a broader
// candidate set, then filter client-side" strategy Search uses when a
// metadata Filter is given -- see this package's own doc comment.
const (
	overFetchMultiplier = 20
	overFetchMax        = 500
)

// Store implements vectorstore.VectorStore against a Weaviate instance.
type Store struct {
	baseURL    string
	class      string
	dimensions int
	client     *http.Client
}

// New creates a Weaviate-backed store. dimensions is enforced by this
// package on every Upsert/Search call (checked before any HTTP call is
// made) -- Weaviate itself doesn't fix a vector width at the schema
// level the way pgvector's vector(N) column or Qdrant's collection
// config do, so nothing would otherwise catch a caller mixing embedding
// sizes within the same class.
func New(baseURL, class string, dimensions int) (*Store, error) {
	if dimensions <= 0 {
		return nil, fmt.Errorf("weaviate: dimensions must be > 0, got %d", dimensions)
	}
	if class == "" {
		class = "VectorItems"
	}
	return &Store{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		class:      class,
		dimensions: dimensions,
		// Bound every call: a hung/unreachable Weaviate must not hang
		// the control plane's /vectors/* handlers forever. 30s covers
		// large ANN searches without pretending the request can wait
		// forever -- same bound and reasoning as Qdrant's.
		client: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (s *Store) Close() error {
	// No persistent connection to close -- s.client is a plain
	// *http.Client, not a pool.
	return nil
}

// Init creates the class (collection) if it doesn't already exist.
// Checks via GET /v1/schema/{class} first (200 = already there) rather
// than blindly POSTing and matching an error string -- Weaviate's
// "class already exists" response is a generic 422 that isn't reliably
// distinguishable from other schema validation failures, unlike
// Qdrant's dedicated 409.
//
// vectorizer: "none" is load-bearing, not a default left in place --
// without it Weaviate runs its own configured vectorizer module against
// the object's properties and DISCARDS the caller-supplied vector
// entirely, silently breaking every embedding this store is handed.
//
// Known limitation, same shape as pgvector's and Qdrant's: the class's
// vector width is fixed by whatever dimensions New was called with, the
// first time the class is actually created. Changing
// vector_store.dimensions in langgraph.json after the class already
// exists does not migrate it -- Weaviate has no fixed-width vector
// index constraint to reject a mismatched Upsert the way pgvector's
// column type does, so a silent width mismatch there would corrupt the
// index rather than error; this package's own explicit dimensions
// check on every Upsert/Search is what actually catches it instead.
func (s *Store) Init(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/v1/schema/"+s.class, nil)
	if err != nil {
		return fmt.Errorf("weaviate: build schema-check request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("weaviate: check schema: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}

	// tokenization: "field" on tenant_id/namespace/item_id is
	// load-bearing, not a style choice: Weaviate's default ("word")
	// splits a value like "ns-a" into indexed tokens ["ns", "a"], and
	// its "Equal" where-operator matches on ANY shared token -- so a
	// filter for namespace="ns-a" would also match "ns-b" (both share
	// the "ns" token), confirmed live while building this against a
	// real Weaviate instance (NamespaceIsolation/TenantIsolation both
	// failed with "field" left at the default before this fix). "field"
	// tokenization treats the whole value as one indexed token, giving
	// the exact-match semantics Search's where clause actually needs.
	// content/metadata_json/created_at/updated_at are never filtered on
	// by this package, so they're left at the default.
	body := map[string]interface{}{
		"class":      s.class,
		"vectorizer": "none",
		"properties": []map[string]interface{}{
			{"name": "tenant_id", "dataType": []string{"text"}, "tokenization": "field"},
			{"name": "namespace", "dataType": []string{"text"}, "tokenization": "field"},
			// "id" is reserved (Weaviate's own object UUID) --
			// item_id carries the caller's own VectorItem.ID.
			{"name": "item_id", "dataType": []string{"text"}, "tokenization": "field"},
			{"name": "content", "dataType": []string{"text"}},
			{"name": "metadata_json", "dataType": []string{"text"}},
			{"name": "created_at", "dataType": []string{"text"}},
			{"name": "updated_at", "dataType": []string{"text"}},
		},
	}
	return s.post(ctx, "/v1/schema", body)
}

func (s *Store) post(ctx context.Context, path string, body interface{}) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("weaviate: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("weaviate: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("weaviate: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("weaviate: %s returned %d: %s", path, resp.StatusCode, string(respBody))
	}
	return nil
}

func (s *Store) deleteByID(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.baseURL+"/v1/objects/"+s.class+"/"+id, nil)
	if err != nil {
		return fmt.Errorf("weaviate: build delete request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("weaviate: delete request failed: %w", err)
	}
	defer resp.Body.Close()
	// 404 (nothing to delete) is fine -- Delete's own idempotency
	// contract, and also the expected first half of every Upsert below.
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("weaviate: delete returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// Upsert's created_at is a known, stated limitation, same as Qdrant's:
// Weaviate's PUT /v1/objects/{class}/{id} replaces an EXISTING object
// (404 if missing) and POST /v1/objects fails if the object already
// exists (see the package's git history / Weaviate's own docs) --
// neither alone is a true upsert. Delete-then-create (ignoring
// not-found on the delete) is simple and correct for "same ID
// overwrites, not duplicates," at the cost of one extra round trip
// versus a real upsert endpoint, and it means created_at is reset to
// "now" on every Upsert call, including a re-index of an
// already-existing id -- correct for a fresh item, but re-indexing an
// existing document loses its original created_at rather than
// preserving it.
func (s *Store) Upsert(ctx context.Context, item *models.VectorItem) error {
	if len(item.Embedding) != s.dimensions {
		return fmt.Errorf("weaviate: embedding has %d dimensions, store configured for %d", len(item.Embedding), s.dimensions)
	}
	tenantID := tenant.FromContext(ctx)
	id := objectID(tenantID, item.Namespace, item.ID)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	metaJSON, err := json.Marshal(item.Metadata)
	if err != nil {
		return fmt.Errorf("weaviate: marshal metadata: %w", err)
	}

	if err := s.deleteByID(ctx, id); err != nil {
		return err
	}
	body := map[string]interface{}{
		"class": s.class,
		"id":    id,
		"properties": map[string]interface{}{
			"tenant_id":     tenantID,
			"namespace":     item.Namespace,
			"item_id":       item.ID,
			"content":       item.Content,
			"metadata_json": string(metaJSON),
			"created_at":    now,
			"updated_at":    now,
		},
		"vector": item.Embedding,
	}
	return s.post(ctx, "/v1/objects", body)
}

func (s *Store) Delete(ctx context.Context, namespace, id string) error {
	return s.deleteByID(ctx, objectID(tenant.FromContext(ctx), namespace, id))
}

type graphQLHit struct {
	ItemID       string `json:"item_id"`
	Content      string `json:"content"`
	MetadataJSON string `json:"metadata_json"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	Additional   struct {
		Distance float64 `json:"distance"`
	} `json:"_additional"`
}

type graphQLResponse struct {
	Data struct {
		Get map[string][]graphQLHit `json:"Get"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// Search's ordering relies on Weaviate's GraphQL Get already returning
// nearVector hits sorted by similarity (closest first) -- Go only ever
// filters that sequence down (client-side metadata filter) and
// truncates it (TopK), never re-sorts it, so an over-fetched batch's
// relative order is preserved through to the final TopK results.
func (s *Store) Search(ctx context.Context, req *models.VectorSearchRequest) ([]*models.VectorSearchResult, error) {
	if len(req.Embedding) != s.dimensions {
		return nil, fmt.Errorf("weaviate: query embedding has %d dimensions, store configured for %d", len(req.Embedding), s.dimensions)
	}
	topK := req.TopK
	if topK <= 0 {
		topK = 10
	}
	limit := topK
	if len(req.Filter) > 0 {
		limit = topK * overFetchMultiplier
		if limit > overFetchMax {
			limit = overFetchMax
		}
	}

	vectorJSON, err := json.Marshal(req.Embedding)
	if err != nil {
		return nil, fmt.Errorf("weaviate: marshal query embedding: %w", err)
	}
	// %q escapes tenantID/req.Namespace the same way encoding/json would
	// for a string (quotes, backslashes, control chars) -- GraphQL and
	// JSON string-literal escaping are close enough for this that a
	// value containing a stray quote stays inside the string literal
	// rather than breaking out of it, without needing a full GraphQL
	// query-builder dependency for four interpolated values.
	query := fmt.Sprintf(`{
		Get {
			%s(
				nearVector: {vector: %s}
				limit: %d
				where: {
					operator: And
					operands: [
						{path: ["tenant_id"], operator: Equal, valueText: %q}
						{path: ["namespace"], operator: Equal, valueText: %q}
					]
				}
			) {
				item_id
				content
				metadata_json
				created_at
				updated_at
				_additional { distance }
			}
		}
	}`, s.class, string(vectorJSON), limit, tenant.FromContext(ctx), req.Namespace)

	resp, err := s.graphQL(ctx, query)
	if err != nil {
		return nil, err
	}

	// Non-nil so a no-results search JSON-encodes to [] rather than
	// null -- SDK clients call .map() on it unconditionally (same
	// contract as Qdrant's and internal/state/conformance's
	// empty_list_results_are_not_nil).
	results := []*models.VectorSearchResult{}
	for _, hit := range resp.Data.Get[s.class] {
		metadata := map[string]interface{}{}
		if hit.MetadataJSON != "" {
			_ = json.Unmarshal([]byte(hit.MetadataJSON), &metadata)
		}
		if !matchesFilter(metadata, req.Filter) {
			continue
		}
		item := &models.VectorItem{
			Namespace: req.Namespace,
			ID:        hit.ItemID,
			Content:   hit.Content,
			Metadata:  metadata,
			CreatedAt: parseTime(hit.CreatedAt),
			UpdatedAt: parseTime(hit.UpdatedAt),
		}
		// Weaviate's cosine distance is 1 - cosine_sim(a, b); this
		// store's own contract (matching Qdrant's) is the similarity
		// itself, not the distance, so it's converted back here.
		results = append(results, &models.VectorSearchResult{Item: item, Score: 1 - hit.Additional.Distance})
		if len(results) >= topK {
			break
		}
	}
	return results, nil
}

func matchesFilter(metadata, filter map[string]interface{}) bool {
	for k, v := range filter {
		if fmt.Sprintf("%v", metadata[k]) != fmt.Sprintf("%v", v) {
			return false
		}
	}
	return true
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func (s *Store) graphQL(ctx context.Context, query string) (*graphQLResponse, error) {
	b, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, fmt.Errorf("weaviate: marshal graphql request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/v1/graphql", bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("weaviate: build graphql request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("weaviate: graphql request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("weaviate: read graphql response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("weaviate: graphql returned %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed graphQLResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("weaviate: parse graphql response: %w", err)
	}
	if len(parsed.Errors) > 0 {
		return nil, fmt.Errorf("weaviate: graphql error: %s", parsed.Errors[0].Message)
	}
	return &parsed, nil
}
