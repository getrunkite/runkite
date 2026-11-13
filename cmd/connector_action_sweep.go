package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// Reconcile prior unconfirmed intents even if new recording has been disabled.
// This changes evidence only; it never sends a tool request or retries an effect.
func runConnectorActionSweep(ctx context.Context, store state.ProtectedActionStore) {
	runConnectorActionSweepTick(ctx, store)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runConnectorActionSweepTick(ctx, store)
		}
	}
}

func runConnectorActionSweepTick(ctx context.Context, store state.ProtectedActionStore) {
	ctx, cancel := context.WithTimeout(tenant.SystemContext(ctx), 15*time.Second)
	defer cancel()
	n, err := store.MarkExpiredConnectorActionsUnknown(ctx, time.Now().UTC(), 200)
	if err != nil {
		slog.Warn("connector action reconciliation failed", "error", err)
	} else if n > 0 {
		slog.Info("connector action intents remain unconfirmed", "count", n)
	}
}
