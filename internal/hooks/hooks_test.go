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
