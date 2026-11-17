package kafkatransport

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/getrunkite/runkite/internal/transport"
)

func TestDefaultConstructorRefusesNonAtomicFencingBeforeNetwork(t *testing.T) {
	for _, options := range [][]Option{nil, {AllowNonAtomicFencing(false)}} {
		_, err := NewQueueWithNamespace(context.Background(), []string{"invalid.fixture:1"}, "strict-fixture", options...)
		if !errors.Is(err, ErrNonAtomicFencing) {
			t.Fatalf("strict constructor did not fail before broker access: %v", err)
		}
	}
	queue := &Queue{}
	if err := queue.Enqueue(context.Background(), &transport.RunAssignment{RunID: "strict-run", RunIncarnationID: "original", Generation: 1}); !errors.Is(err, ErrNonAtomicFencing) {
		t.Fatal("unconfigured queue attempted publication", err)
	}
}

func TestCompatibleModeStillRejectsOldRecordWithoutWriting(t *testing.T) {
	raw, err := json.Marshal(&transport.RunAssignment{RunID: "reused", RunIncarnationID: "current", Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	queue := &Queue{allowNonAtomicFencing: true, state: map[string]stateEntry{"reused": {Generation: 1, Job: raw}}}
	// No writer exists. A foreign record must be rejected before any produce.
	for _, identity := range []string{"", "old"} {
		if accepted, err := queue.Ack(context.Background(), "reused", identity, 1); err != nil || accepted {
			t.Fatal(accepted, err)
		}
		if accepted, err := queue.Renew(context.Background(), "reused", identity, 1); err != nil || accepted {
			t.Fatal(accepted, err)
		}
		if err := queue.Nack(context.Background(), "reused", identity, 1); err != nil {
			t.Fatal(err)
		}
		if err := queue.Cancel(context.Background(), "reused", identity); err != nil {
			t.Fatal(err)
		}
	}
	if entry, ok := queue.getState("reused"); !ok || !stateHasIdentity(entry, "current") {
		t.Fatal("foreign record changed compatible state")
	}
}
