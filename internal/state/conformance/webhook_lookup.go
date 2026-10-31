package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func runWebhookLookupTests(t *testing.T, factory StoreFactory) {
	s := factory(t)
	lookup, ok := s.(state.WebhookDeadLetterLookup)
	if !ok {
		t.Skip("custom store does not implement optional dead-letter lookup")
	}
	ctxA := tenant.WithContext(context.Background(), "lookup-tenant-a")
	ctxB := tenant.WithContext(context.Background(), "lookup-tenant-b")
	system := tenant.SystemContext(context.Background())
	base := time.Now().UTC().Round(time.Second).Add(-time.Hour)
	old := &models.WebhookDeadLetter{
		ID: "lookup-old-letter", URL: "https://example.invalid/old-hook", EventType: "run_complete", RunID: "old-run",
		Payload: json.RawMessage(`{"event":"old"}`), Error: "synthetic failed delivery", Attempts: 3, FailedAt: base,
	}
	if err := s.SaveWebhookDeadLetter(ctxA, old); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1001; i++ {
		if err := s.SaveWebhookDeadLetter(ctxB, &models.WebhookDeadLetter{
			ID: fmt.Sprintf("lookup-new-%04d", i), URL: "https://example.invalid/new-hook", EventType: "run_complete",
			Payload: json.RawMessage(`{}`), FailedAt: base.Add(time.Duration(i+1) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	recent, err := s.ListWebhookDeadLetters(system, 1000)
	if err != nil || len(recent) != 1000 {
		t.Fatalf("recent list: count=%d err=%v", len(recent), err)
	}
	for _, row := range recent {
		if row.ID == old.ID {
			t.Fatal("fixture must place the old ID beyond the redelivery scan window")
		}
	}
	for _, ctx := range []context.Context{ctxA, system} {
		got, err := lookup.GetWebhookDeadLetter(ctx, old.ID)
		if err != nil || got == nil {
			t.Fatalf("indexed lookup failed for an existing old ID: row=%+v err=%v", got, err)
		}
		if got.ID != old.ID || got.TenantID != "lookup-tenant-a" || got.URL != old.URL || got.EventType != old.EventType || got.RunID != old.RunID || got.Error != old.Error || got.Attempts != old.Attempts || !got.FailedAt.Equal(base) {
			t.Fatalf("lookup changed delivery metadata: %+v", got)
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(got.Payload, &payload); err != nil || payload["event"] != "old" {
			t.Fatalf("lookup changed delivery payload: %s err=%v", got.Payload, err)
		}
	}
	for _, request := range []struct {
		ctx context.Context
		id  string
	}{{ctxB, old.ID}, {system, "lookup-missing"}, {system, strings.ToUpper(old.ID)}} {
		got, err := lookup.GetWebhookDeadLetter(request.ctx, request.id)
		var missing *state.ErrNotFound
		if got != nil || !errors.As(err, &missing) || missing.Resource != "webhook_dead_letter" {
			t.Fatalf("missing/foreign lookup should be not found: id=%q row=%+v err=%v", request.id, got, err)
		}
	}
}
