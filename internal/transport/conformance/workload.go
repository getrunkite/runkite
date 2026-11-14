package conformance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
)

// RunWorkloadIdentityQueueSuite checks the same persisted owner through two
// queue clients. The factory returns clients sharing one isolated queue.
func RunWorkloadIdentityQueueSuite(t *testing.T, factory func() (transport.JobQueue, transport.JobQueue)) {
	t.Helper()
	a, b := factory()
	ownerA := &transport.WorkloadIdentity{URI: "spiffe://test.runkite/worker", KeySHA256: strings.Repeat("a", 64), NotAfter: time.Now().Add(time.Hour).UTC()}
	ownerB := *ownerA
	ownerB.KeySHA256 = strings.Repeat("b", 64)
	ctxA, ctxB := transport.WithWorkloadIdentity(context.Background(), ownerA), transport.WithWorkloadIdentity(context.Background(), &ownerB)
	input := json.RawMessage(`{"generation":500,"empty":[],"nested":{"workload_owner":"untrusted"}}`)
	if err := a.Enqueue(context.Background(), &transport.RunAssignment{RunID: "workload-conformance", RunnerKind: "kind", Generation: 1, Input: input, WorkloadOwner: &ownerB}); err != nil {
		t.Fatal(err)
	}
	job, err := a.Dequeue(ctxA, "kind", time.Second)
	if err != nil || job == nil {
		t.Fatalf("dequeue: %v %v", job, err)
	}
	if !transport.MatchesWorkload(job.WorkloadOwner, ownerA, time.Now()) {
		t.Fatalf("owner not captured: %#v", job.WorkloadOwner)
	}
	job.WorkloadOwner.KeySHA256 = "mutated returned pointer"
	live, err := b.LookupInflight(context.Background(), job.RunID)
	if err != nil || live == nil || !transport.MatchesWorkload(live.WorkloadOwner, ownerA, time.Now()) {
		t.Fatalf("owner not persisted independently: %#v %v", live, err)
	}
	if string(live.Input) != string(input) {
		t.Fatalf("input bytes changed: %s", live.Input)
	}
	if accepted, err := b.Renew(context.Background(), job.RunID, 1); err != nil || accepted {
		t.Fatalf("missing identity renewed owned work: %t %v", accepted, err)
	}
	if accepted, err := b.Ack(context.Background(), job.RunID, 1); err != nil || accepted {
		t.Fatalf("missing identity acknowledged owned work: %t %v", accepted, err)
	}
	if accepted, err := b.Renew(ctxB, job.RunID, 1); err != nil || accepted {
		t.Fatalf("peer renewed: %t %v", accepted, err)
	}
	if accepted, err := b.Ack(ctxB, job.RunID, 1); err != nil || accepted {
		t.Fatalf("peer acknowledged: %t %v", accepted, err)
	}
	if err := b.Nack(ctxB, job.RunID, 1); err != nil {
		t.Fatal(err)
	}
	if live, err := a.LookupInflight(context.Background(), job.RunID); err != nil || live == nil {
		t.Fatalf("peer removed assignment: %v %v", live, err)
	}
	if err := a.Nack(ctxA, job.RunID, 1); err != nil {
		t.Fatal(err)
	}
	replacement, err := b.Dequeue(ctxB, "kind", time.Second)
	if err != nil || replacement == nil || replacement.Generation != 2 || !transport.MatchesWorkload(replacement.WorkloadOwner, &ownerB, time.Now()) {
		t.Fatalf("Nack replacement not rebound at a fresh generation: %#v %v", replacement, err)
	}
	if accepted, err := a.Renew(ctxA, job.RunID, 2); err != nil || accepted {
		t.Fatalf("old key borrowed new generation: %t %v", accepted, err)
	}
	if accepted, err := b.Renew(ctxB, job.RunID, 2); err != nil || !accepted {
		t.Fatalf("owner renewal: %t %v", accepted, err)
	}
	if accepted, err := b.Ack(ctxB, job.RunID, 2); err != nil || !accepted {
		t.Fatalf("owner acknowledgement: %t %v", accepted, err)
	}
	if live, err := a.LookupInflight(context.Background(), job.RunID); err != nil || live != nil {
		t.Fatalf("ack retained assignment: %v %v", live, err)
	}
	if err := a.Enqueue(context.Background(), &transport.RunAssignment{RunID: "workload-recovery", RunnerKind: "kind", Generation: 3}); err != nil {
		t.Fatal(err)
	}
	recoverable, err := a.Dequeue(ctxA, "kind", time.Second)
	if err != nil || recoverable == nil {
		t.Fatal(recoverable, err)
	}
	trusted := transport.TrustedQueueContext(context.Background())
	if accepted, err := b.Ack(trusted, recoverable.RunID, 2); err != nil || accepted {
		t.Fatalf("trusted recovery bypassed generation: %t %v", accepted, err)
	}
	if accepted, err := b.Ack(trusted, recoverable.RunID, 3); err != nil || !accepted {
		t.Fatalf("trusted retained-attempt recovery failed: %t %v", accepted, err)
	}
}
