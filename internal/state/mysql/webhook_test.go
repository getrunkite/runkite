package mysql_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
)

// Mirrors the shared conformance suite's runWebhookDeadLetterTests.
// webhook_dead_letters carries no tenant_id column (see SaveWebhookDeadLetter's
// doc comment), so there's no tenant-isolation case here -- same as
// every other backend.

func TestWebhook_SaveAndListOrderedNewestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC()

	for i, id := range []string{"dl-1", "dl-2", "dl-3"} {
		dl := &models.WebhookDeadLetter{
			ID: id, URL: "https://example.com/hook", EventType: "run_complete",
			RunID: "run-" + id, Payload: json.RawMessage(`{"type":"run_complete"}`),
			Error: "connection refused", Attempts: 3,
			FailedAt: base.Add(time.Duration(i) * time.Second),
		}
		if err := s.SaveWebhookDeadLetter(ctx, dl); err != nil {
			t.Fatalf("SaveWebhookDeadLetter(%s): %v", id, err)
		}
	}

	got, err := s.ListWebhookDeadLetters(ctx, 10)
	if err != nil {
		t.Fatalf("ListWebhookDeadLetters: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 dead letters, got %d", len(got))
	}
	if got[0].ID != "dl-3" || got[2].ID != "dl-1" {
		t.Errorf("expected newest-first order, got %v, %v, %v", got[0].ID, got[1].ID, got[2].ID)
	}
	if got[0].Attempts != 3 || got[0].Error != "connection refused" || got[0].URL != "https://example.com/hook" {
		t.Errorf("dead letter fields not preserved: %+v", got[0])
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil || payload["type"] != "run_complete" {
		t.Errorf("payload not preserved: %s (err=%v)", got[0].Payload, err)
	}
}

func TestWebhook_ListRespectsLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := s.SaveWebhookDeadLetter(ctx, &models.WebhookDeadLetter{
			ID: fmt.Sprintf("dl-limit-%d", i), URL: "u", EventType: "error", RunID: "r",
			Payload: json.RawMessage(`{}`), FailedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("SaveWebhookDeadLetter(%d): %v", i, err)
		}
	}
	got, err := s.ListWebhookDeadLetters(ctx, 2)
	if err != nil {
		t.Fatalf("ListWebhookDeadLetters: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected limit=2 to be respected, got %d results", len(got))
	}
}

func TestWebhook_ListEmptyWhenNoneSaved(t *testing.T) {
	s := newTestStore(t)
	got, err := s.ListWebhookDeadLetters(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListWebhookDeadLetters: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no dead letters, got %d", len(got))
	}
}
