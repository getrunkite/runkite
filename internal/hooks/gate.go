package hooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// DefaultPreflightTimeout is used when a webhook gate config omits timeout_ms.
const DefaultPreflightTimeout = 2 * time.Second

// Decision is a Gate's allow/deny answer for one BeforeRun check.
type Decision struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason,omitempty"`
}

// ErrDenied is returned when a pre-flight Gate rejects a run (or times
// out / errors under fail-closed policy). Callers map it to HTTP 403.
type ErrDenied struct {
	Reason string
}

func (e *ErrDenied) Error() string {
	if e.Reason == "" {
		return "run denied by preflight hook"
	}
	return "run denied by preflight hook: " + e.Reason
}

// Gate is a synchronous pre-flight check. Unlike Sink (observational,
// async, never blocks run creation), Gate.Decide runs inline and can
// reject the run before any state is mutated.
type Gate interface {
	Decide(ctx context.Context, event Event) Decision
}

type registeredGate struct {
	gate Gate
}

// RegisterGate adds a synchronous pre-flight Gate. Safe on a nil
// Dispatcher (no-op). Gates run in registration order; the first deny
// (or fail-closed timeout/error) rejects the run.
func (d *Dispatcher) RegisterGate(gate Gate) {
	if d == nil || gate == nil {
		return
	}
	d.mu.Lock()
	d.gates = append(d.gates, registeredGate{gate: gate})
	d.mu.Unlock()
}

// HasGates reports whether any pre-flight Gate is registered.
func (d *Dispatcher) HasGates() bool {
	if d == nil {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.gates) > 0
}

// CheckBeforeRun runs every registered Gate against event (Type should
// be BeforeRun). Returns nil if all allow, or *ErrDenied on the first
// deny / timeout / gate panic-equivalent failure. Fail-closed: a Gate
// that returns Allow=false, or whose Decide exceeds ctx, denies.
//
// Safe on a nil Dispatcher (no-op allow). Empty gates = allow.
func (d *Dispatcher) CheckBeforeRun(ctx context.Context, event Event) error {
	if d == nil {
		return nil
	}
	d.mu.RLock()
	gates := make([]registeredGate, len(d.gates))
	copy(gates, d.gates)
	d.mu.RUnlock()
	if len(gates) == 0 {
		return nil
	}
	if event.Type == "" {
		event.Type = BeforeRun
	}
	for _, rg := range gates {
		dec, err := decideWithContext(ctx, rg.gate, event)
		if err != nil {
			slog.Warn("preflight gate error (fail-closed deny)", "run_id", event.RunID, "agent_id", event.AgentID, "error", err)
			return &ErrDenied{Reason: "preflight hook failed: " + err.Error()}
		}
		if !dec.Allow {
			reason := dec.Reason
			if reason == "" {
				reason = "denied"
			}
			return &ErrDenied{Reason: reason}
		}
	}
	return nil
}

func decideWithContext(ctx context.Context, gate Gate, event Event) (Decision, error) {
	type result struct {
		dec Decision
	}
	ch := make(chan result, 1)
	go func() {
		// Isolate Decide from the caller's goroutine so a hung HTTP
		// client still respects ctx; Decision itself has no error path.
		ch <- result{dec: gate.Decide(ctx, event)}
	}()
	select {
	case <-ctx.Done():
		return Decision{}, ctx.Err()
	case r := <-ch:
		return r.dec, nil
	}
}

// WebhookGateConfig configures one HTTP pre-flight guardrail endpoint.
type WebhookGateConfig struct {
	URL     string
	Secret  string        // HMAC-SHA256; omit to send unsigned
	Timeout time.Duration // zero → DefaultPreflightTimeout
}

// WebhookGate POSTs the BeforeRun event JSON to URL and parses
// {"allow":bool,"reason":string}. Non-2xx, timeout, or malformed body
// → Allow=false (fail-closed). Same signing header as WebhookSink.
type WebhookGate struct {
	cfg    WebhookGateConfig
	client *http.Client
}

// NewWebhookGate builds a WebhookGate. Timeout defaults to 2s.
func NewWebhookGate(cfg WebhookGateConfig) *WebhookGate {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultPreflightTimeout
	}
	return &WebhookGate{
		cfg: cfg,
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

type gateResponseBody struct {
	Allow  *bool  `json:"allow"`
	Reason string `json:"reason"`
}

// Decide implements Gate.
func (g *WebhookGate) Decide(ctx context.Context, event Event) Decision {
	payload, err := json.Marshal(event)
	if err != nil {
		return Decision{Allow: false, Reason: "failed to marshal preflight event"}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.URL, bytes.NewReader(payload))
	if err != nil {
		return Decision{Allow: false, Reason: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if g.cfg.Secret != "" {
		mac := hmac.New(sha256.New, []byte(g.cfg.Secret))
		mac.Write(payload)
		req.Header.Set("X-Runkite-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return Decision{Allow: false, Reason: fmt.Sprintf("preflight request failed: %v", err)}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reason := string(bytes.TrimSpace(body))
		if reason == "" {
			reason = fmt.Sprintf("preflight HTTP %d", resp.StatusCode)
		}
		return Decision{Allow: false, Reason: reason}
	}

	var parsed gateResponseBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Decision{Allow: false, Reason: "preflight response is not valid JSON"}
	}
	if parsed.Allow == nil {
		return Decision{Allow: false, Reason: "preflight response missing allow field"}
	}
	return Decision{Allow: *parsed.Allow, Reason: parsed.Reason}
}
