package connector

import (
	"sync"
	"time"
)

// DefaultCircuitBreakerConfig is used when a connector doesn't set its own
// circuit_breaker config.
var DefaultCircuitBreakerConfig = CircuitBreakerConfig{
	FailureThreshold: 5,
	CooldownSeconds:  30,
}

type breakerState int

const (
	stateClosed breakerState = iota
	stateOpen
	stateHalfOpen
)

func (s breakerState) String() string {
	switch s {
	case stateOpen:
		return "open"
	case stateHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// CircuitBreaker is a standard 3-state (closed/open/half-open) breaker,
// one per connector, guarding the actual network call in its token-fetch
// path. api_key/bearer auth never call Allow/RecordSuccess/RecordFailure --
// they don't make network calls, so there's nothing to break on.
//
// closed:    calls pass through; consecutive failures are counted.
// open:      calls are rejected immediately (no network attempt) until the
//
//	cooldown elapses.
//
// half_open: exactly one trial call is let through after cooldown; success
//
//	closes the breaker, failure reopens it (cooldown restarts).
type CircuitBreaker struct {
	cfg CircuitBreakerConfig

	mu                  sync.Mutex
	state               breakerState
	consecutiveFailures int
	openedAt            time.Time
	halfOpenInFlight    bool
}

// NewCircuitBreaker builds a breaker, filling in DefaultCircuitBreakerConfig
// for any zero-valued fields.
func NewCircuitBreaker(cfg *CircuitBreakerConfig) *CircuitBreaker {
	c := DefaultCircuitBreakerConfig
	if cfg != nil {
		if cfg.FailureThreshold > 0 {
			c.FailureThreshold = cfg.FailureThreshold
		}
		if cfg.CooldownSeconds > 0 {
			c.CooldownSeconds = cfg.CooldownSeconds
		}
	}
	return &CircuitBreaker{cfg: c}
}

// ErrCircuitOpen is returned by Allow's caller convention: callers check
// Allow() first and should return this (or wrap it) instead of attempting
// the call.
type ErrCircuitOpen struct {
	Connector string
}

func (e *ErrCircuitOpen) Error() string {
	return "circuit breaker open for connector: " + e.Connector
}

// Allow reports whether a call should be attempted right now. When it
// returns true for a half-open trial, the caller MUST report the outcome
// via RecordSuccess/RecordFailure -- that single call is what decides
// whether the breaker closes or reopens.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case stateClosed:
		return true
	case stateOpen:
		if time.Since(cb.openedAt) < time.Duration(cb.cfg.CooldownSeconds)*time.Second {
			return false
		}
		cb.state = stateHalfOpen
		cb.halfOpenInFlight = true
		return true
	case stateHalfOpen:
		// Only one trial call in flight at a time; concurrent callers
		// during the trial are rejected rather than piling onto a
		// possibly-still-broken endpoint.
		return false
	}
	return true
}

// RecordSuccess resets the failure count and closes the breaker if it was
// trialing in half-open.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.consecutiveFailures = 0
	cb.state = stateClosed
	cb.halfOpenInFlight = false
}

// RecordFailure counts a failure, opening the breaker once the threshold
// is reached (or immediately, if the failure was the half-open trial call).
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.state == stateHalfOpen {
		cb.state = stateOpen
		cb.openedAt = time.Now()
		cb.halfOpenInFlight = false
		return
	}
	cb.consecutiveFailures++
	if cb.consecutiveFailures >= cb.cfg.FailureThreshold {
		cb.state = stateOpen
		cb.openedAt = time.Now()
	}
}

// State reports the current state name, for status/debugging endpoints.
func (cb *CircuitBreaker) State() string {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state.String()
}
