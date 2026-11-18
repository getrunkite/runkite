package mysql_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/mysql"
)

func TestTruncateAllRemovesBoundAndLegacyUsageHolds(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set")
	}
	ctx := context.Background()
	store, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.TruncateAll(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: "hold-cleanup-thread", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	run := &models.Run{RunID: "hold-cleanup-current", ThreadID: "hold-cleanup-thread", AgentID: "agent", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRunUsageHold(ctx, state.RunIdentity(run), &models.UsageHold{RunID: run.RunID, TokensHold: 7}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertUsageHold(ctx, &models.UsageHold{RunID: "hold-cleanup-legacy", TokensHold: 5}); err != nil {
		t.Fatal(err)
	}
	if _, tokens, count, err := store.SumUsageHolds(ctx, "default", "", now.Add(-time.Hour), now.Add(time.Hour)); err != nil || tokens != 12 || count != 2 {
		t.Fatal("fixture did not create both hold kinds", tokens, count, err)
	}
	if err := store.TruncateAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, tokens, count, err := store.SumUsageHolds(ctx, "default", "", now.Add(-time.Hour), now.Add(time.Hour)); err != nil || tokens != 0 || count != 0 {
		t.Fatal("truncate retained budget reservations", tokens, count, err)
	}
}
