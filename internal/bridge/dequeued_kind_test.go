package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A backend filter or consumer-name collision can return a different kind's
// assignment even when the authenticated request selects its own queue.
type misroutedKindQueue struct {
	transport.JobQueue
	returnedKind string
	nackError    error
	nackRunID    string
	nackGen      int64
	nackDeadline bool
}

func (q *misroutedKindQueue) Nack(ctx context.Context, runID string, generation int64) error {
	q.nackRunID, q.nackGen = runID, generation
	_, q.nackDeadline = ctx.Deadline()
	if q.nackError != nil {
		return q.nackError
	}
	return q.JobQueue.Nack(ctx, runID, generation)
}

func (q *misroutedKindQueue) Dequeue(ctx context.Context, _ string, timeout time.Duration) (*transport.RunAssignment, error) {
	return q.JobQueue.Dequeue(ctx, q.returnedKind, timeout)
}

func TestGetJobRejectsDequeuedAssignmentFromAnotherAuthenticatedKind(t *testing.T) {
	ctx := context.WithValue(context.Background(), runnerIdentityKey{}, "allowed-kind")
	queue := inprocess.NewQueue()
	job := &transport.RunAssignment{
		RunID: "foreign-assignment", RunnerKind: "private-kind", GraphID: "private-agent", Generation: 1,
		Input: json.RawMessage(`{"private":"synthetic tenant input"}`),
	}
	if err := queue.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	routed := &misroutedKindQueue{JobQueue: queue, returnedKind: "private-kind"}
	srv := NewServer(routed, inprocess.NewBroker(), inprocess.NewCancelBus(), nil)
	defer srv.cleanupRun(job.RunID)
	attempts := 0
	srv.SetRunLifecycle(func(context.Context, string, int64) (bool, error) {
		attempts++
		return true, nil
	}, nil)
	response, err := srv.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "allowed-kind", TimeoutSeconds: 1})
	if status.Code(err) != codes.PermissionDenied || response != nil {
		t.Errorf("foreign assignment reached an authenticated runner: hasResponse=%t err=%v", response != nil, err)
	}
	if attempts != 0 {
		t.Errorf("foreign assignment advanced durable attempt before authorization: attempts=%d", attempts)
	}
	if routed.nackRunID != job.RunID || routed.nackGen != job.Generation || !routed.nackDeadline {
		t.Errorf("foreign assignment cleanup was not bounded and generation-fenced: %+v", routed)
	}
	returned, err := queue.Dequeue(context.Background(), "private-kind", 10*time.Millisecond)
	if err != nil || returned == nil || returned.RunID != job.RunID || returned.Generation != job.Generation {
		t.Fatalf("foreign assignment was not retained for its own runner: job=%v err=%v", returned, err)
	}
}

func TestGetJobRetainsForeignAssignmentWhenReturnFails(t *testing.T) {
	ctx := context.WithValue(context.Background(), runnerIdentityKey{}, "allowed-kind")
	queue := inprocess.NewQueue()
	job := &transport.RunAssignment{RunID: "foreign-nack-failure", RunnerKind: "private-kind", Generation: 3}
	if err := queue.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	routed := &misroutedKindQueue{JobQueue: queue, returnedKind: "private-kind", nackError: errors.New("backend unavailable")}
	srv := NewServer(routed, inprocess.NewBroker(), inprocess.NewCancelBus(), nil)
	attempts := 0
	srv.SetRunLifecycle(func(context.Context, string, int64) (bool, error) {
		attempts++
		return true, nil
	}, nil)
	response, err := srv.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "allowed-kind", TimeoutSeconds: 1})
	if status.Code(err) != codes.Unavailable || response != nil || attempts != 0 {
		t.Fatalf("failed return leaked/advanced assignment: response=%v err=%v attempts=%d", response, err, attempts)
	}
	if routed.nackRunID != job.RunID || routed.nackGen != job.Generation || !routed.nackDeadline {
		t.Fatalf("return failure lost the exact bounded cleanup: %+v", routed)
	}
	retained, lookupErr := queue.LookupInflight(context.Background(), job.RunID)
	if lookupErr != nil || retained == nil || retained.Generation != job.Generation {
		t.Fatalf("failed return removed recoverable assignment: assignment=%v err=%v", retained, lookupErr)
	}
}

func TestGetJobPreservesTrustedLocalModeForDequeuedKind(t *testing.T) {
	ctx := context.Background()
	queue := inprocess.NewQueue()
	job := &transport.RunAssignment{RunID: "local-assignment", RunnerKind: "private-kind", Generation: 1}
	if err := queue.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(&misroutedKindQueue{JobQueue: queue, returnedKind: "private-kind"}, inprocess.NewBroker(), inprocess.NewCancelBus(), nil)
	defer srv.cleanupRun(job.RunID)
	response, err := srv.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "allowed-kind", TimeoutSeconds: 1})
	if err != nil || response == nil || !response.HasJob {
		t.Fatalf("local no-token compatibility changed: response=%v err=%v", response, err)
	}
}
