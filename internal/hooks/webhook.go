package hooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// DeadLetterStore persists deliveries that failed every retry attempt,
// as part of webhook delivery's retry-and-dead-letter handling. Satisfied
// by state.Store.
type DeadLetterStore interface {
	SaveWebhookDeadLetter(ctx context.Context, dl *models.WebhookDeadLetter) error
}

// WebhookConfig configures one webhook subscription.
type WebhookConfig struct {
	URL    string
	Secret string // HMAC-SHA256 signing secret; omit to send unsigned
	Events []EventType
}

const (
	maxDeliveryAttempts = 3
	initialBackoff      = 500 * time.Millisecond
)

// WebhookSink delivers Events as signed HTTP POSTs, with exponential-backoff
// retry and a persisted dead-letter on exhaustion.
type WebhookSink struct {
	cfg        WebhookConfig
	client     *http.Client
	deadLetter DeadLetterStore // nil is valid: failed deliveries are only logged, not persisted
}

func NewWebhookSink(cfg WebhookConfig, deadLetter DeadLetterStore) *WebhookSink {
	return &WebhookSink{
		cfg:        cfg,
		client:     &http.Client{Timeout: 10 * time.Second},
		deadLetter: deadLetter,
	}
}

// Handle implements Sink. Runs on its own goroutine per Dispatcher.Dispatch
// call, so blocking here (across retries with backoff) never delays run
// execution or other sinks.
func (w *WebhookSink) Handle(ctx context.Context, event Event) {
	payload, err := json.Marshal(event)
	if err != nil {
		slog.Error("webhook: failed to marshal event", "url", w.cfg.URL, "error", err)
		return
	}

	backoff := initialBackoff
	var lastErr error
	for attempt := 1; attempt <= maxDeliveryAttempts; attempt++ {
		if lastErr = w.deliver(ctx, payload); lastErr == nil {
			return
		}
		slog.Warn("webhook delivery failed", "url", w.cfg.URL, "event_type", event.Type, "attempt", attempt, "error", lastErr)
		if attempt < maxDeliveryAttempts {
			time.Sleep(backoff)
			backoff *= 2
		}
	}

	slog.Error("webhook delivery exhausted retries, dead-lettering", "url", w.cfg.URL, "event_type", event.Type, "run_id", event.RunID)
	if w.deadLetter == nil {
		return
	}
	tid := event.TenantID
	if tid == "" {
		tid = tenant.DefaultTenant
	}
	dl := &models.WebhookDeadLetter{
		ID:        uuid.New().String(),
		TenantID:  tid,
		URL:       w.cfg.URL,
		EventType: string(event.Type),
		RunID:     event.RunID,
		Payload:   payload,
		Error:     lastErr.Error(),
		Attempts:  maxDeliveryAttempts,
		FailedAt:  time.Now().UTC(),
	}
	// Deliberately not the caller's ctx -- it may already be cancelled
	// (e.g. request context from whatever triggered the event); persisting
	// a dead letter should still happen. Tenant is threaded via
	// WithContext so Save sees the same tenant as the originating run.
	saveCtx := tenant.WithContext(context.Background(), tid)
	if err := w.deadLetter.SaveWebhookDeadLetter(saveCtx, dl); err != nil {
		slog.Error("webhook: failed to persist dead letter", "url", w.cfg.URL, "error", err)
	}
}

func (w *WebhookSink) deliver(ctx context.Context, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.URL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.cfg.Secret != "" {
		mac := hmac.New(sha256.New, []byte(w.cfg.Secret))
		mac.Write(payload)
		req.Header.Set("X-Runkite-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook endpoint returned status %d", resp.StatusCode)
	}
	return nil
}
