package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
)

var checkpointOrderFixtures = []struct{ id, timestamp string }{
	{"whole", "2020-01-02T10:00:00Z"},
	{"short", "2020-01-02T10:00:00.1234567Z"},
	{"offset", "2020-01-02T15:30:00.123456788+05:30"},
	{"long", "2020-01-02T10:00:00.123456789Z"},
}

func checkpointOrderStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.CreateThread(context.Background(), &models.Thread{ThreadID: "ordering", Status: models.ThreadStatusIdle, Metadata: map[string]any{}, Values: map[string]any{"prior": true}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return s
}

func insertLegacyCheckpoint(t *testing.T, s *SQLiteStore, id, timestamp string) {
	t.Helper()
	_, err := s.db.ExecContext(context.Background(), `INSERT INTO thread_checkpoints(checkpoint_id,thread_id,values_json,created_at) VALUES(?,'ordering',?,?)`, id, `{"preserved":"`+id+`"}`, timestamp)
	if err != nil {
		t.Fatal(err)
	}
}

func assertCheckpointOrder(t *testing.T, s *SQLiteStore) {
	t.Helper()
	ctx := context.Background()
	latest, err := s.GetLatestCheckpoint(ctx, "ordering")
	if err != nil || latest.Checkpoint.CheckpointID != "long" {
		t.Fatalf("latest checkpoint=%+v error=%v", latest, err)
	}
	list, err := s.ListCheckpoints(ctx, "ordering", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"long", "offset", "short", "whole"} {
		if len(list) <= i || list[i].Checkpoint.CheckpointID != want {
			t.Fatalf("checkpoint %d=%+v; want %s", i, list, want)
		}
	}
	before, err := s.ListCheckpoints(ctx, "ordering", 2, "long")
	if err != nil || len(before) != 2 || before[0].Checkpoint.CheckpointID != "offset" || before[1].Checkpoint.CheckpointID != "short" {
		t.Fatalf("before cursor=%+v error=%v", before, err)
	}
	// Completion must append to the actual newest retained checkpoint.
	now := time.Now().UTC()
	run := &models.Run{RunID: "ordered-completion", ThreadID: "ordering", AgentID: "agent", Status: models.RunStatusSuccess, Metadata: map[string]any{}, Input: json.RawMessage(`{}`), Config: json.RawMessage(`{}`), Output: json.RawMessage(`{"answer":42}`), CreatedAt: now, UpdatedAt: now}
	if err := s.CreateCachedRun(ctx, run, nil); err != nil {
		t.Fatal(err)
	}
	latest, err = s.GetLatestCheckpoint(ctx, "ordering")
	if err != nil || latest.Checkpoint.CheckpointID == "long" || latest.ParentCheckpoint == nil || latest.ParentCheckpoint.CheckpointID != "long" || len(latest.Values) != 2 {
		t.Fatalf("completion parent/values=%+v error=%v", latest, err)
	}
	if latest.CreatedAt == nil || len(*latest.CreatedAt) != len(sqlcompletion.SQLiteTimestampLayout) {
		t.Fatal("completion did not write a fixed-width timestamp")
	}
}

func TestCheckpointNewWritesPreserveNanosecondOrder(t *testing.T) {
	s := checkpointOrderStore(t)
	ctx := context.Background()
	for _, fixture := range checkpointOrderFixtures {
		created := fixture.timestamp
		if err := s.SaveCheckpoint(ctx, "ordering", &models.ThreadState{Checkpoint: models.ThreadCheckpoint{CheckpointID: fixture.id}, Values: map[string]any{"preserved": fixture.id}, CreatedAt: &created}); err != nil {
			t.Fatal(err)
		}
		stored, err := s.GetLatestCheckpoint(ctx, "ordering")
		if err != nil || stored.CreatedAt == nil || len(*stored.CreatedAt) != len(sqlcompletion.SQLiteTimestampLayout) {
			t.Fatalf("noncanonical checkpoint=%+v error=%v", stored, err)
		}
		want, _ := time.Parse(time.RFC3339Nano, fixture.timestamp)
		got, err := time.Parse(time.RFC3339Nano, *stored.CreatedAt)
		if err != nil || !got.Equal(want) {
			t.Fatalf("timestamp changed instant: got %s want %s error=%v", got, want, err)
		}
	}
	assertCheckpointOrder(t, s)
	invalid := "not-a-timestamp"
	if err := s.SaveCheckpoint(ctx, "ordering", &models.ThreadState{Checkpoint: models.ThreadCheckpoint{CheckpointID: "invalid"}, CreatedAt: &invalid}); err == nil {
		t.Fatal("invalid checkpoint timestamp accepted")
	}
}

func TestCheckpointUpgradeNormalizesLegacyHistoryWithoutChangingPayloads(t *testing.T) {
	s := checkpointOrderStore(t)
	ctx := context.Background()
	if err := s.Downgrade(ctx); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range checkpointOrderFixtures {
		insertLegacyCheckpoint(t, s, fixture.id, fixture.timestamp)
	}
	// More than one normalization page, including the empty legacy ID.
	insertLegacyCheckpoint(t, s, "", "2019-01-01T00:00:00Z")
	for i := range 260 {
		insertLegacyCheckpoint(t, s, fmt.Sprintf("batch-%03d", i), time.Date(2019, 1, 1, 0, 0, 0, i, time.UTC).Format(time.RFC3339Nano))
	}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range checkpointOrderFixtures {
		var raw, timestamp string
		if err := s.db.QueryRowContext(ctx, `SELECT values_json,created_at FROM thread_checkpoints WHERE checkpoint_id=?`, fixture.id).Scan(&raw, &timestamp); err != nil {
			t.Fatal(err)
		}
		want, _ := time.Parse(time.RFC3339Nano, fixture.timestamp)
		if timestamp != want.UTC().Format(sqlcompletion.SQLiteTimestampLayout) || raw != `{"preserved":"`+fixture.id+`"}` {
			t.Fatalf("upgrade changed history: %s %s", timestamp, raw)
		}
	}
	var normalized, total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),SUM(length(created_at)=30) FROM thread_checkpoints`).Scan(&total, &normalized); err != nil || total != 265 || normalized != total {
		t.Fatalf("normalization incomplete: total=%d normalized=%d error=%v", total, normalized, err)
	}
	assertCheckpointOrder(t, s)
	if err := s.Downgrade(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatalf("normalized history cannot re-upgrade: %v", err)
	}
}

func TestCheckpointNormalizationRejectsInvalidHistoryAtomically(t *testing.T) {
	s := checkpointOrderStore(t)
	insertLegacyCheckpoint(t, s, "a-valid", "2020-01-02T10:00:00Z")
	for i := range 256 {
		insertLegacyCheckpoint(t, s, fmt.Sprintf("b-valid-%03d", i), "2020-01-02T10:00:00Z")
	}
	insertLegacyCheckpoint(t, s, "z-invalid", "broken")
	err := s.normalizeCheckpointTimes(context.Background())
	if err == nil || !strings.Contains(err.Error(), "repair it before upgrading") {
		t.Fatalf("invalid retained timestamp accepted: %v", err)
	}
	var unchanged string
	if err := s.db.QueryRow(`SELECT created_at FROM thread_checkpoints WHERE checkpoint_id='a-valid'`).Scan(&unchanged); err != nil || unchanged != "2020-01-02T10:00:00Z" {
		t.Fatalf("normalization partially committed: %s %v", unchanged, err)
	}
}
