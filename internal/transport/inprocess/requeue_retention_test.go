package inprocess

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
)

func fullReadyQueueWithLease(t *testing.T) (*Queue, *transport.RunAssignment) {
	t.Helper()
	q, assignments := fullReadyQueueWithLeases(t, 1)
	return q, assignments[0]
}

func fullReadyQueueWithLeases(t *testing.T, count int) (*Queue, []*transport.RunAssignment) {
	t.Helper()
	q := NewQueue()
	ctx := context.Background()
	var assignments []*transport.RunAssignment
	for i := 0; i < count; i++ {
		if err := q.Enqueue(ctx, &transport.RunAssignment{RunID: fmt.Sprintf("retained-%d", i), RunnerKind: "kind", Generation: 1}); err != nil {
			t.Fatal(err)
		}
		assignment, err := q.Dequeue(ctx, "kind", time.Second)
		if err != nil || assignment == nil {
			t.Fatalf("dequeue retained assignment: %+v %v", assignment, err)
		}
		q.mu.Lock()
		q.inflight[assignment.RunID].dequeuedAt = time.Now().Add(-time.Hour)
		q.mu.Unlock()
		assignments = append(assignments, assignment)
	}
	for i := 0; i < cap(q.getChannel("kind")); i++ {
		if err := q.Enqueue(ctx, &transport.RunAssignment{RunID: fmt.Sprintf("ready-%d", i), RunnerKind: "kind"}); err != nil {
			t.Fatal(err)
		}
	}
	return q, assignments
}

func requireRetainedLease(t *testing.T, q *Queue, assignment *transport.RunAssignment) {
	t.Helper()
	current, err := q.LookupInflight(context.Background(), assignment.RunID)
	if err != nil || current == nil || current.Generation != assignment.Generation {
		t.Fatalf("failed requeue lost its inflight generation: got=%+v original=%+v err=%v", current, assignment, err)
	}
}

func TestQueueFailedRequeueRetainsLease(t *testing.T) {
	for _, op := range []string{"nack", "reclaim"} {
		for _, cancelFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/canceled=%v", op, cancelFirst), func(t *testing.T) {
				q, assignment := fullReadyQueueWithLease(t)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if cancelFirst {
					cancel()
				}
				var err error
				if op == "nack" {
					err = q.Nack(ctx, assignment.RunID, assignment.Generation)
				} else {
					var n int
					n, _, err = q.ReclaimStale(ctx, time.Minute, 0)
					if n != 0 {
						t.Errorf("failed reclaim advanced %d assignments", n)
					}
				}
				if err == nil {
					t.Fatal("full/canceled requeue did not report failure")
				}
				if !cancelFirst && errors.Is(err, context.DeadlineExceeded) {
					t.Error("full ready buffer blocked until the caller's deadline")
				}
				requireRetainedLease(t, q, assignment)
				if depth, err := q.Len(context.Background()); err != nil || depth != int64(cap(q.getChannel("kind"))) {
					t.Fatalf("failed requeue changed ready contents: depth=%d err=%v", depth, err)
				}
				// A later retry succeeds after a worker frees one ready slot.
				ready, err := q.Dequeue(context.Background(), "kind", time.Second)
				if err != nil || ready == nil {
					t.Fatal(ready, err)
				}
				if _, err := q.Ack(context.Background(), ready.RunID, ready.Generation); err != nil {
					t.Fatal(err)
				}
				wantedGeneration := assignment.Generation
				if op == "nack" {
					if err := q.Nack(context.Background(), assignment.RunID, assignment.Generation); err != nil {
						t.Fatal(err)
					}
				} else {
					wantedGeneration++
					if n, _, err := q.ReclaimStale(context.Background(), time.Minute, 0); err != nil || n != 1 {
						t.Fatalf("reclaim retry: n=%d err=%v", n, err)
					}
				}
				found := false
				for i := 0; i < cap(q.getChannel("kind")); i++ {
					job, err := q.Dequeue(context.Background(), "kind", time.Second)
					if err != nil || job == nil {
						t.Fatalf("drain ready job: %+v %v", job, err)
					}
					if job.RunID == assignment.RunID {
						if found || job.Generation != wantedGeneration {
							t.Fatalf("retry duplicated or changed dispatch generation: %+v", job)
						}
						found = true
					}
					if _, err := q.Ack(context.Background(), job.RunID, job.Generation); err != nil {
						t.Fatal(err)
					}
				}
				if !found {
					t.Fatal("retained assignment was lost after retry")
				}
			})
		}
	}
}

func TestQueueStaleNackDoesNotTouchFullReadyOrNewLease(t *testing.T) {
	q, assignment := fullReadyQueueWithLease(t)
	q.mu.Lock()
	q.inflight[assignment.RunID].job.Generation = 2
	q.mu.Unlock()
	if err := q.Nack(context.Background(), assignment.RunID, 1); err != nil {
		t.Fatal(err)
	}
	assignment.Generation = 2
	requireRetainedLease(t, q, assignment)
}

func TestQueueCanceledRequeueWithCapacityRetainsLease(t *testing.T) {
	for _, op := range []string{"nack", "reclaim"} {
		t.Run(op, func(t *testing.T) {
			q, assignment := fullReadyQueueWithLease(t)
			ready, err := q.Dequeue(context.Background(), "kind", time.Second)
			if err != nil || ready == nil {
				t.Fatal(ready, err)
			}
			if _, err := q.Ack(context.Background(), ready.RunID, ready.Generation); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if op == "nack" {
				err = q.Nack(ctx, assignment.RunID, assignment.Generation)
			} else {
				_, _, err = q.ReclaimStale(ctx, time.Minute, 0)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("known-canceled context committed a ready write: %v", err)
			}
			requireRetainedLease(t, q, assignment)
		})
	}
}

func TestQueuePartialReclaimRetainsEveryUncommittedLease(t *testing.T) {
	q, assignments := fullReadyQueueWithLeases(t, 2)
	ready, err := q.Dequeue(context.Background(), "kind", time.Second)
	if err != nil || ready == nil {
		t.Fatal(ready, err)
	}
	if _, err := q.Ack(context.Background(), ready.RunID, ready.Generation); err != nil {
		t.Fatal(err)
	}
	if n, _, err := q.ReclaimStale(context.Background(), time.Minute, 0); n != 1 || !errors.Is(err, errReadyQueueFull) {
		t.Fatalf("partial reclaim must report only committed writes: n=%d err=%v", n, err)
	}
	retained := 0
	for _, assignment := range assignments {
		current, err := q.LookupInflight(context.Background(), assignment.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if current != nil {
			retained++
			if current.Generation != 1 {
				t.Fatalf("uncommitted reclaim advanced its generation: %+v", current)
			}
		}
		if assignment.Generation != 1 {
			t.Fatalf("reclaim changed a previously delivered assignment: %+v", assignment)
		}
	}
	if retained != 1 {
		t.Fatalf("partial reclaim lost an uncommitted lease: retained=%d", retained)
	}
}
