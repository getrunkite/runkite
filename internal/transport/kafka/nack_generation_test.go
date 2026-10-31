package kafkatransport

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/getrunkite/runkite/internal/transport"
)

func TestStaleNackDoesNotProduceOrRemoveCurrentAssignment(t *testing.T) {
	for _, current := range []int64{0, 2} {
		raw, err := json.Marshal(&transport.RunAssignment{RunID: "fenced-nack", RunnerKind: "kind", Generation: current})
		if err != nil {
			t.Fatal(err)
		}
		q := &Queue{state: map[string]stateEntry{"fenced-nack": {RunnerKind: "kind", Generation: current, TouchedAtUnixMilli: 10, Job: raw}}}
		// No writer is installed. A stale Nack must return before attempting
		// a produce, just as it must leave the live state entry untouched.
		wrong := int64(0)
		if current == 0 {
			wrong = 2
		}
		if err := q.Nack(context.Background(), "fenced-nack", wrong); err != nil {
			t.Fatal(err)
		}
		entry, ok := q.getState("fenced-nack")
		if !ok || entry.Generation != current || entry.TouchedAtUnixMilli != 10 || string(entry.Job) != string(raw) {
			t.Fatalf("stale generation %d changed current entry: %+v", wrong, entry)
		}
	}
}
