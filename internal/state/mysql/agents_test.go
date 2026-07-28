package mysql_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/state/mysql"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// Checkpoint-2 (Agents section) coverage. Hand-written rather than
// wired into internal/state/conformance's shared suite -- *mysql.Store
// doesn't satisfy the full state.Store interface yet (most sections
// aren't implemented in this checkpoint), so it can't be passed to
// conformance.RunStoreSuite's factory type until every method exists.
// This file's tests get superseded by (not duplicated alongside) the
// conformance suite once the interface is complete.
func newTestStore(t *testing.T) *mysql.Store {
	t.Helper()
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		dsn = "runkite:runkite@tcp(127.0.0.1:3307)/runkite_test?parseTime=true"
	}
	ctx := context.Background()
	s, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Skipf("mysql not available: %v", err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := execTruncate(ctx, s); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// execTruncate clears agents/agent_versions/agent_schemas between
// tests -- a package-private helper here rather than a Store method,
// since TruncateAll (a test-only concept on every other backend) isn't
// part of this checkpoint's scope yet either.
func execTruncate(ctx context.Context, s *mysql.Store) (bool, error) {
	for _, tbl := range []string{"agent_schemas", "agent_versions", "agents"} {
		if err := mysqlExec(ctx, s, "DELETE FROM "+tbl); err != nil {
			return false, err
		}
	}
	return true, nil
}

func mysqlExec(ctx context.Context, s *mysql.Store, query string) error {
	// s.DB() isn't exposed (Store deliberately keeps *sql.DB private,
	// same encapsulation as every other backend) -- reuse UpsertAgent's
	// own connection indirectly isn't possible for arbitrary DDL/DML,
	// so this test package opens its own short-lived connection using
	// the same DSN convention instead of reaching into Store's
	// internals.
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		dsn = "runkite:runkite@tcp(127.0.0.1:3307)/runkite_test?parseTime=true"
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, query)
	return err
}

func TestAgent_UpsertVersionBumpsOnlyOnActualChange(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	agent := &models.Agent{AgentID: "a1", Name: "v1", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}}
	if err := s.UpsertAgent(ctx, agent); err != nil {
		t.Fatalf("upsert v1: %v", err)
	}
	if err := s.UpsertAgent(ctx, agent); err != nil {
		t.Fatalf("upsert v1 unchanged: %v", err)
	}
	got, err := s.GetAgent(ctx, "a1")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("expected version 1 after unchanged republish, got %d", got.Version)
	}

	agent.Name = "v2"
	if err := s.UpsertAgent(ctx, agent); err != nil {
		t.Fatalf("upsert v2: %v", err)
	}
	got, err = s.GetAgent(ctx, "a1")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("expected version 2 after change, got %d", got.Version)
	}
}

func TestAgent_VersionHistoryAndRollbackSnapshot(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	agent := &models.Agent{AgentID: "a1", Name: "v1", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}}
	s.UpsertAgent(ctx, agent)
	agent.Name = "v2"
	s.UpsertAgent(ctx, agent)

	versions, err := s.ListAgentVersions(ctx, "a1")
	if err != nil {
		t.Fatalf("ListAgentVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("expected 2 version snapshots, got %d: %+v", len(versions), versions)
	}
	if versions[0].Version != 2 || versions[0].Name != "v2" {
		t.Fatalf("expected newest-first v2, got %+v", versions[0])
	}
	if versions[1].Version != 1 || versions[1].Name != "v1" {
		t.Fatalf("expected v1 last, got %+v", versions[1])
	}

	v1, err := s.GetAgentVersion(ctx, "a1", 1)
	if err != nil {
		t.Fatalf("GetAgentVersion(1): %v", err)
	}
	if v1.Name != "v1" {
		t.Fatalf("expected v1 snapshot name v1, got %s", v1.Name)
	}

	_, err = s.GetAgentVersion(ctx, "a1", 99)
	if _, ok := err.(*state.ErrNotFound); !ok {
		t.Fatalf("expected ErrNotFound for unknown version, got %v", err)
	}
}

func TestAgent_GetNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetAgent(context.Background(), "does-not-exist")
	if _, ok := err.(*state.ErrNotFound); !ok {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestAgent_SearchByNameAndMetadata(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.UpsertAgent(ctx, &models.Agent{AgentID: "alpha", Name: "alpha-agent", Metadata: map[string]interface{}{"team": "sales"}, Capabilities: map[string]interface{}{}})
	s.UpsertAgent(ctx, &models.Agent{AgentID: "beta", Name: "beta-agent", Metadata: map[string]interface{}{"team": "support"}, Capabilities: map[string]interface{}{}})

	all, err := s.SearchAgents(ctx, &models.AgentSearchRequest{Limit: 10})
	if err != nil {
		t.Fatalf("SearchAgents(all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(all))
	}

	byName, err := s.SearchAgents(ctx, &models.AgentSearchRequest{Name: "alpha", Limit: 10})
	if err != nil {
		t.Fatalf("SearchAgents(name): %v", err)
	}
	if len(byName) != 1 || byName[0].AgentID != "alpha" {
		t.Fatalf("expected only alpha, got %+v", byName)
	}

	byMeta, err := s.SearchAgents(ctx, &models.AgentSearchRequest{Metadata: map[string]interface{}{"team": "support"}, Limit: 10})
	if err != nil {
		t.Fatalf("SearchAgents(metadata): %v", err)
	}
	if len(byMeta) != 1 || byMeta[0].AgentID != "beta" {
		t.Fatalf("expected only beta, got %+v", byMeta)
	}
}

func TestAgentSchema_UpsertAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.UpsertAgent(ctx, &models.Agent{AgentID: "a1", Name: "a", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})

	if err := s.UpsertAgentSchema(ctx, &models.AgentSchema{
		AgentID: "a1", InputSchema: map[string]interface{}{"a": float64(1)},
		OutputSchema: map[string]interface{}{}, StateSchema: map[string]interface{}{}, ConfigSchema: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("UpsertAgentSchema: %v", err)
	}
	sch, err := s.GetAgentSchema(ctx, "a1")
	if err != nil {
		t.Fatalf("GetAgentSchema: %v", err)
	}
	if sch.InputSchema["a"] != float64(1) {
		t.Fatalf("expected input_schema.a=1, got %+v", sch.InputSchema)
	}

	// Republish updates in place, not a second row.
	if err := s.UpsertAgentSchema(ctx, &models.AgentSchema{
		AgentID: "a1", InputSchema: map[string]interface{}{"a": float64(2)},
		OutputSchema: map[string]interface{}{}, StateSchema: map[string]interface{}{}, ConfigSchema: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("UpsertAgentSchema (update): %v", err)
	}
	sch, _ = s.GetAgentSchema(ctx, "a1")
	if sch.InputSchema["a"] != float64(2) {
		t.Fatalf("expected updated input_schema.a=2, got %+v", sch.InputSchema)
	}
}

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

func TestAgent_TenantIsolation(t *testing.T) {
	s := newTestStore(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")

	s.UpsertAgent(ctxA, &models.Agent{AgentID: "shared", Name: "a-name", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})
	s.UpsertAgent(ctxB, &models.Agent{AgentID: "shared", Name: "b-name", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})

	gotA, err := s.GetAgent(ctxA, "shared")
	if err != nil || gotA.Name != "a-name" {
		t.Fatalf("expected tenant-a's own agent, got %+v err=%v", gotA, err)
	}
	gotB, err := s.GetAgent(ctxB, "shared")
	if err != nil || gotB.Name != "b-name" {
		t.Fatalf("expected tenant-b's own agent, got %+v err=%v", gotB, err)
	}

	// tenant-a upserting must not bump tenant-b's version.
	agentA := &models.Agent{AgentID: "shared", Name: "a-name-v2", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}}
	s.UpsertAgent(ctxA, agentA)
	gotBAfter, _ := s.GetAgent(ctxB, "shared")
	if gotBAfter.Version != 1 {
		t.Fatalf("expected tenant-b's version unaffected by tenant-a's upsert, got %d", gotBAfter.Version)
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

// TestAgent_GoMapInsertionOrderDoesNotBumpVersion is a narrower claim
// than it might look: Go's own encoding/json ALWAYS sorts map keys
// alphabetically before marshaling (verified separately, not assumed --
// json.Marshal on {"b":2,"a":1} and {"a":1,"b":2} produces byte-identical
// output regardless of Go map insertion order), so this test's two
// UpsertAgent calls actually send byte-identical JSON to MySQL either
// way -- it confirms the Go-level guarantee holds through this code
// path, not MySQL's own JSON `!=` normalization at the SQL level.
// MySQL's OWN normalization (that its JSON columns compare by parsed
// value, not raw byte-for-byte text, independent of what Go sends) was
// verified directly against the server with raw, differently-formatted
// JSON literals (`{"x":1}` inserted then compared against MySQL's own
// re-serialized `{"x": 1}` with a space) before writing UpsertAgent at
// all -- see this file's mysql.go package doc and this checkpoint's own
// commit history for that raw-SQL verification, not repeated here as a
// Go test since it would require bypassing json.Marshal entirely to be
// a meaningful re-check.
func TestAgent_GoMapInsertionOrderDoesNotBumpVersion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.UpsertAgent(ctx, &models.Agent{
		AgentID: "json-order", Name: "n",
		Metadata:     map[string]interface{}{"a": float64(1), "b": float64(2)},
		Capabilities: map[string]interface{}{"x": true, "y": false},
	})
	// Same logical JSON, different Go map iteration insertion order.
	s.UpsertAgent(ctx, &models.Agent{
		AgentID: "json-order", Name: "n",
		Metadata:     map[string]interface{}{"b": float64(2), "a": float64(1)},
		Capabilities: map[string]interface{}{"y": false, "x": true},
	})
	got, err := s.GetAgent(ctx, "json-order")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Fatalf("JSON key order should not bump version, got %d", got.Version)
	}
}
