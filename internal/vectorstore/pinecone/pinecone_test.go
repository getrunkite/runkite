package pinecone_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/vectorstore"
	"github.com/sharanharsoor/runkite/internal/vectorstore/conformance"
	"github.com/sharanharsoor/runkite/internal/vectorstore/pinecone"
)

const testDimensions = 3

// newTestStore returns a Pinecone store backed by a fresh, uniquely-named
// index per test -- simpler and safer than truncating a shared index
// between tests (no cross-test vector leakage possible even if cleanup
// is skipped), same convention as Qdrant's/Weaviate's own tests. Skips
// (not fails) if PINECONE_URL is unset, same convention as every other
// backend's tests needing real infra (see docker-compose.test.yml) --
// intended to point at Pinecone Local (ghcr.io/pinecone-io/pinecone-
// local), not a real paid Pinecone account, for exactly this kind of
// create/destroy-per-test churn.
func newTestStore(t *testing.T) vectorstore.VectorStore {
	t.Helper()
	url := os.Getenv("PINECONE_URL")
	if url == "" {
		t.Skip("PINECONE_URL not set — skipping pinecone tests")
	}
	// Pinecone index names must be lowercase alphanumeric/hyphens only
	// -- unlike Qdrant's/Weaviate's collection/class naming, uppercase
	// and underscores aren't allowed.
	index := fmt.Sprintf("test-%d", time.Now().UnixNano())

	s, err := pinecone.New(url, "pclocal", index, testDimensions, "", "")
	if err != nil {
		t.Fatalf("pinecone.New: %v", err)
	}
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Deliberately NOT deleting the index in cleanup, unlike Qdrant's/
	// Weaviate's own tests -- confirmed live that Pinecone Local's index
	// deletion is asynchronous (DELETE returns 202 Accepted, not
	// 200/204), and a newly created index can be assigned the exact
	// same host:port a very recently deleted one had before that old
	// index's data is actually cleared, even after polling GET until it
	// 404s (the name disappearing from the index list and the
	// underlying port's data actually being torn down turned out to be
	// two separate events, not one). Reproduced live as later
	// conformance tests seeing 3-4 phantom results in what should have
	// been a brand-new, empty index. Leaking indices for the test
	// binary's lifetime sidesteps the race entirely and is harmless --
	// Pinecone Local is in-memory only and discarded on container
	// teardown, same "a little sprawl, no cross-test leakage possible"
	// trade-off Qdrant's/Weaviate's own tests accept for their
	// collections/classes.
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPineconeStore(t *testing.T) {
	conformance.RunVectorStoreSuite(t, newTestStore)
}
