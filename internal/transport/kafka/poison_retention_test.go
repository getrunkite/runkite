package kafkatransport

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/getrunkite/runkite/internal/transport"
)

func TestExhaustedReclaimRetainsStateUntilDurableAcknowledgement(t *testing.T) {
	raw, err := json.Marshal(&transport.RunAssignment{RunID: "exhausted", RunnerKind: "kind", GraphID: "graph", TenantID: "acme", Generation: 3})
	if err != nil {
		t.Fatal(err)
	}
	// No broker writer is installed. Poison detection must be read-only with
	// respect to Kafka state and may not send a tombstone before durable completion.
	queue := &Queue{state: map[string]stateEntry{"exhausted": {RunnerKind: "kind", Generation: 3, TouchedAtUnixMilli: 1, Job: raw}}}
	for attempt := 0; attempt < 2; attempt++ {
		reclaimed, dead, err := queue.ReclaimStale(context.Background(), 0, 3)
		if err != nil || reclaimed != 0 || len(dead) != 1 {
			t.Fatalf("reaper tick=%d reclaimed=%d dead=%+v err=%v", attempt, reclaimed, dead, err)
		}
		pill := dead[0]
		if pill.RunID != "exhausted" || pill.Generation != 4 || pill.AgentID != "graph" || pill.TenantID != "acme" {
			t.Fatalf("bad poison pill: %+v", pill)
		}
		actual, err := queue.LookupInflight(context.Background(), pill.RunID)
		if err != nil || actual == nil || actual.Generation != pill.Generation-1 {
			t.Fatalf("actual attempt lost before Ack: %+v %v", actual, err)
		}
		saved, ok := queue.getState(pill.RunID)
		if !ok || saved.Generation != 3 || saved.TouchedAtUnixMilli != 1 {
			t.Fatalf("poison detection changed state: %+v %v", saved, ok)
		}
	}
}
