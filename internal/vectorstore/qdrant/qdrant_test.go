package qdrant_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/vectorstore"
	"github.com/sharanharsoor/runkite/internal/vectorstore/conformance"
	"github.com/sharanharsoor/runkite/internal/vectorstore/qdrant"
)

const testDimensions = 3

// newTestStore returns a Qdrant store backed by a fresh, uniquely-named
// collection per test -- simpler and safer than truncating a shared
// collection between tests (no cross-test point leakage possible even
// if cleanup is skipped), at the cost of a little collection sprawl in
// a throwaway test instance. Skips (not fails) if QDRANT_URL is unset,
// same convention as every other backend's tests needing real infra
// (see docker-compose.test.yml).
func newTestStore(t *testing.T) vectorstore.VectorStore {
	t.Helper()
	url := os.Getenv("QDRANT_URL")
	if url == "" {
		t.Skip("QDRANT_URL not set — skipping qdrant tests")
	}
	collection := fmt.Sprintf("test_%d", time.Now().UnixNano())

	s, err := qdrant.New(url, collection, testDimensions)
	if err != nil {
		t.Fatalf("qdrant.New: %v", err)
	}
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		s.Close()
		req, _ := http.NewRequest(http.MethodDelete, url+"/collections/"+collection, nil)
		if req != nil {
			http.DefaultClient.Do(req)
		}
	})
	return s
}

func TestQdrantStore(t *testing.T) {
	conformance.RunVectorStoreSuite(t, newTestStore)
}
