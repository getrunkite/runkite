package mongo_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	runkiteMongo "github.com/getrunkite/runkite/internal/state/mongo"
	"github.com/getrunkite/runkite/internal/tenant"
)

// Mongo's admission daily-count exclusion is a bson filter, not a SQL
// string fragment like the other three backends -- structurally the most
// likely place for a copy-paste mismatch, and untested until this file.
// Confirms sim runs against AgentDaily=1 do not exhaust the cap on real
// Mongo, and a real run still is (and then isn't) admitted after.
func TestCreateRunAdmitted_DailyExcludesSimulation(t *testing.T) {
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		t.Skip("MONGO_URI not set")
	}
	const dbName = "runkite_test"
	ctx := context.Background()
	s, err := runkiteMongo.New(ctx, uri, dbName)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.TruncateAll(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	tctx := tenant.WithContext(ctx, "sim-verify-admit-mongo")
	now := time.Now().UTC()
	if err := s.CreateThread(tctx, &models.Thread{ThreadID: "simmt1", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	caps := &state.RunAdmissionCaps{AgentDaily: 1, Now: now}

	for i := 0; i < 3; i++ {
		run := &models.Run{
			RunID: "sim-admit-sim-" + string(rune('a'+i)), ThreadID: "simmt1", AgentID: "echo",
			Status: models.RunStatusPending, Metadata: map[string]interface{}{"simulation": true},
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateRunAdmitted(tctx, run, caps); err != nil {
			t.Fatalf("sim create %d should not hit daily cap: %v", i, err)
		}
	}

	real1 := &models.Run{RunID: "sim-admit-real-1", ThreadID: "simmt1", AgentID: "echo", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateRunAdmitted(tctx, real1, caps); err != nil {
		t.Fatalf("first real create should be admitted: %v", err)
	}

	real2 := &models.Run{RunID: "sim-admit-real-2", ThreadID: "simmt1", AgentID: "echo", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}
	err = s.CreateRunAdmitted(tctx, real2, caps)
	if err == nil {
		t.Fatal("second real create should be denied by daily cap")
	}
	if _, ok := err.(*state.ErrAdmissionLimitExceeded); !ok {
		t.Fatalf("want ErrAdmissionLimitExceeded, got %v (%T)", err, err)
	}
}
