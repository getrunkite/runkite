package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

// policyOverlayPollInterval matches cron's tick cadence: grant changes are
// rare, and sibling replicas can tolerate up to this lag before Decide
// sees a new Admin overlay.
const policyOverlayPollInterval = 15 * time.Second

// policyGrantsFingerprint is a cheap change detector for the poll loop.
// COUNT alone misses in-place updates; MAX(updated_at) alone misses deletes
// that leave the prior max intact — so we hash id + updated_at for every row.
func policyGrantsFingerprint(rows []*models.PolicyGrant) string {
	if len(rows) == 0 {
		return "0"
	}
	ids := make([]string, 0, len(rows))
	byID := make(map[string]*models.PolicyGrant, len(rows))
	for _, g := range rows {
		if g == nil || g.ID == "" {
			continue
		}
		ids = append(ids, g.ID)
		byID[g.ID] = g
	}
	sort.Strings(ids)
	var b strings.Builder
	fmt.Fprintf(&b, "%d", len(ids))
	for _, id := range ids {
		g := byID[id]
		fmt.Fprintf(&b, "|%s|%d", id, g.UpdatedAt.UTC().UnixNano())
	}
	return b.String()
}

// syncPolicyOverlaysIfChanged reloads the engine when the SQL grant table
// fingerprint differs from prev. Returns the current fingerprint.
func syncPolicyOverlaysIfChanged(ctx context.Context, store policyGrantLister, apiServer *api.Server, prev string) (string, error) {
	rows, err := store.ListPolicyGrants(tenant.SystemContext(ctx))
	if err != nil {
		return prev, err
	}
	fp := policyGrantsFingerprint(rows)
	if fp == prev {
		return prev, nil
	}
	apiServer.ReloadPolicyOverlays(tenant.SystemContext(ctx))
	slog.Info("policy: overlays reloaded from poll", "grants", len(rows))
	return fp, nil
}

// runPolicyOverlayPoll keeps each control-plane replica's in-process
// overlays aligned with SQL after Admin writes that hit a different
// replica. The writing replica still reloads immediately in the Admin
// handlers; this loop closes the sibling lag. Runs until ctx is cancelled.
func runPolicyOverlayPoll(ctx context.Context, store policyGrantLister, apiServer *api.Server) {
	ticker := time.NewTicker(policyOverlayPollInterval)
	defer ticker.Stop()

	fp := ""
	if rows, err := store.ListPolicyGrants(tenant.SystemContext(ctx)); err != nil {
		slog.Warn("policy: overlay poll seed failed", "error", err)
	} else {
		fp = policyGrantsFingerprint(rows)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		next, err := syncPolicyOverlaysIfChanged(ctx, store, apiServer, fp)
		if err != nil {
			slog.Warn("policy: overlay poll failed", "error", err)
			continue
		}
		fp = next
	}
}
