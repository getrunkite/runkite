package hooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
)

type fakeDeadLetterStore struct {
	mu    sync.Mutex
	saved []*models.WebhookDeadLetter
}

func (f *fakeDeadLetterStore) SaveWebhookDeadLetter(_ context.Context, dl *models.WebhookDeadLetter) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, dl)
	return nil
}

func (f *fakeDeadLetterStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.saved)
}

func TestWebhookSink_SuccessfulDeliveryNoRetryNoDeadLetter(t *testing.T) {
	var calls int32
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dl := &fakeDeadLetterStore{}
	sink := NewWebhookSink(WebhookConfig{URL: srv.URL}, dl)
	sink.Handle(context.Background(), Event{Type: RunComplete, RunID: "r1", Timestamp: time.Now()})

	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("expected exactly 1 delivery attempt on success, got %d", calls)
	}
	if dl.count() != 0 {
		t.Fatalf("expected no dead letter on success, got %d", dl.count())
	}
	var decoded Event
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("delivered body did not decode as Event: %v", err)
	}
	if decoded.RunID != "r1" || decoded.Type != RunComplete {
		t.Errorf("delivered event mismatch: %+v", decoded)
	}
}

func TestWebhookSink_HMACSignatureIsValid(t *testing.T) {
	secret := "test-secret"
	var gotSig string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Runkite-Signature")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := NewWebhookSink(WebhookConfig{URL: srv.URL, Secret: secret}, nil)
	sink.Handle(context.Background(), Event{Type: Error, RunID: "r2"})

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(gotBody)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Errorf("signature mismatch: got %q, want %q", gotSig, want)
	}
}

func TestWebhookSink_RetriesThenDeadLettersOnPersistentFailure(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dl := &fakeDeadLetterStore{}
	sink := NewWebhookSink(WebhookConfig{URL: srv.URL}, dl)

	start := time.Now()
	sink.Handle(context.Background(), Event{Type: RunComplete, RunID: "r3", TenantID: "tenant-x"})
	elapsed := time.Since(start)

	if atomic.LoadInt32(&calls) != maxDeliveryAttempts {
		t.Fatalf("expected %d delivery attempts, got %d", maxDeliveryAttempts, calls)
	}
	// Backoff is 500ms + 1000ms between the 3 attempts -- must have taken
	// at least that long (proves retries actually waited, not busy-looped).
	if elapsed < initialBackoff+2*initialBackoff {
		t.Errorf("expected retries to include backoff delay, took only %v", elapsed)
	}
	if dl.count() != 1 {
		t.Fatalf("expected exactly 1 dead letter after exhausting retries, got %d", dl.count())
	}
	saved := dl.saved[0]
	if saved.RunID != "r3" || saved.Attempts != maxDeliveryAttempts || saved.URL != srv.URL {
		t.Errorf("dead letter fields wrong: %+v", saved)
	}
	if saved.TenantID != "tenant-x" {
		t.Errorf("TenantID = %q, want tenant-x", saved.TenantID)
	}
}

func TestWebhookSink_SucceedsOnRetryAfterTransientFailure(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dl := &fakeDeadLetterStore{}
	sink := NewWebhookSink(WebhookConfig{URL: srv.URL}, dl)
	sink.Handle(context.Background(), Event{Type: RunComplete, RunID: "r4"})

	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected exactly 2 attempts (fail then succeed), got %d", calls)
	}
	if dl.count() != 0 {
		t.Fatal("expected no dead letter when a retry eventually succeeds")
	}
}

func TestWebhookSink_NilDeadLetterStoreDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sink := NewWebhookSink(WebhookConfig{URL: srv.URL}, nil) // nil dead-letter store
	sink.Handle(context.Background(), Event{Type: RunComplete, RunID: "r5"})
	// Reaching here without panicking is the assertion.
}
