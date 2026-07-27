package connector

import (
	"testing"
	"time"
)

func TestCircuitBreaker_DefaultsWhenConfigNil(t *testing.T) {
	cb := NewCircuitBreaker(nil)
	if cb.cfg.FailureThreshold != DefaultCircuitBreakerConfig.FailureThreshold {
		t.Errorf("expected default threshold, got %d", cb.cfg.FailureThreshold)
	}
	if cb.State() != "closed" {
		t.Errorf("expected initial state closed, got %s", cb.State())
	}
}

func TestCircuitBreaker_PartialConfigFillsDefaults(t *testing.T) {
	cb := NewCircuitBreaker(&CircuitBreakerConfig{FailureThreshold: 2}) // CooldownSeconds unset
	if cb.cfg.FailureThreshold != 2 {
		t.Errorf("expected explicit threshold 2, got %d", cb.cfg.FailureThreshold)
	}
	if cb.cfg.CooldownSeconds != DefaultCircuitBreakerConfig.CooldownSeconds {
		t.Errorf("expected default cooldown, got %d", cb.cfg.CooldownSeconds)
	}
}

func TestCircuitBreaker_ClosedAllowsUntilThreshold(t *testing.T) {
	cb := NewCircuitBreaker(&CircuitBreakerConfig{FailureThreshold: 3, CooldownSeconds: 30})

	for i := 0; i < 2; i++ {
		if !cb.Allow() {
			t.Fatalf("expected Allow() true before threshold, iteration %d", i)
		}
		cb.RecordFailure()
	}
	if cb.State() != "closed" {
		t.Fatalf("expected still closed after 2/3 failures, got %s", cb.State())
	}
	if !cb.Allow() {
		t.Fatal("expected Allow() true for the 3rd attempt")
	}
	cb.RecordFailure() // 3rd failure -- trips the breaker

	if cb.State() != "open" {
		t.Fatalf("expected open after reaching threshold, got %s", cb.State())
	}
	if cb.Allow() {
		t.Fatal("expected Allow() false immediately after opening")
	}
}

func TestCircuitBreaker_SuccessResetsFailureCount(t *testing.T) {
	cb := NewCircuitBreaker(&CircuitBreakerConfig{FailureThreshold: 2, CooldownSeconds: 30})

	cb.Allow()
	cb.RecordFailure()
	cb.Allow()
	cb.RecordSuccess() // resets the count -- should NOT trip on the next single failure

	cb.Allow()
	cb.RecordFailure()
	if cb.State() != "closed" {
		t.Fatalf("expected closed (failure count was reset by success), got %s", cb.State())
	}
}

// forceOpenExpired trips the breaker and back-dates openedAt so the
// cooldown is already considered elapsed, without a real sleep or needing
// CooldownSeconds: 0 (which NewCircuitBreaker treats as "unset, use
// default" -- the same zero-means-default convention as ratelimit.Rule).
func forceOpenExpired(cb *CircuitBreaker, cooldownSeconds int) {
	cb.Allow()
	cb.RecordFailure() // threshold=1 trips it immediately
	cb.mu.Lock()
	cb.openedAt = time.Now().Add(-time.Duration(cooldownSeconds+1) * time.Second)
	cb.mu.Unlock()
}

func TestCircuitBreaker_HalfOpenAfterCooldown(t *testing.T) {
	cb := NewCircuitBreaker(&CircuitBreakerConfig{FailureThreshold: 1, CooldownSeconds: 10})
	forceOpenExpired(cb, 10)
	if cb.State() != "open" {
		t.Fatalf("expected open, got %s", cb.State())
	}

	if !cb.Allow() {
		t.Fatal("expected the trial call to be allowed after cooldown elapses")
	}
	if cb.State() != "half_open" {
		t.Fatalf("expected half_open after cooldown, got %s", cb.State())
	}
	// A second concurrent caller during the trial must be rejected.
	if cb.Allow() {
		t.Fatal("expected a second concurrent call during half-open trial to be rejected")
	}
}

func TestCircuitBreaker_HalfOpenSuccessCloses(t *testing.T) {
	cb := NewCircuitBreaker(&CircuitBreakerConfig{FailureThreshold: 1, CooldownSeconds: 10})
	forceOpenExpired(cb, 10)
	cb.Allow() // half-open trial
	cb.RecordSuccess()

	if cb.State() != "closed" {
		t.Fatalf("expected closed after successful half-open trial, got %s", cb.State())
	}
	if !cb.Allow() {
		t.Fatal("expected normal operation after closing")
	}
}

func TestCircuitBreaker_HalfOpenFailureReopens(t *testing.T) {
	cb := NewCircuitBreaker(&CircuitBreakerConfig{FailureThreshold: 1, CooldownSeconds: 10})
	forceOpenExpired(cb, 10)
	cb.Allow() // half-open trial
	cb.RecordFailure()

	if cb.State() != "open" {
		t.Fatalf("expected reopened after failed half-open trial, got %s", cb.State())
	}
}

func TestErrCircuitOpen_Error(t *testing.T) {
	err := &ErrCircuitOpen{Connector: "salesforce"}
	if err.Error() == "" {
		t.Fatal("expected non-empty error message")
	}
}
