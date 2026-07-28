package mongo_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/state"
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

// TestMongoStore_ConcurrentUpsertAgentDifferentContent_HistoryMatchesCurrent
// is the race this package's docs used to describe as "deliberately
// deferred" pending a replica set: N goroutines racing to upsert
// DIFFERENT new content for the same agent_id, not the same content
// like the test above. Before UpsertAgent ran inside a real
// transaction, this could leave the "agents" collection's current row
// as whichever UpdateOne committed last while agent_versions kept a
// snapshot matching whichever InsertOne won instead -- a genuine
// content mismatch between the current row and its own "latest"
// history entry, only reachable with concurrent different-content
// writers (narrow but real). Now closed: every racer must succeed
// without error, and the FINAL current content must exactly match its
// own history snapshot at that exact version, every time.
func TestMongoStore_ConcurrentUpsertAgentDifferentContent_HistoryMatchesCurrent(t *testing.T) {
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

	if err := s.UpsertAgent(ctx, &models.Agent{AgentID: "race-diff", Name: "seed", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const concurrency = 20
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.UpsertAgent(ctx, &models.Agent{
				AgentID: "race-diff", Name: fmt.Sprintf("name-%c%d", 'a'+i%26, i/26),
				Metadata: map[string]interface{}{"i": float64(i)}, Capabilities: map[string]interface{}{},
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: UpsertAgent returned an error for a different-content race, expected the transaction to serialize it cleanly: %v", i, err)
		}
	}

	got, err := s.GetAgent(ctx, "race-diff")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	snap, err := s.GetAgentVersion(ctx, "race-diff", got.Version)
	if err != nil {
		t.Fatalf("history missing version %d: %v", got.Version, err)
	}
	if snap.Name != got.Name {
		t.Fatalf("content/history mismatch: agents.name=%q agent_versions@%d.name=%q -- the exact race the transaction is supposed to close", got.Name, got.Version, snap.Name)
	}
	if snap.Metadata["i"] != got.Metadata["i"] {
		t.Fatalf("metadata mismatch: agents=%v history=%v", got.Metadata, snap.Metadata)
	}

	versions, err := s.ListAgentVersions(ctx, "race-diff")
	if err != nil {
		t.Fatalf("ListAgentVersions: %v", err)
	}
	if len(versions) != got.Version {
		t.Fatalf("expected exactly %d version snapshots (one per actual content change, no duplicates/gaps), got %d", got.Version, len(versions))
	}
}

// TestMongoStore_ConcurrentPublishRegistryEntryDifferentContent_HistoryMatchesCurrent
// is PublishRegistryEntry's equivalent of the test above -- same
// transaction, same race, same guarantee.
func TestMongoStore_ConcurrentPublishRegistryEntryDifferentContent_HistoryMatchesCurrent(t *testing.T) {
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

	if err := s.PublishRegistryEntry(ctx, &models.RegistryEntry{
		Name: "race-entry", DisplayName: "seed", SourceType: "url", SourceRef: "x",
		Tags: []string{}, Metadata: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const concurrency = 20
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.PublishRegistryEntry(ctx, &models.RegistryEntry{
				Name: "race-entry", DisplayName: fmt.Sprintf("display-%c%d", 'a'+i%26, i/26), SourceType: "url", SourceRef: "x",
				Tags: []string{}, Metadata: map[string]interface{}{"i": float64(i)},
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: PublishRegistryEntry returned an error for a different-content race: %v", i, err)
		}
	}

	got, err := s.GetRegistryEntry(ctx, "race-entry")
	if err != nil {
		t.Fatalf("GetRegistryEntry: %v", err)
	}
	snap, err := s.GetRegistryEntryVersion(ctx, "race-entry", got.Version)
	if err != nil {
		t.Fatalf("history missing version %d: %v", got.Version, err)
	}
	if snap.DisplayName != got.DisplayName {
		t.Fatalf("content/history mismatch: current.display_name=%q history@%d.display_name=%q", got.DisplayName, got.Version, snap.DisplayName)
	}

	versions, err := s.ListRegistryEntryVersions(ctx, "race-entry")
	if err != nil {
		t.Fatalf("ListRegistryEntryVersions: %v", err)
	}
	if len(versions) != got.Version {
		t.Fatalf("expected exactly %d version snapshots, got %d", got.Version, len(versions))
	}
}

// TestMongoStore_DeleteRegistryEntryNotFoundReturnsQuicklyNotStuckRetrying
// is a regression test for a subtlety specific to running
// DeleteRegistryEntry inside a real transaction now (see
// withTransaction's doc comment): WithTransaction automatically retries
// its callback on a genuine MongoDB write conflict, and it would be a
// serious bug if a plain "nothing to delete" ErrNotFound were
// misclassified as one of those and retried forever instead of
// returning immediately. Bounded by a short deadline so a regression
// (an actual retry loop) fails this test via timeout rather than
// hanging the whole test binary.
func TestMongoStore_DeleteRegistryEntryNotFoundReturnsQuicklyNotStuckRetrying(t *testing.T) {
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		t.Skip("MONGO_URI not set — skipping MongoDB concurrency test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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

	start := time.Now()
	err = s.DeleteRegistryEntry(ctx, "never-existed")
	elapsed := time.Since(start)

	if _, ok := err.(*state.ErrNotFound); !ok {
		t.Fatalf("expected *state.ErrNotFound, got %T: %v", err, err)
	}
	if elapsed > time.Second {
		t.Fatalf("DeleteRegistryEntry on a nonexistent entry took %v -- expected near-instant, not stuck in a retry loop", elapsed)
	}
}
