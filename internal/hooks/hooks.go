// Package hooks implements the "Event hooks" platform
// extension (on_run_start, on_run_complete, on_tool_call, on_error,
// on_interrupt), extensible via config (the webhook sink this package
// ships) or via code (any type implementing Sink, registered directly with
// a Dispatcher by anyone embedding runkite as a library).
package hooks

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sharanharsoor/runkite/internal/metrics"
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

type dispatchJob struct {
	sink  Sink
	event Event
}

// Default pool sizing: webhook events fire only on lifecycle
// transitions (a handful per run at most: run_start, run_complete,
// maybe tool_call/interrupt/error), not per-request, so this
// comfortably covers realistic sustained load on a single replica while
// still bounding worst-case goroutine/socket usage to a known ceiling --
// see Dispatcher's own doc comment for what unbounded looked like
// before this existed.
const (
	defaultWebhookWorkers   = 20
	defaultWebhookQueueSize = 500
)

// Dispatcher fans an Event out to every registered Sink whose subscription
// matches the event's type, via a small bounded worker pool rather than
// one goroutine per sink per event. The pool exists specifically to
// bound worst-case resource use: a burst of run completions (e.g. many
// concurrent runs finishing around the same time), each fanning out to
// N configured webhook sinks, each of which can itself hold a goroutine
// open for up to ~30s while retrying with backoff against an unreachable
// endpoint (WebhookSink.Handle) -- unbounded goroutine-per-dispatch grows
// without limit under that combination, which is a real resource-
// exhaustion risk for a production deployment, not a purely theoretical
// one.
//
// Dispatch itself stays non-blocking either way (preserving the
// original "hooks never delay run execution" contract): if every worker
// is busy AND the queue is already full, the job is dropped -- logged
// and counted in runkite_webhook_queue_dropped_total -- rather than
// blocking the caller (which would mean a webhook sink slowness starts
// delaying run completion itself) or growing the queue without bound
// (reintroducing the exact resource-exhaustion risk this exists to
// close). This does trade one new, narrower risk for the old unbounded
// one: because delivery attempts for every sink share this one pool, a
// genuinely slow or hung endpoint can occupy enough workers to delay
// (not silently lose -- WebhookSink's own retry+dead-letter path is
// unaffected once a job IS picked up) another, healthy sink's delivery.
// A well-sized pool makes this rare in practice; it is not eliminated.
type Dispatcher struct {
	mu    sync.RWMutex
	sinks []registeredSink

	jobs chan dispatchJob
	wg   sync.WaitGroup // tracks worker goroutines, for Close to wait on
}

// Option configures a Dispatcher at construction time.
type Option func(*dispatcherConfig)

type dispatcherConfig struct {
	workers   int
	queueSize int
}

// WithWorkerPool overrides the default worker count and queue depth.
// Mainly for tests that want to force queue-full behavior deterministically;
// production callers can use the default via plain NewDispatcher().
func WithWorkerPool(workers, queueSize int) Option {
	return func(c *dispatcherConfig) {
		c.workers = workers
		c.queueSize = queueSize
	}
}

// NewDispatcher returns an empty Dispatcher backed by a bounded worker
// pool (defaultWebhookWorkers workers, defaultWebhookQueueSize queue
// depth, unless overridden via WithWorkerPool). Dispatching to an empty
// Dispatcher (or a nil one -- see the nil-receiver methods below) still
// costs nothing beyond the nil/empty-sinks check; the pool's own workers
// sit idle blocked on an empty channel until something is registered and
// dispatched.
func NewDispatcher(opts ...Option) *Dispatcher {
	cfg := dispatcherConfig{workers: defaultWebhookWorkers, queueSize: defaultWebhookQueueSize}
	for _, opt := range opts {
		opt(&cfg)
	}
	d := &Dispatcher{jobs: make(chan dispatchJob, cfg.queueSize)}
	d.wg.Add(cfg.workers)
	for i := 0; i < cfg.workers; i++ {
		go d.worker()
	}
	return d
}

func (d *Dispatcher) worker() {
	defer d.wg.Done()
	for job := range d.jobs {
		job.sink.Handle(context.Background(), job.event)
	}
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
		select {
		case d.jobs <- dispatchJob{sink: rs.sink, event: event}:
		default:
			slog.Warn("webhook dispatch queue full, dropping event", "event_type", event.Type, "run_id", event.RunID)
			metrics.WebhookQueueDroppedTotal.WithLabelValues(string(event.Type)).Inc()
		}
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

// Close stops the worker pool for graceful shutdown: closes the job
// queue (so every worker finishes whatever job it's currently running,
// then drains whatever was already queued behind it, then exits its
// range loop) and waits up to ctx's deadline for all of them to actually
// exit. Without this, the pool's workers previously ran for the whole
// process lifetime with no shutdown hook at all -- fine for the
// background loops elsewhere in this codebase that intentionally just
// die with the process (nothing there is externally-visible, in-flight
// work), but wrong here: a webhook delivery already queued or in
// progress when SIGTERM arrives would otherwise be silently abandoned
// mid-flight instead of getting the same "let it finish or run out of
// budget" treatment cmd/serve.go's own HTTP/gRPC drain gives every other
// kind of in-flight work.
//
// Returns ctx.Err() if the deadline is hit before every worker exits --
// same "best effort within a bounded budget, not a guarantee" contract
// as srv.Shutdown/grpcServer.GracefulStop in cmd/serve.go's own shutdown
// sequence; any delivery attempt still running past that point is
// abandoned when the process actually exits afterward, exactly as it
// always was before this existed.
//
// Register/Dispatch must not be called after Close (closing an
// already-closed or in-use channel panics) -- in practice this is only
// ever called once, as the very last step of graceful shutdown, by
// which point nothing dispatches new events anymore. Safe to call on a
// nil Dispatcher (no-op, matching Dispatch/HasSinks).
func (d *Dispatcher) Close(ctx context.Context) error {
	if d == nil {
		return nil
	}
	close(d.jobs)
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
