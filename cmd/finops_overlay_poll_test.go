package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/finops"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestSyncFinOpsOverlayIfChanged_SiblingSeesAdminWrite(t *testing.T) {
	store, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	writer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	sibling := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	base := &finops.Config{
		Pricebook: finops.Pricebook{"base": {InputPer1k: 1, OutputPer1k: 2}},
	}
	writer.SetFinOps(base)
	sibling.SetFinOps(base)

	sys := tenant.SystemContext(ctx)
	prev, err := syncFinOpsOverlayIfChanged(sys, store, sibling, "0")
	if err != nil {
		t.Fatal(err)
	}
	if prev != "0" {
		// no row yet
		t.Fatalf("seed fp = %q", prev)
	}

	payload, _ := json.Marshal(map[string]any{
		"pricebook": map[string]any{
			"sibling": map[string]any{"input_per_1k": 0.5, "output_per_1k": 1.0},
		},
	})
	if err := store.UpsertFinOpsOverlay(sys, &models.FinOpsOverlay{
		ID: models.FinOpsOverlayID, Payload: payload, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	writer.ReloadFinOpsOverlays(sys)

	next, err := syncFinOpsOverlayIfChanged(sys, store, sibling, prev)
	if err != nil {
		t.Fatal(err)
	}
	if next == prev {
		t.Fatal("expected fingerprint change")
	}
	eff := sibling.FinOps()
	if eff == nil || eff.Pricebook["sibling"].InputPer1k != 0.5 {
		t.Fatalf("sibling did not pick up overlay: %#v", eff)
	}
	if eff.Pricebook["base"].InputPer1k != 1 {
		t.Fatalf("sibling lost baseline: %#v", eff)
	}
}
