package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sharanharsoor/runkite/internal/vectorstore/pgvector"
)

// newTestEnvWithVectorStore attaches a real pgvector-backed store (needs
// POSTGRES_DSN -- see docker-compose.test.yml's pgvector/pgvector image).
// Skips, not fails, when unset, same convention as pgvector's own package
// tests.
func newTestEnvWithVectorStore(t *testing.T) *testEnv {
	t.Helper()
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping vector store API tests")
	}
	env := newTestEnv(t)
	ctx := context.Background()

	// Drop first: vector_items' embedding width is fixed at CREATE TABLE
	// time and never migrated (see pgvector.go's Init doc comment) -- a
	// stale table left at a different dimensions value by another test
	// binary/example sharing this database would otherwise fail every
	// Upsert here with an unrelated dimension-mismatch error.
	setupPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New for setup: %v", err)
	}
	if _, err := setupPool.Exec(ctx, "DROP TABLE IF EXISTS vector_items"); err != nil {
		setupPool.Close()
		t.Fatalf("drop vector_items: %v", err)
	}
	setupPool.Close()

	vs, err := pgvector.New(ctx, dsn, 3)
	if err != nil {
		t.Fatalf("pgvector.New: %v", err)
	}
	if err := vs.Init(ctx); err != nil {
		t.Fatalf("vs.Init: %v", err)
	}
	t.Cleanup(func() { vs.Close() })
	env.apiServer.SetVectorStore(vs)
	env.srv.Close()
	env.srv = httptest.NewServer(env.apiServer.Handler())
	t.Cleanup(env.srv.Close)
	return env
}

func TestVectorStore_DisabledReturns501(t *testing.T) {
	env := newTestEnv(t) // no vector store attached

	resp, err := postJSON(env.srv.URL+"/vectors/search",
		map[string]interface{}{"namespace": "docs", "embedding": []float32{1, 0, 0}})
	if err != nil {
		t.Fatalf("POST /vectors/search: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501 when vector store isn't configured, got %d", resp.StatusCode)
	}
}

func TestVectorStore_UpsertSearchDelete(t *testing.T) {
	env := newTestEnvWithVectorStore(t)

	// Upsert
	resp, err := putJSON(env.srv.URL+"/vectors/items", map[string]interface{}{
		"namespace": "docs", "id": "doc1", "content": "hello world", "embedding": []float32{1, 0, 0},
	})
	if err != nil {
		t.Fatalf("PUT /vectors/items: %v", err)
	}
	expectStatus(t, resp, http.StatusNoContent)

	// Search finds it
	resp, err = postJSON(env.srv.URL+"/vectors/search",
		map[string]interface{}{"namespace": "docs", "embedding": []float32{1, 0, 0}, "top_k": 5})
	if err != nil {
		t.Fatalf("POST /vectors/search: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	var searchResp struct {
		Results []struct {
			Item struct {
				ID      string `json:"id"`
				Content string `json:"content"`
			} `json:"item"`
			Score float64 `json:"score"`
		} `json:"results"`
	}
	json.NewDecoder(resp.Body).Decode(&searchResp)
	if len(searchResp.Results) != 1 || searchResp.Results[0].Item.ID != "doc1" {
		t.Fatalf("expected to find doc1, got %+v", searchResp.Results)
	}
	if searchResp.Results[0].Item.Content != "hello world" {
		t.Errorf("expected content 'hello world', got %q", searchResp.Results[0].Item.Content)
	}

	// Delete
	resp, err = deleteJSON(env.srv.URL+"/vectors/items", map[string]interface{}{"namespace": "docs", "id": "doc1"})
	if err != nil {
		t.Fatalf("DELETE /vectors/items: %v", err)
	}
	expectStatus(t, resp, http.StatusNoContent)

	// Search no longer finds it
	resp, err = postJSON(env.srv.URL+"/vectors/search",
		map[string]interface{}{"namespace": "docs", "embedding": []float32{1, 0, 0}})
	if err != nil {
		t.Fatalf("POST /vectors/search after delete: %v", err)
	}
	json.NewDecoder(resp.Body).Decode(&searchResp)
	if len(searchResp.Results) != 0 {
		t.Fatalf("expected 0 results after delete, got %d", len(searchResp.Results))
	}
}

func TestVectorStore_UpsertMissingFieldsRejected(t *testing.T) {
	env := newTestEnvWithVectorStore(t)

	resp, err := putJSON(env.srv.URL+"/vectors/items", map[string]interface{}{"namespace": "docs"})
	if err != nil {
		t.Fatalf("PUT /vectors/items: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing id/embedding, got %d", resp.StatusCode)
	}
}

func TestVectorStore_InternalProxyRouteWorks(t *testing.T) {
	env := newTestEnvWithVectorStore(t)

	// Same handlers mounted under /internal/vectors/* for the dual-mode
	// convention (see server.go) -- a runner without a client credential
	// reaches the same store this way.
	resp, err := putJSON(env.srv.URL+"/internal/vectors/items", map[string]interface{}{
		"namespace": "docs", "id": "doc1", "embedding": []float32{1, 0, 0},
	})
	if err != nil {
		t.Fatalf("PUT /internal/vectors/items: %v", err)
	}
	expectStatus(t, resp, http.StatusNoContent)
}
