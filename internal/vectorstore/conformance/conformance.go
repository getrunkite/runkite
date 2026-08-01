// Package conformance provides a backend-agnostic test suite for the
// vectorstore.VectorStore interface -- every backend (pgvector, Qdrant)
// runs the exact same tests, so a behavioral difference between them
// shows up as a test failure, not a surprise in production. Same
// pattern as internal/state/conformance and
// internal/transport/conformance, extracted once a second backend
// (Qdrant) existed to actually share tests against.
package conformance

import (
	"context"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/vectorstore"
)

// StoreFactory creates a fresh (or freshly-truncated) VectorStore for
// each test, fixed at 3 dimensions -- every test below uses simple
// axis-aligned vectors ({1,0,0} etc.) so results are exact and easy to
// reason about, not fixed-point-arithmetic-sensitive.
type StoreFactory func(t *testing.T) vectorstore.VectorStore

// RunVectorStoreSuite runs every conformance test against factory.
func RunVectorStoreSuite(t *testing.T, factory StoreFactory) {
	t.Run("UpsertAndSearch_ReturnsNearestNeighborFirst", func(t *testing.T) { testUpsertAndSearchOrdering(t, factory) })
	t.Run("Upsert_SameIDOverwrites", func(t *testing.T) { testUpsertOverwrites(t, factory) })
	t.Run("Delete_RemovesItemAndIsIdempotent", func(t *testing.T) { testDeleteIdempotent(t, factory) })
	t.Run("NamespaceIsolation", func(t *testing.T) { testNamespaceIsolation(t, factory) })
	t.Run("TenantIsolation", func(t *testing.T) { testTenantIsolation(t, factory) })
	t.Run("Search_MetadataFilter", func(t *testing.T) { testMetadataFilter(t, factory) })
	t.Run("Search_TopKLimitsResults", func(t *testing.T) { testTopKLimits(t, factory) })
	t.Run("Upsert_DimensionMismatchRejected", func(t *testing.T) { testUpsertDimensionMismatch(t, factory) })
	t.Run("Search_DimensionMismatchRejected", func(t *testing.T) { testSearchDimensionMismatch(t, factory) })
	t.Run("Search_ReturnsNonNilEmptySlice", func(t *testing.T) { testSearchEmptyIsNotNil(t, factory) })
}

func testUpsertAndSearchOrdering(t *testing.T, factory StoreFactory) {
	s := factory(t)
	ctx := context.Background()

	items := map[string][]float32{
		"close":  {1, 0, 0},
		"medium": {0.5, 0.5, 0},
		"far":    {0, 1, 0},
	}
	for id, emb := range items {
		if err := s.Upsert(ctx, &models.VectorItem{Namespace: "docs", ID: id, Content: id, Embedding: emb}); err != nil {
			t.Fatalf("Upsert(%s): %v", id, err)
		}
	}

	results, err := s.Search(ctx, &models.VectorSearchRequest{Namespace: "docs", Embedding: []float32{1, 0, 0}, TopK: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	order := []string{results[0].Item.ID, results[1].Item.ID, results[2].Item.ID}
	if order[0] != "close" || order[2] != "far" {
		t.Fatalf("expected order close, medium, far; got %v", order)
	}
	if results[0].Score <= results[1].Score || results[1].Score <= results[2].Score {
		t.Fatalf("expected strictly descending scores, got %v, %v, %v", results[0].Score, results[1].Score, results[2].Score)
	}
	if results[0].Score < 0.99 {
		t.Errorf("expected near-1.0 score for identical vector, got %v", results[0].Score)
	}
}

func testUpsertOverwrites(t *testing.T, factory StoreFactory) {
	s := factory(t)
	ctx := context.Background()

	if err := s.Upsert(ctx, &models.VectorItem{Namespace: "docs", ID: "doc1", Content: "v1", Embedding: []float32{1, 0, 0}}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	if err := s.Upsert(ctx, &models.VectorItem{Namespace: "docs", ID: "doc1", Content: "v2", Embedding: []float32{0, 1, 0}}); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	results, err := s.Search(ctx, &models.VectorSearchRequest{Namespace: "docs", Embedding: []float32{0, 1, 0}, TopK: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 item (overwrite, not a second row), got %d", len(results))
	}
	if results[0].Item.Content != "v2" {
		t.Errorf("expected overwritten content 'v2', got %q", results[0].Item.Content)
	}
}

func testDeleteIdempotent(t *testing.T, factory StoreFactory) {
	s := factory(t)
	ctx := context.Background()

	if err := s.Upsert(ctx, &models.VectorItem{Namespace: "docs", ID: "doc1", Embedding: []float32{1, 0, 0}}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.Delete(ctx, "docs", "doc1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	results, err := s.Search(ctx, &models.VectorSearchRequest{Namespace: "docs", Embedding: []float32{1, 0, 0}, TopK: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results after delete, got %d", len(results))
	}

	if err := s.Delete(ctx, "docs", "doc1"); err != nil {
		t.Fatalf("expected idempotent delete of a missing item to succeed, got: %v", err)
	}
}

func testNamespaceIsolation(t *testing.T, factory StoreFactory) {
	s := factory(t)
	ctx := context.Background()

	if err := s.Upsert(ctx, &models.VectorItem{Namespace: "ns-a", ID: "shared-id", Embedding: []float32{1, 0, 0}}); err != nil {
		t.Fatalf("Upsert ns-a: %v", err)
	}
	if err := s.Upsert(ctx, &models.VectorItem{Namespace: "ns-b", ID: "shared-id", Embedding: []float32{1, 0, 0}}); err != nil {
		t.Fatalf("Upsert ns-b: %v", err)
	}

	results, err := s.Search(ctx, &models.VectorSearchRequest{Namespace: "ns-a", Embedding: []float32{1, 0, 0}, TopK: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 result scoped to ns-a, got %d", len(results))
	}
}

func testTenantIsolation(t *testing.T, factory StoreFactory) {
	s := factory(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")

	if err := s.Upsert(ctxA, &models.VectorItem{Namespace: "docs", ID: "doc1", Content: "tenant-a's doc", Embedding: []float32{1, 0, 0}}); err != nil {
		t.Fatalf("Upsert tenant-a: %v", err)
	}
	if err := s.Upsert(ctxB, &models.VectorItem{Namespace: "docs", ID: "doc1", Content: "tenant-b's doc", Embedding: []float32{1, 0, 0}}); err != nil {
		t.Fatalf("Upsert tenant-b: %v", err)
	}

	resultsA, err := s.Search(ctxA, &models.VectorSearchRequest{Namespace: "docs", Embedding: []float32{1, 0, 0}, TopK: 10})
	if err != nil {
		t.Fatalf("Search tenant-a: %v", err)
	}
	if len(resultsA) != 1 || resultsA[0].Item.Content != "tenant-a's doc" {
		t.Fatalf("expected tenant-a to see only its own doc, got %+v", resultsA)
	}

	if err := s.Delete(ctxA, "docs", "doc1"); err != nil {
		t.Fatalf("Delete tenant-a: %v", err)
	}
	resultsB, err := s.Search(ctxB, &models.VectorSearchRequest{Namespace: "docs", Embedding: []float32{1, 0, 0}, TopK: 10})
	if err != nil {
		t.Fatalf("Search tenant-b: %v", err)
	}
	if len(resultsB) != 1 {
		t.Fatalf("expected tenant-b's doc to survive tenant-a's delete, got %d results", len(resultsB))
	}
}

func testMetadataFilter(t *testing.T, factory StoreFactory) {
	s := factory(t)
	ctx := context.Background()

	s.Upsert(ctx, &models.VectorItem{Namespace: "docs", ID: "a", Embedding: []float32{1, 0, 0}, Metadata: map[string]interface{}{"lang": "en"}})
	s.Upsert(ctx, &models.VectorItem{Namespace: "docs", ID: "b", Embedding: []float32{1, 0, 0}, Metadata: map[string]interface{}{"lang": "fr"}})

	results, err := s.Search(ctx, &models.VectorSearchRequest{
		Namespace: "docs",
		Embedding: []float32{1, 0, 0},
		TopK:      10,
		Filter:    map[string]interface{}{"lang": "fr"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Item.ID != "b" {
		t.Fatalf("expected only item 'b' (lang=fr), got %+v", results)
	}
}

func testTopKLimits(t *testing.T, factory StoreFactory) {
	s := factory(t)
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c", "d"} {
		s.Upsert(ctx, &models.VectorItem{Namespace: "docs", ID: id, Embedding: []float32{1, 0, 0}})
	}
	results, err := s.Search(ctx, &models.VectorSearchRequest{Namespace: "docs", Embedding: []float32{1, 0, 0}, TopK: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected TopK=2 to cap results at 2, got %d", len(results))
	}
}

func testUpsertDimensionMismatch(t *testing.T, factory StoreFactory) {
	s := factory(t)
	ctx := context.Background()

	err := s.Upsert(ctx, &models.VectorItem{Namespace: "docs", ID: "a", Embedding: []float32{1, 0}})
	if err == nil {
		t.Fatal("expected an error for a 2-dimension embedding against a 3-dimension store")
	}
}

func testSearchDimensionMismatch(t *testing.T, factory StoreFactory) {
	s := factory(t)
	ctx := context.Background()

	_, err := s.Search(ctx, &models.VectorSearchRequest{Namespace: "docs", Embedding: []float32{1, 0}})
	if err == nil {
		t.Fatal("expected an error for a 2-dimension query embedding against a 3-dimension store")
	}
}

func testSearchEmptyIsNotNil(t *testing.T, factory StoreFactory) {
	s := factory(t)
	ctx := context.Background()

	results, err := s.Search(ctx, &models.VectorSearchRequest{Namespace: "nonexistent-namespace", Embedding: []float32{1, 0, 0}, TopK: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if results == nil {
		t.Fatal("expected a non-nil empty slice for zero results, got nil")
	}
}
