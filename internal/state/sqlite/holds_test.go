package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
)

func TestExpireUsageHolds(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	old := &models.UsageHold{
		RunID: "run-old", TenantID: "acme", AgentID: "echo",
		USDHold: 1.5, TokensHold: 100, CreatedAt: now.Add(-2 * time.Hour),
	}
	fresh := &models.UsageHold{
		RunID: "run-fresh", TenantID: "acme", AgentID: "echo",
		USDHold: 0.5, TokensHold: 50, CreatedAt: now.Add(-10 * time.Minute),
	}
	if err := store.UpsertUsageHold(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertUsageHold(ctx, fresh); err != nil {
		t.Fatal(err)
	}

	n, err := store.ExpireUsageHolds(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expired=%d want 1", n)
	}
	usd, tok, count, err := store.SumUsageHolds(ctx, "acme", "echo", now.Add(-24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || usd != 0.5 || tok != 50 {
		t.Fatalf("remaining holds usd=%v tok=%d count=%d", usd, tok, count)
	}
}
