package mysql_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
)

// Concurrency races against a live MySQL container -- the shared
// conformance suite (internal/state/conformance) is deliberately
// single-threaded, so these are this package's own coverage for the
// specific race conditions MySQL's atomic INSERT ... ON DUPLICATE KEY
// UPDATE design claims to close.

// TestAgent_ConcurrentUpsertIsRaceFree proves the single-atomic-
// statement design claim in UpsertAgent's own doc comment: unlike
// MongoDB's separate read-then-write (which needs an explicit
// duplicate-key-is-benign carve-out for exactly this race), MySQL's
// INSERT ... ON DUPLICATE KEY UPDATE computes and applies the version
// bump in one statement, so N concurrent upserts of the SAME new
// content should never error and should always converge on exactly
// one final version with exactly one matching history snapshot --
// no race-handling carve-out needed at all.
func TestAgent_ConcurrentUpsertIsRaceFree(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.UpsertAgent(ctx, &models.Agent{AgentID: "race-agent", Name: "v1", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})

	const concurrency = 20
	errs := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			errs <- s.UpsertAgent(ctx, &models.Agent{AgentID: "race-agent", Name: "v2", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})
		}()
	}
	for i := 0; i < concurrency; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent upsert %d returned an error: %v", i, err)
		}
	}

	got, err := s.GetAgent(ctx, "race-agent")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("expected version 2 after N racers all upsert the same new content, got %d", got.Version)
	}
	versions, err := s.ListAgentVersions(ctx, "race-agent")
	if err != nil {
		t.Fatalf("ListAgentVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("expected exactly 2 version snapshots (v1 seed + v2, not one per racing goroutine), got %d: %+v", len(versions), versions)
	}
}

// TestAgent_ConcurrentDifferentContent keeps agents.row content aligned
// with the matching agent_versions snapshot under last-writer-wins.
// Catches the Mongo-style "current row says X@vN but history[vN] says Y"
// failure mode if the MySQL row lock + LAST_INSERT_ID path ever regresses.
func TestAgent_ConcurrentDifferentContent_HistoryMatchesCurrent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.UpsertAgent(ctx, &models.Agent{AgentID: "race-diff", Name: "seed", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})

	const concurrency = 20
	errs := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		i := i
		go func() {
			errs <- s.UpsertAgent(ctx, &models.Agent{
				AgentID: "race-diff", Name: "name-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
				Metadata: map[string]interface{}{"i": float64(i)}, Capabilities: map[string]interface{}{},
			})
		}()
	}
	for i := 0; i < concurrency; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	got, err := s.GetAgent(ctx, "race-diff")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.GetAgentVersion(ctx, "race-diff", got.Version)
	if err != nil {
		t.Fatalf("history missing version %d: %v", got.Version, err)
	}
	if snap.Name != got.Name {
		t.Fatalf("content/history mismatch: agents.name=%q agent_versions@%d.name=%q", got.Name, got.Version, snap.Name)
	}
	if snap.Metadata["i"] != got.Metadata["i"] {
		t.Fatalf("metadata mismatch: agents=%v history=%v", got.Metadata, snap.Metadata)
	}
}

// TestThread_ConcurrentClaimOnlyOneWins verifies TryClaimThread's
// TOCTOU-safety directly: 20 goroutines racing to claim the same idle
// thread under -race, exactly one must win every time.
func TestThread_ConcurrentClaimOnlyOneWins(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedThread(t, ctx, s, "t-claim-race")

	const concurrency = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, err := s.TryClaimThread(ctx, "t-claim-race")
			if err != nil {
				t.Errorf("TryClaimThread: %v", err)
				return
			}
			if won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("expected exactly 1 winner among %d concurrent claims, got %d", concurrency, wins)
	}
}

// TestCron_ConcurrentClaimOnlyOneWins directly exercises the empirical
// claim underlying TryClaimCronFire's design (verified live before
// writing the implementation): MySQL's INSERT ... ON DUPLICATE KEY
// UPDATE reports RowsAffected()=1 for a genuine insert and =0 for a
// no-op update onto an existing row, so N concurrent instances racing
// to claim the exact same (schedule, fire_time) tick must produce
// exactly one winner, never zero and never more than one.
func TestCron_ConcurrentClaimOnlyOneWins(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fireTime := time.Now().UTC().Truncate(time.Minute)

	const concurrency = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, err := s.TryClaimCronFire(ctx, "sched-concurrent", fireTime)
			if err != nil {
				t.Errorf("TryClaimCronFire: %v", err)
				return
			}
			if won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("expected exactly 1 winner among %d concurrent claims, got %d", concurrency, wins)
	}
}
