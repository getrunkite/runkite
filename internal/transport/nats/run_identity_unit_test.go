package natstransport

import (
	"context"
	"testing"

	"github.com/getrunkite/runkite/internal/transport"
)

func TestLiteralRunIdentityValidationPrecedesBackendAccess(t *testing.T) {
	ctx := context.Background()
	queue := &Queue{}
	broker := &Broker{}
	cancels := &CancelBus{}
	for _, id := range []string{"", "*", ">", "tenant.*", "tenant.>", "a*b", ".a", "a.", "a..b", "a b", "a\nb", "a\x00b", "日本語", "id:part"} {
		t.Run(id, func(t *testing.T) {
			checks := []struct {
				name string
				run  func() error
			}{
				{"enqueue", func() error { return queue.Enqueue(ctx, &transport.RunAssignment{RunID: id}) }},
				{"ack", func() error { _, err := queue.Ack(ctx, id, 1); return err }},
				{"renew", func() error { _, err := queue.Renew(ctx, id, 1); return err }},
				{"lookup", func() error { _, err := queue.LookupInflight(ctx, id); return err }},
				{"nack", func() error { return queue.Nack(ctx, id, 1) }},
				{"cancel", func() error { return queue.Cancel(ctx, id) }},
				{"publish", func() error { return broker.Publish(ctx, id, nil) }},
				{"subscribe", func() error { _, err := broker.Subscribe(ctx, id); return err }},
				{"replay", func() error { _, err := broker.Replay(ctx, id, 0); return err }},
				{"close", func() error { return broker.Close(id) }},
				{"publish_cancel", func() error { return cancels.PublishCancel(ctx, id) }},
				{"subscribe_cancel", func() error { _, err := cancels.SubscribeCancel(ctx, id); return err }},
			}
			for _, check := range checks {
				if err := check.run(); err != errInvalidRunID {
					t.Errorf("%s did not reject an invalid literal ID before backend access: %v", check.name, err)
				}
			}
		})
	}
	for _, id := range []string{"550e8400-e29b-41d4-a716-446655440000", "Upper_case-ID", "tenant/run=1", "tenant.child"} {
		if err := validateRunID(id); err != nil {
			t.Errorf("literal existing key %q was changed: %v", id, err)
		}
	}
}
