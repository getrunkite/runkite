package hooks

import (
	"context"
	"sync"
	"testing"
	"time"
)

type recordingSink struct {
	mu     sync.Mutex
	events []Event
}

func (r *recordingSink) Handle(_ context.Context, event Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordingSink) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func waitForCount(t *testing.T, sink *recordingSink, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sink.count() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d events, got %d", want, sink.count())
}

func TestDispatch_FiltersBySubscribedEventTypes(t *testing.T) {
	d := NewDispatcher()
	onlyErrors := &recordingSink{}
	everything := &recordingSink{}
	d.Register(onlyErrors, Error)
	d.Register(everything) // no filter -- all types

	d.Dispatch(Event{Type: RunStart, RunID: "r1"})
	d.Dispatch(Event{Type: Error, RunID: "r1"})

	waitForCount(t, everything, 2)
	waitForCount(t, onlyErrors, 1)

	if onlyErrors.events[0].Type != Error {
		t.Errorf("expected onlyErrors to only receive Error events, got %+v", onlyErrors.events)
	}
}

func TestDispatch_NilDispatcherIsNoOp(t *testing.T) {
	var d *Dispatcher
	d.Dispatch(Event{Type: RunStart}) // must not panic
	if d.HasSinks() {
		t.Fatal("nil dispatcher must report no sinks")
	}
}

func TestHasSinks(t *testing.T) {
	d := NewDispatcher()
	if d.HasSinks() {
		t.Fatal("expected no sinks initially")
	}
	d.Register(&recordingSink{})
	if !d.HasSinks() {
		t.Fatal("expected HasSinks to be true after Register")
	}
}

// blockingSink never returns from Handle until unblocked, letting a test
// deterministically saturate a small worker pool.
type blockingSink struct {
	unblock chan struct{}
	started chan struct{} // signals each Handle call actually started
}

func (b *blockingSink) Handle(_ context.Context, _ Event) {
	b.started <- struct{}{}
	<-b.unblock
}

// TestDispatch_BoundedPoolDropsRatherThanUnboundedGoroutines proves the
// worker pool actually bounds concurrency: with 1 worker and a
// queue depth of 1, a sink that never returns can only ever have one
// job running and one more queued -- a third dispatched event must be
// dropped (not spawn an unbounded 3rd goroutine), and Dispatch itself
// must not block the caller either way.
func TestDispatch_BoundedPoolDropsRatherThanUnboundedGoroutines(t *testing.T) {
	d := NewDispatcher(WithWorkerPool(1, 1))
	sink := &blockingSink{unblock: make(chan struct{}), started: make(chan struct{}, 3)}
	d.Register(sink)

	done := make(chan struct{})
	go func() {
		d.Dispatch(Event{Type: RunStart, RunID: "r1"}) // picked up by the 1 worker
		d.Dispatch(Event{Type: RunStart, RunID: "r2"}) // fills the queue (depth 1)
		d.Dispatch(Event{Type: RunStart, RunID: "r3"}) // queue full -- must be dropped, not block
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Dispatch blocked instead of dropping the 3rd job when the pool+queue were saturated")
	}

	// Exactly one Handle call should have actually started (the worker
	// picked up r1); r2 is queued but not yet running, r3 was dropped.
	select {
	case <-sink.started:
	case <-time.After(1 * time.Second):
		t.Fatal("expected the worker to pick up the first job")
	}
	select {
	case <-sink.started:
		t.Fatal("expected only 1 job running concurrently with a 1-worker pool, got a 2nd")
	case <-time.After(100 * time.Millisecond):
	}

	close(sink.unblock) // let r1 finish so r2 can start
	select {
	case <-sink.started:
	case <-time.After(1 * time.Second):
		t.Fatal("expected the queued 2nd job to run once the worker freed up")
	}
	// No 3rd Handle call ever -- it was dropped at Dispatch time.
	select {
	case <-sink.started:
		t.Fatal("expected the 3rd job to have been dropped, not delivered")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestDispatcherClose_WaitsForQueuedDeliveriesToDrain proves the fix for
// the graceful-shutdown gap: a job already queued (not yet picked up by
// any worker) when Close is called must still be allowed to run to
// completion within Close's own budget, not be abandoned the instant
// Close is invoked.
func TestDispatcherClose_WaitsForQueuedDeliveriesToDrain(t *testing.T) {
	d := NewDispatcher(WithWorkerPool(1, 5))
	sink := &recordingSink{}
	d.Register(sink)

	// Queue several jobs faster than the single worker can process them
	// (recordingSink.Handle is effectively instant, but the point is
	// that Close is called immediately after Dispatch returns, before
	// the worker could possibly have drained the queue yet).
	for i := 0; i < 5; i++ {
		d.Dispatch(Event{Type: RunStart, RunID: "r"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Close(ctx); err != nil {
		t.Fatalf("expected Close to drain the queue within budget, got %v", err)
	}
	if sink.count() != 5 {
		t.Fatalf("expected all 5 queued events to be delivered before Close returned, got %d", sink.count())
	}
}

// TestDispatcherClose_ReturnsDeadlineExceededOnSlowSink proves Close
// doesn't hang forever waiting for a delivery that won't finish in
// time -- same bounded-budget contract as srv.Shutdown/GracefulStop in
// cmd/serve.go's own shutdown sequence.
func TestDispatcherClose_ReturnsDeadlineExceededOnSlowSink(t *testing.T) {
	d := NewDispatcher(WithWorkerPool(1, 1))
	sink := &blockingSink{unblock: make(chan struct{}), started: make(chan struct{}, 1)}
	d.Register(sink)
	d.Dispatch(Event{Type: RunStart, RunID: "r"})
	<-sink.started // ensure the worker actually picked it up before racing Close

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := d.Close(ctx)
	close(sink.unblock) // let the stuck worker finish so the test itself doesn't leak it
	if err == nil {
		t.Fatal("expected Close to return an error when the deadline is hit before the worker finishes")
	}
}

// TestDispatcherClose_NilDispatcherIsNoOp matches Dispatch/HasSinks'
// own nil-receiver convention.
func TestDispatcherClose_NilDispatcherIsNoOp(t *testing.T) {
	var d *Dispatcher
	if err := d.Close(context.Background()); err != nil {
		t.Fatalf("expected nil Dispatcher Close to be a no-op, got %v", err)
	}
}

func TestDispatch_MultipleSinksAllReceiveMatchingEvent(t *testing.T) {
	d := NewDispatcher()
	a := &recordingSink{}
	b := &recordingSink{}
	d.Register(a, RunComplete)
	d.Register(b, RunComplete)

	d.Dispatch(Event{Type: RunComplete, RunID: "r1", ThreadID: "t1", AgentID: "agent-x"})

	waitForCount(t, a, 1)
	waitForCount(t, b, 1)
	if a.events[0].AgentID != "agent-x" || b.events[0].ThreadID != "t1" {
		t.Errorf("event fields not propagated correctly: a=%+v b=%+v", a.events[0], b.events[0])
	}
}
