package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

// finopsOverlayPollInterval matches policy overlay sibling lag.
const finopsOverlayPollInterval = 15 * time.Second

type finopsOverlayLister interface {
	GetFinOpsOverlay(ctx context.Context) (*models.FinOpsOverlay, error)
}

func finopsOverlayFingerprint(row *models.FinOpsOverlay) string {
	if row == nil {
		return "0"
	}
	return fmt.Sprintf("1|%s|%d", row.ID, row.UpdatedAt.UTC().UnixNano())
}

func syncFinOpsOverlayIfChanged(ctx context.Context, store finopsOverlayLister, apiServer *api.Server, prev string) (string, error) {
	sys := tenant.SystemContext(ctx)
	row, err := store.GetFinOpsOverlay(sys)
	if err != nil {
		return prev, err
	}
	fp := finopsOverlayFingerprint(row)
	if fp == prev {
		return prev, nil
	}
	apiServer.ReloadFinOpsOverlays(sys)
	slog.Info("finops: overlay reloaded from poll", "has_overlay", row != nil)
	return fp, nil
}

func runFinOpsOverlayPoll(ctx context.Context, store finopsOverlayLister, apiServer *api.Server) {
	ticker := time.NewTicker(finopsOverlayPollInterval)
	defer ticker.Stop()

	fp := ""
	sys := tenant.SystemContext(ctx)
	if row, err := store.GetFinOpsOverlay(sys); err != nil {
		slog.Warn("finops: overlay poll seed failed", "error", err)
	} else {
		fp = finopsOverlayFingerprint(row)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		next, err := syncFinOpsOverlayIfChanged(ctx, store, apiServer, fp)
		if err != nil {
			slog.Warn("finops: overlay poll failed", "error", err)
			continue
		}
		fp = next
	}
}
