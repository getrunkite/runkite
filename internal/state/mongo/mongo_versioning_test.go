package mongo_test

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/sharanharsoor/runkite/internal/models"
	runkiteMongo "github.com/sharanharsoor/runkite/internal/state/mongo"
)

// TestMongoStore_ConcurrentUpsertAgentDoesNotFailOrDuplicateVersion is a
// regression test for a real gap found via audit: adding a unique index
// on agent_versions(tenant_id, agent_id, version) (fixing silent
// duplicate rows under a race) can turn that same race into a hard
// UpsertAgent failure instead, if the duplicate-key error isn't treated
// as the benign no-op it actually is here -- two concurrent calls
// racing to record the identical (tenant_id, agent_id, version) snapshot
// is not a real conflict, unlike two calls racing to create the same
// thread_id. Fires N goroutines upserting the exact same new content
// concurrently (all should compute the same target version from the
// same prior state) and asserts none of them return an error and
// exactly one version snapshot exists afterward.
func TestMongoStore_ConcurrentUpsertAgentDoesNotFailOrDuplicateVersion(t *testing.T) {
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		t.Skip("MONGO_URI not set — skipping MongoDB concurrency test")
	}
	ctx := context.Background()
	s, err := runkiteMongo.New(ctx, uri, "runkite_test")
	if err != nil {
		t.Fatalf("connect to mongo: %v", err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := s.TruncateAll(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Seed v1 so every goroutine below races to create v2 from the same
	// starting point -- the actual race this test targets.
	if err := s.UpsertAgent(ctx, &models.Agent{AgentID: "race-agent", Name: "v1", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}}); err != nil {
		t.Fatalf("seed v1: %v", err)
	}

	const concurrency = 20
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.UpsertAgent(ctx, &models.Agent{
				AgentID: "race-agent", Name: "v2", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{},
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: UpsertAgent returned an error for a benign version-snapshot race: %v", i, err)
		}
	}

	versions, err := s.ListAgentVersions(ctx, "race-agent")
	if err != nil {
		t.Fatalf("ListAgentVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("expected exactly 2 version snapshots (v1 seed + v2, not one per racing goroutine), got %d: %+v", len(versions), versions)
	}
}
