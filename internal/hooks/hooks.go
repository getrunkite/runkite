// Package hooks implements the "Event hooks" platform
// extension (on_run_start, on_run_complete, on_tool_call, on_error,
// on_interrupt), extensible via config (the webhook sink this package
// ships) or via code (any type implementing Sink, registered directly with
// a Dispatcher by anyone embedding runkite as a library).
package hooks

import (
	"context"
	"sync"
	"time"
)

// EventType identifies which lifecycle moment fired a hook.
type EventType string

const (
	RunStart    EventType = "run_start"
	RunComplete EventType = "run_complete"
	ToolCall    EventType = "tool_call"
	Error       EventType = "error"
	Interrupt   EventType = "interrupt"
)

// Event is what gets dispatched to every registered Sink.
type Event struct {
	Type      EventType              `json:"type"`
	RunID     string                 `json:"run_id"`
	ThreadID  string                 `json:"thread_id"`
	AgentID   string                 `json:"agent_id"`
	Data      map[string]interface{} `json:"data,omitempty"`
	Timestamp time.Time              `json:"timestamp"`
}

// Sink receives dispatched events. Implement this to hook into runkite
// from Go code; WebhookSink is the config-driven, HTTP-delivery
// implementation this package ships for the common case.
type Sink interface {
	Handle(ctx context.Context, event Event)
}

type registeredSink struct {
	sink   Sink
	events map[EventType]bool // nil/empty means "all event types"
}

// Dispatcher fans an Event out to every registered Sink whose subscription
// matches the event's type. Dispatch is fire-and-forget: each sink runs on
// its own goroutine so a slow or failing sink (e.g. an unreachable webhook
// URL, retried with backoff) never blocks run execution or the caller.
type Dispatcher struct {
	mu    sync.RWMutex
	sinks []registeredSink
}

// NewDispatcher returns an empty Dispatcher. Dispatching to an empty
// Dispatcher (or a nil one -- see the nil-receiver methods below) is a
// no-op, so "no hooks configured" has zero overhead beyond the check.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{}
}

// Register adds sink, called for every event in events (or every event
// type at all, if events is empty).
func (d *Dispatcher) Register(sink Sink, events ...EventType) {
	m := make(map[EventType]bool, len(events))
	for _, e := range events {
		m[e] = true
	}
	d.mu.Lock()
	d.sinks = append(d.sinks, registeredSink{sink: sink, events: m})
	d.mu.Unlock()
}

// Dispatch fans event out to every subscribed sink. Safe to call on a nil
// Dispatcher (no-op) so callers never need to nil-check before dispatching.
func (d *Dispatcher) Dispatch(event Event) {
	if d == nil {
		return
	}
	d.mu.RLock()
	sinks := make([]registeredSink, len(d.sinks))
	copy(sinks, d.sinks)
	d.mu.RUnlock()

	for _, rs := range sinks {
		if len(rs.events) > 0 && !rs.events[event.Type] {
			continue
		}
		go rs.sink.Handle(context.Background(), event)
	}
}

// HasSinks reports whether anything is registered at all, so callers doing
// extra bookkeeping only for hooks (e.g. tailing a run's event stream just
// to detect tool calls) can skip that work entirely when nothing is
// listening.
func (d *Dispatcher) HasSinks() bool {
	if d == nil {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.sinks) > 0
}
