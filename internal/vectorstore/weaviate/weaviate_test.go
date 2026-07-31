package weaviate_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/tenant"
	"github.com/sharanharsoor/runkite/internal/vectorstore"
	"github.com/sharanharsoor/runkite/internal/vectorstore/conformance"
	"github.com/sharanharsoor/runkite/internal/vectorstore/weaviate"
)

const testDimensions = 3

// newTestStore returns a Weaviate store backed by a fresh, uniquely-named
// class per test -- simpler and safer than truncating a shared class
// between tests (no cross-test object leakage possible even if cleanup
// is skipped), same convention as Qdrant's own tests. Skips (not fails)
// if WEAVIATE_URL is unset, same convention as every other backend's
// tests needing real infra (see docker-compose.test.yml).
func newTestStore(t *testing.T) vectorstore.VectorStore {
	t.Helper()
	url := os.Getenv("WEAVIATE_URL")
	if url == "" {
		t.Skip("WEAVIATE_URL not set — skipping weaviate tests")
	}
	// Weaviate class names must start with an uppercase letter --
	// "Test" prefix (not "test_" like Qdrant's collection names)
	// satisfies that while still being unique per test run.
	class := fmt.Sprintf("Test%d", time.Now().UnixNano())

	s, err := weaviate.New(url, class, testDimensions)
	if err != nil {
		t.Fatalf("weaviate.New: %v", err)
	}
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		s.Close()
		req, _ := http.NewRequest(http.MethodDelete, url+"/v1/schema/"+class, nil)
		if req != nil {
			http.DefaultClient.Do(req)
		}
	})
	return s
}

func TestWeaviateStore(t *testing.T) {
	conformance.RunVectorStoreSuite(t, newTestStore)
}

// TestSearch_FilterMatchOutsideOverFetchWindowIsStillFound is a
// permanent regression test for a live-found bug in this package's
// first implementation: Search's metadata Filter can't be pushed into
// Weaviate's own where clause (see the package's own doc comment), so
// it was handled by fetching a single fixed-size window of the
// nearest-by-vector candidates and filtering that window in Go. When a
// filter matched only a vector OUTSIDE that window (i.e. many closer,
// non-matching vectors exist), the match was silently dropped and
// Search returned an empty result instead of the one real match --
// confirmed live before the fix with exactly this shape: 25 near
// vectors that don't match the filter, 1 far vector that does, TopK=1.
// Fixed by paging through nearVector's own similarity order until a
// match is found or the corpus is exhausted, rather than stopping after
// one fixed window -- this test would have caught the bug the original
// conformance suite's own 2-item MetadataFilter test never could.
func TestSearch_FilterMatchOutsideOverFetchWindowIsStillFound(t *testing.T) {
	store := newTestStore(t)
	ctx := tenant.WithContext(context.Background(), "tenant-regress")

	// 25 vectors clustered near the query vector, none matching the
	// filter -- deliberately more than the old fixed over-fetch window
	// (topK * 20 = 20 for topK=1) so this reproduces the bug exactly.
	for i := 0; i < 25; i++ {
		near := []float32{1.0, 0.001 * float32(i), 0.0}
		if err := store.Upsert(ctx, &models.VectorItem{
			Namespace: "ns",
			ID:        fmt.Sprintf("near-%d", i),
			Embedding: near,
			Metadata:  map[string]interface{}{"lang": "en"},
		}); err != nil {
			t.Fatalf("Upsert near-%d: %v", i, err)
		}
	}

	// One vector far from the query, the ONLY one matching the filter.
	far := []float32{-1.0, 0.0, 0.0}
	if err := store.Upsert(ctx, &models.VectorItem{
		Namespace: "ns",
		ID:        "far-fr",
		Embedding: far,
		Metadata:  map[string]interface{}{"lang": "fr"},
	}); err != nil {
		t.Fatalf("Upsert far-fr: %v", err)
	}

	results, err := store.Search(ctx, &models.VectorSearchRequest{
		Namespace: "ns",
		Embedding: []float32{1.0, 0.0, 0.0}, // near the 25 non-matching vectors
		TopK:      1,
		Filter:    map[string]interface{}{"lang": "fr"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search with filter matching only a far vector returned %d results, want 1 (bug: filter match outside the old fixed over-fetch window was silently dropped)", len(results))
	}
	if results[0].Item.ID != "far-fr" {
		t.Errorf("Search returned ID %q, want %q", results[0].Item.ID, "far-fr")
	}
}
