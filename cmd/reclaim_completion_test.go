package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

type poisonAckQueue struct {
	*inprocess.Queue
	completed bool
	failAck   bool
	acks      []int64
}

func (q *poisonAckQueue) Ack(ctx context.Context, runID string, generation int64) (bool, error) {
	if !q.completed {
		return false, errors.New("ack attempted before durable completion")
	}
	q.acks = append(q.acks, generation)
	if q.failAck {
		return false, errors.New("injected queue outage")
	}
	return q.Queue.Ack(ctx, runID, generation)
}

func poisonQueueFixture(t *testing.T) *poisonAckQueue {
	t.Helper()
	q := &poisonAckQueue{Queue: inprocess.NewQueue()}
	ctx := context.Background()
	if err := q.Enqueue(ctx, &transport.RunAssignment{RunID: "poison", RunnerKind: "kind", Generation: 2}); err != nil {
		t.Fatal(err)
	}
	if assignment, err := q.Dequeue(ctx, "kind", time.Second); err != nil || assignment == nil {
		t.Fatal(assignment, err)
	}
	return q
}

func TestFinishPoisonPillRetainsLeaseUntilCompletionAndAckSucceed(t *testing.T) {
	ctx := context.Background()
	q := poisonQueueFixture(t)
	pill := transport.PoisonPill{RunID: "poison", Generation: 3}
	storeDown := errors.New("injected store outage")
	if finished, err := finishPoisonPill(ctx, q, pill, "exhausted", func(context.Context, transport.PoisonPill, string) (bool, error) {
		return false, storeDown
	}); finished || !errors.Is(err, storeDown) || len(q.acks) != 0 {
		t.Fatalf("store failure was acknowledged: finished=%v err=%v acks=%v", finished, err, q.acks)
	}
	complete := func(context.Context, transport.PoisonPill, string) (bool, error) {
		q.completed = true
		return true, nil
	}
	q.failAck = true
	if finished, err := finishPoisonPill(ctx, q, pill, "exhausted", complete); finished || err == nil {
		t.Fatalf("queue failure confirmed: finished=%v err=%v", finished, err)
	}
	if lease, err := q.LookupInflight(ctx, pill.RunID); err != nil || lease == nil || lease.Generation != 2 {
		t.Fatalf("failed finalization discarded lease: lease=%v err=%v", lease, err)
	}
	q.failAck = false
	if finished, err := finishPoisonPill(ctx, q, pill, "exhausted", complete); err != nil || !finished {
		t.Fatal(finished, err)
	}
	if lease, err := q.LookupInflight(ctx, pill.RunID); err != nil || lease != nil {
		t.Fatalf("successful finalization retained lease: lease=%v err=%v", lease, err)
	}
	if len(q.acks) != 2 || q.acks[0] != 2 || q.acks[1] != 2 {
		t.Fatalf("ack used a different attempt: %v", q.acks)
	}
}

func TestFinishPoisonPillDoesNotAckRejectedCompletion(t *testing.T) {
	q := poisonQueueFixture(t)
	finished, err := finishPoisonPill(context.Background(), q, transport.PoisonPill{RunID: "poison", Generation: 3}, "exhausted", func(context.Context, transport.PoisonPill, string) (bool, error) {
		return false, nil
	})
	if finished || err != nil || len(q.acks) != 0 {
		t.Fatalf("rejected completion acknowledged: finished=%v err=%v acks=%v", finished, err, q.acks)
	}
}
