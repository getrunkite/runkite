package pgvector_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/tenant"
	"github.com/sharanharsoor/runkite/internal/vectorstore/pgvector"
)

const testDimensions = 3

// newTestStore returns a freshly-truncated pgvector store for one test.
// Skips (not fails) if POSTGRES_DSN is unset, same convention as
// internal/state/postgres's own tests -- this suite needs a real
// pgvector-capable Postgres (see docker-compose.test.yml), not something
// SQLite could stand in for.
func newTestStore(t *testing.T) *pgvector.Store {
	t.Helper()
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping pgvector tests")
	}
	ctx := context.Background()

	// Drop first, not just truncate: vector_items' embedding column
	// width is fixed at CREATE TABLE time (see pgvector.go's Init doc
	// comment) and IF NOT EXISTS never revisits it, so a stale table
	// left over from a different dimensions value (another test binary,
	// or a manually-run example against this same shared test database)
	// would otherwise make every Upsert/Search in this suite fail with a
	// dimension-mismatch error that has nothing to do with the code
	// under test.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New for setup: %v", err)
	}
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS vector_items"); err != nil {
		pool.Close()
		t.Fatalf("drop vector_items: %v", err)
	}
	pool.Close()

	s, err := pgvector.New(ctx, dsn, testDimensions)
	if err != nil {
		t.Fatalf("pgvector.New: %v", err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertAndSearch_ReturnsNearestNeighborFirst(t *testing.T) {
	s := newTestStore(t)
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
	// The exact-match query vector should score ~1.0 (identical direction).
	if results[0].Score < 0.99 {
		t.Errorf("expected near-1.0 score for identical vector, got %v", results[0].Score)
	}
}

func TestUpsert_SameIDOverwrites(t *testing.T) {
	s := newTestStore(t)
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

func TestDelete_RemovesItemAndIsIdempotent(t *testing.T) {
	s := newTestStore(t)
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

	// Deleting again (already gone) must not error.
	if err := s.Delete(ctx, "docs", "doc1"); err != nil {
		t.Fatalf("expected idempotent delete of a missing item to succeed, got: %v", err)
	}
}

func TestNamespaceIsolation(t *testing.T) {
	s := newTestStore(t)
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

func TestTenantIsolation(t *testing.T) {
	s := newTestStore(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")

	// Same namespace + id across two tenants must not collide.
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

	// tenant-a deleting doc1 must not affect tenant-b's row.
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

func TestSearch_MetadataFilter(t *testing.T) {
	s := newTestStore(t)
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

func TestSearch_TopKLimitsResults(t *testing.T) {
	s := newTestStore(t)
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

func TestUpsert_DimensionMismatchRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.Upsert(ctx, &models.VectorItem{Namespace: "docs", ID: "a", Embedding: []float32{1, 0}})
	if err == nil {
		t.Fatal("expected an error for a 2-dimension embedding against a 3-dimension store")
	}
}

func TestSearch_DimensionMismatchRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.Search(ctx, &models.VectorSearchRequest{Namespace: "docs", Embedding: []float32{1, 0}})
	if err == nil {
		t.Fatal("expected an error for a 2-dimension query embedding against a 3-dimension store")
	}
}
