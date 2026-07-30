package weaviate_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

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
