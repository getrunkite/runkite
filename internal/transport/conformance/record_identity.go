package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
)

func runRecordQueueTests(t *testing.T, factory JobQueueFactory) {
	t.Run("record_identity_fences_same_generation_recreation", func(t *testing.T) {
		q, ctx := factory(), context.Background()
		old := makeAssignment("record-reuse", "identity-kind", "agent")
		old.Generation, old.RunIncarnationID = 1, "original-record"
		if err := q.Enqueue(ctx, old); err != nil {
			t.Fatal(err)
		}
		first, err := q.Dequeue(ctx, old.RunnerKind, 5*time.Second)
		if err != nil || first == nil {
			t.Fatal(first, err)
		}
		if accepted, err := q.Ack(ctx, old.RunID, old.RunIncarnationID, first.Generation); err != nil || !accepted {
			t.Fatal(accepted, err)
		}
		current := *old
		current.RunIncarnationID = "replacement-record"
		if err := q.Enqueue(ctx, &current); err != nil {
			t.Fatal(err)
		}
		job, err := q.Dequeue(ctx, current.RunnerKind, 5*time.Second)
		if err != nil || job == nil || job.RunIncarnationID != current.RunIncarnationID {
			t.Fatal(job, err)
		}
		for _, wrong := range []string{"", old.RunIncarnationID} {
			if accepted, err := q.Ack(ctx, current.RunID, wrong, job.Generation); err != nil || accepted {
				t.Fatal("foreign record acknowledged", accepted, err)
			}
			if accepted, err := q.Renew(ctx, current.RunID, wrong, job.Generation); err != nil || accepted {
				t.Fatal("foreign record renewed", accepted, err)
			}
			if err := q.Nack(ctx, current.RunID, wrong, job.Generation); err != nil {
				t.Fatal(err)
			}
		}
		if err := q.Cancel(ctx, current.RunID, old.RunIncarnationID); err != nil {
			t.Fatal(err)
		}
		live, err := q.LookupInflight(ctx, current.RunID)
		if err != nil || live == nil || live.RunIncarnationID != current.RunIncarnationID || live.Generation != job.Generation {
			t.Fatal("old authority changed current lease", live, err)
		}
		if accepted, err := q.Renew(ctx, current.RunID, current.RunIncarnationID, job.Generation); err != nil || !accepted {
			t.Fatal(accepted, err)
		}
		if accepted, err := q.Ack(ctx, current.RunID, current.RunIncarnationID, job.Generation); err != nil || !accepted {
			t.Fatal(accepted, err)
		}
	})
	t.Run("dequeue_retains_other_record_without_replacing_live_slot", func(t *testing.T) {
		q, ctx := factory(), context.Background()
		current := makeAssignment("slot-reuse", "identity-kind", "agent")
		current.Generation, current.RunIncarnationID = 1, "current-record"
		if err := q.Enqueue(ctx, current); err != nil {
			t.Fatal(err)
		}
		live, err := q.Dequeue(ctx, current.RunnerKind, 5*time.Second)
		if err != nil || live == nil {
			t.Fatal(live, err)
		}
		other := *current
		other.RunIncarnationID = "other-record"
		if err := q.Enqueue(ctx, &other); err != nil {
			t.Fatal(err)
		}
		candidate, err := q.Dequeue(ctx, current.RunnerKind, 2*time.Second)
		if err != nil || candidate != nil {
			t.Fatal("conflicting record was delivered", candidate, err)
		}
		still, err := q.LookupInflight(ctx, current.RunID)
		if err != nil || still == nil || still.RunIncarnationID != current.RunIncarnationID {
			t.Fatal("live slot replaced", still, err)
		}
		// A retained collision must not monopolize later polls for this kind.
		// Repeat while the original slot stays occupied, so an implementation
		// cannot pass by checking only the ready channel on the first poll.
		for _, id := range []string{"unrelated-first", "unrelated-second"} {
			unrelated := makeAssignment(id, current.RunnerKind, "agent")
			unrelated.RunIncarnationID = "unrelated-record"
			if err := q.Enqueue(ctx, unrelated); err != nil {
				t.Fatal(err)
			}
			var delivered *transport.RunAssignment
			for poll := 0; poll < 4 && delivered == nil; poll++ {
				delivered, err = q.Dequeue(ctx, current.RunnerKind, time.Second)
				if err != nil {
					t.Fatal(err)
				}
			}
			if delivered == nil || delivered.RunID != id || delivered.RunIncarnationID != unrelated.RunIncarnationID {
				t.Fatal("record collision starved unrelated ready work", delivered)
			}
			if accepted, err := q.Ack(ctx, delivered.RunID, delivered.RunIncarnationID, delivered.Generation); err != nil || !accepted {
				t.Fatal(accepted, err)
			}
		}
		if accepted, err := q.Ack(ctx, current.RunID, current.RunIncarnationID, live.Generation); err != nil || !accepted {
			t.Fatal(accepted, err)
		}
		// Redis retains the conflicting candidate on its existing pending list.
		if reclaimer, ok := q.(interface {
			ReclaimStale(context.Context, time.Duration, int) (int, []transport.PoisonPill, error)
		}); ok {
			if _, _, err := reclaimer.ReclaimStale(ctx, time.Hour, 0); err != nil {
				t.Fatal(err)
			}
		}
		candidate, err = q.Dequeue(ctx, current.RunnerKind, 5*time.Second)
		if candidate == nil && err == nil {
			// A recovered Redis pending item is already retained as inflight;
			// it must remain discoverable until reclaim hands it out again.
			candidate, err = q.LookupInflight(ctx, current.RunID)
		}
		if err != nil || candidate == nil || candidate.RunIncarnationID != other.RunIncarnationID {
			t.Fatal("conflicting candidate lost", candidate, err)
		}
	})
	t.Run("cancel_marker_does_not_poison_reused_public_id", func(t *testing.T) {
		q, ctx := factory(), context.Background()
		if err := q.Cancel(ctx, "cancel-record", "old-record"); err != nil {
			t.Fatal(err)
		}
		job := makeAssignment("cancel-record", "identity-kind", "agent")
		job.Generation, job.RunIncarnationID = 1, "new-record"
		if err := q.Enqueue(ctx, job); err != nil {
			t.Fatal(err)
		}
		got, err := q.Dequeue(ctx, job.RunnerKind, 5*time.Second)
		if err != nil || got == nil || got.RunIncarnationID != job.RunIncarnationID {
			t.Fatal("old cancel poisoned replacement", got, err)
		}
	})
	t.Run("poison_receipt_keeps_original_record_identity", func(t *testing.T) {
		q, ctx := factory(), context.Background()
		r, ok := q.(interface {
			ReclaimStale(context.Context, time.Duration, int) (int, []transport.PoisonPill, error)
		})
		if !ok {
			t.Skip("queue has no stale reclaimer")
		}
		job := makeAssignment("poison-record", "identity-kind", "agent")
		job.Generation, job.RunIncarnationID = 1, "poison-original"
		if err := q.Enqueue(ctx, job); err != nil {
			t.Fatal(err)
		}
		got, err := q.Dequeue(ctx, job.RunnerKind, 5*time.Second)
		if err != nil || got == nil {
			t.Fatal(got, err)
		}
		_, dead, err := r.ReclaimStale(ctx, -time.Second, int(got.Generation))
		if err != nil || len(dead) != 1 || dead[0].RunIncarnationID != job.RunIncarnationID {
			t.Fatal("poison lost record identity", dead, err)
		}
	})
}

func runRecordBrokerTests(t *testing.T, factory EventBrokerFactory) {
	t.Run("replacement_stream_isolated_from_old_publish_close_and_replay", func(t *testing.T) {
		b, ctx := factory(), context.Background()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ch, err := b.Subscribe(ctx, "broker-record", "new-record")
		if err != nil {
			t.Fatal(err)
		}
		old := makeEvent("broker-record", 1, "end", map[string]string{"status": "success"})
		old.RunIncarnationID = "old-record"
		if err := b.Publish(ctx, "broker-record", "old-record", old); err != nil {
			t.Fatal(err)
		}
		if err := b.Close("broker-record", "old-record"); err != nil {
			t.Fatal(err)
		}
		if err := b.Publish(ctx, "broker-record", "new-record", old); err == nil {
			t.Fatal("foreign stamped event accepted")
		}
		current := makeEvent("broker-record", 1, "values", map[string]string{"answer": "current"})
		current.RunIncarnationID = "new-record"
		if err := b.Publish(ctx, "broker-record", "new-record", current); err != nil {
			t.Fatal(err)
		}
		select {
		case event, ok := <-ch:
			if !ok || event == nil || event.RunIncarnationID != "new-record" || event.Method != "values" {
				t.Fatal("old terminal contaminated replacement", event, ok)
			}
		case <-ctx.Done():
			t.Fatal("replacement stream was closed or stranded")
		}
		for _, identity := range []string{"old-record", "new-record"} {
			events, err := b.Replay(ctx, "broker-record", identity, 0)
			if err != nil || len(events) != 1 || events[0].RunIncarnationID != identity {
				t.Fatal("replay mixed records", identity, events, err)
			}
		}
	})
}

func runRecordCancelTests(t *testing.T, factory CancelBrokerFactory) {
	t.Run("old_record_cancel_cannot_cancel_replacement", func(t *testing.T) {
		b, ctx := factory(), context.Background()
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		old, err := b.SubscribeCancel(ctx, "cancel-signal", "old-record")
		if err != nil {
			t.Fatal(err)
		}
		current, err := b.SubscribeCancel(ctx, "cancel-signal", "new-record")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.PublishCancel(ctx, "cancel-signal", "old-record"); err != nil {
			t.Fatal(err)
		}
		select {
		case <-old:
		case <-ctx.Done():
			t.Fatal("old cancellation was not delivered")
		}
		select {
		case <-current:
			t.Fatal("old cancellation reached replacement")
		case <-time.After(100 * time.Millisecond):
		}
		if err := b.PublishCancel(ctx, "cancel-signal", "new-record"); err != nil {
			t.Fatal(err)
		}
		select {
		case <-current:
		case <-ctx.Done():
			t.Fatal("replacement cancellation was not delivered")
		}
	})
}
