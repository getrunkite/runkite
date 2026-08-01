// Package inprocess provides in-memory implementations of JobQueue,
// EventBroker, and CancelBroker for conformance testing and local
// zero-dependency mode.
package inprocess

import (
	"context"
	"sync"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
)

type inflightEntry struct {
	job        *transport.RunAssignment
	dequeuedAt time.Time
}

// Queue is an in-memory FIFO job queue with per-runner_kind channels.
type Queue struct {
	mu       sync.Mutex
	jobs     map[string]chan *transport.RunAssignment // keyed by runner_kind
	canceled map[string]bool                          // run_ids that have been canceled
	inflight map[string]*inflightEntry                // run_id -> dequeued but not yet Ack'd
}

// NewQueue creates a new in-memory queue.
func NewQueue() *Queue {
	return &Queue{
		jobs:     make(map[string]chan *transport.RunAssignment),
		canceled: make(map[string]bool),
		inflight: make(map[string]*inflightEntry),
	}
}

func (q *Queue) getChannel(runnerKind string) chan *transport.RunAssignment {
	q.mu.Lock()
	defer q.mu.Unlock()
	ch, ok := q.jobs[runnerKind]
	if !ok {
		ch = make(chan *transport.RunAssignment, 1000)
		q.jobs[runnerKind] = ch
	}
	return ch
}

// Enqueue adds a job to the queue for the job's runner_kind.
func (q *Queue) Enqueue(ctx context.Context, job *transport.RunAssignment) error {
	q.mu.Lock()
	if q.canceled[job.RunID] {
		q.mu.Unlock()
		return nil // silently drop canceled jobs
	}
	q.mu.Unlock()

	ch := q.getChannel(job.RunnerKind)
	select {
	case ch <- job:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Dequeue blocks until a non-canceled job is available for the given runner_kind or timeout.
// Canceled jobs are silently discarded and the wait continues.
func (q *Queue) Dequeue(ctx context.Context, runnerKind string, timeout time.Duration) (*transport.RunAssignment, error) {
	ch := q.getChannel(runnerKind)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case job := <-ch:
			q.mu.Lock()
			if q.canceled[job.RunID] {
				q.mu.Unlock()
				continue // skip canceled job, keep waiting
			}
			q.inflight[job.RunID] = &inflightEntry{job: job, dequeuedAt: time.Now()}
			q.mu.Unlock()
			return job, nil
		case <-timer.C:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// generationMatches implements the fencing rule shared by Ack and Renew
// -- see transport.JobQueue's own doc comments for the full rationale.
// generation 0 (pre-fencing runner build) or a tracked job whose own
// Generation is 0 (predates this field) both mean "don't enforce
// fencing," matching how Ack/Renew behaved unconditionally before
// fencing existed.
func generationMatches(entry *inflightEntry, generation int64) bool {
	if generation == 0 || entry.job.Generation == 0 {
		return true
	}
	return entry.job.Generation == generation
}

// Ack removes a job from inflight tracking after the runner has claimed
// it, fenced by generation -- see transport.JobQueue's Ack doc comment
// for the full rationale. accepted=false (not an error) if nothing is
// tracked for runID, or a newer generation has already superseded it.
func (q *Queue) Ack(ctx context.Context, runID string, generation int64) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	entry, ok := q.inflight[runID]
	if !ok || !generationMatches(entry, generation) {
		return false, nil
	}
	delete(q.inflight, runID)
	return true, nil
}

// Renew resets an in-flight job's dequeuedAt timestamp without removing
// it from tracking -- see transport.JobQueue's Renew doc comment for the
// heartbeat mechanism and fencing this backs. current=false (not an
// error) if the job is no longer in-flight (already Ack'd/reclaimed/
// canceled) or has been superseded by a newer generation.
func (q *Queue) Renew(ctx context.Context, runID string, generation int64) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	entry, ok := q.inflight[runID]
	if !ok || !generationMatches(entry, generation) {
		return false, nil
	}
	entry.dequeuedAt = time.Now()
	return true, nil
}

// Nack re-enqueues a previously dequeued job so another runner can take it.
func (q *Queue) Nack(ctx context.Context, runID string) error {
	q.mu.Lock()
	entry, ok := q.inflight[runID]
	if !ok {
		q.mu.Unlock()
		return nil
	}
	delete(q.inflight, runID)
	canceled := q.canceled[runID]
	q.mu.Unlock()

	if canceled {
		return nil
	}
	return q.Enqueue(ctx, entry.job)
}

// Cancel marks a run_id as canceled. Enqueued jobs with this ID are skipped
// on Dequeue; new Enqueue calls for the ID are dropped.
func (q *Queue) Cancel(ctx context.Context, runID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.canceled[runID] = true
	delete(q.inflight, runID)
	return nil
}

// Len returns the total number of jobs across all runner_kind channels.
func (q *Queue) Len(ctx context.Context) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var total int64
	for _, ch := range q.jobs {
		total += int64(len(ch))
	}
	return total, nil
}

// Ping always succeeds -- there is no external dependency to be down.
// This is exactly why the in-process transport exists (zero-dependency,
// single-instance mode); a readiness check has nothing meaningful to
// verify here beyond the process itself already being alive.
func (q *Queue) Ping(ctx context.Context) error {
	return nil
}

// ReclaimStale re-enqueues jobs that were dequeued more than maxAge ago
// without being Ack'd. This recovers jobs stolen by a dead runner's
// zombie GetJob long-poll (or any crash between Dequeue and first event).
func (q *Queue) ReclaimStale(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge)
	q.mu.Lock()
	var stale []*transport.RunAssignment
	for runID, entry := range q.inflight {
		if entry.dequeuedAt.Before(cutoff) {
			// Bump generation before redispatching -- see
			// RunAssignment.Generation's own doc comment for why:
			// whichever runner Dequeues this next receives the
			// incremented value, so the ORIGINAL runner's own later
			// Heartbeat/ReportStatus (if its blip was transient and it
			// finishes anyway) presents the OLD generation and gets
			// rejected instead of clobbering this new attempt's work.
			entry.job.Generation++
			stale = append(stale, entry.job)
			delete(q.inflight, runID)
		}
	}
	q.mu.Unlock()

	for _, job := range stale {
		if err := q.Enqueue(ctx, job); err != nil {
			return 0, err
		}
	}
	return len(stale), nil
}

// --------------------------------------------------------------------------
// EventBroker
// --------------------------------------------------------------------------

// Broker is an in-memory event broker with per-run fan-out and replay buffer.
type Broker struct {
	mu          sync.RWMutex
	subscribers map[string][]chan *transport.RunEvent // run_id -> subscriber channels
	buffer      map[string][]*transport.RunEvent      // run_id -> event history for replay
	closed      map[string]bool                       // run_id -> whether the stream is closed
}

// NewBroker creates a new in-memory event broker.
func NewBroker() *Broker {
	return &Broker{
		subscribers: make(map[string][]chan *transport.RunEvent),
		buffer:      make(map[string][]*transport.RunEvent),
		closed:      make(map[string]bool),
	}
}

func (b *Broker) Publish(ctx context.Context, runID string, event *transport.RunEvent) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buffer[runID] = append(b.buffer[runID], event)

	for _, ch := range b.subscribers[runID] {
		select {
		case ch <- event:
		default:
			// Drop if subscriber is too slow; buffered channels absorb normal load.
		}
	}

	if event.IsTerminal() {
		b.closed[runID] = true
		for _, ch := range b.subscribers[runID] {
			close(ch)
		}
		b.subscribers[runID] = nil
	}
	return nil
}

func (b *Broker) Subscribe(ctx context.Context, runID string) (<-chan *transport.RunEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan *transport.RunEvent, 4096)
	if b.closed[runID] {
		close(ch)
		return ch, nil
	}
	b.subscribers[runID] = append(b.subscribers[runID], ch)
	return ch, nil
}

func (b *Broker) Replay(ctx context.Context, runID string, sinceSeq int64) ([]*transport.RunEvent, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var out []*transport.RunEvent
	for _, ev := range b.buffer[runID] {
		if ev.Seq > sinceSeq {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (b *Broker) Close(runID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.closed[runID] = true
	for _, ch := range b.subscribers[runID] {
		close(ch)
	}
	b.subscribers[runID] = nil
	return nil
}

// --------------------------------------------------------------------------
// CancelBroker
// --------------------------------------------------------------------------

// CancelBus is an in-memory cancel signal broker.
type CancelBus struct {
	mu   sync.Mutex
	subs map[string][]chan struct{}
}

// NewCancelBus creates a new in-memory cancel broker.
func NewCancelBus() *CancelBus {
	return &CancelBus{subs: make(map[string][]chan struct{})}
}

func (c *CancelBus) PublishCancel(ctx context.Context, runID string) error {
	c.mu.Lock()
	subs := c.subs[runID]
	delete(c.subs, runID)
	c.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
		close(ch)
	}
	return nil
}

func (c *CancelBus) SubscribeCancel(ctx context.Context, runID string) (<-chan struct{}, error) {
	ch := make(chan struct{}, 1)
	c.mu.Lock()
	c.subs[runID] = append(c.subs[runID], ch)
	c.mu.Unlock()

	// ctx cancellation must release this subscription (see
	// CancelBroker's doc comment) -- without this, c.subs grows by one
	// entry per run forever for any run that completes without ever
	// being cancelled (the common case), an unbounded in-memory leak on
	// the zero-dependency default transport. Same bug class as the
	// Redis implementation's (much costlier, goroutine+connection)
	// leak, just found by inspection here rather than via pprof.
	go func() {
		<-ctx.Done()
		c.mu.Lock()
		defer c.mu.Unlock()
		subs := c.subs[runID]
		for i, sub := range subs {
			if sub == ch {
				c.subs[runID] = append(subs[:i], subs[i+1:]...)
				if len(c.subs[runID]) == 0 {
					delete(c.subs, runID)
				}
				return
			}
		}
		// Not found -- a real cancel already fired and PublishCancel
		// removed it first; nothing left to clean up.
	}()

	return ch, nil
}

// Compile-time interface checks.
var (
	_ transport.JobQueue     = (*Queue)(nil)
	_ transport.EventBroker  = (*Broker)(nil)
	_ transport.CancelBroker = (*CancelBus)(nil)
)
