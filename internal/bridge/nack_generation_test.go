package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDelayedFailedDispatchCannotNackReplacement(t *testing.T) {
	ctx := context.Background()
	queue := inprocess.NewQueue()
	srv := NewServer(queue, inprocess.NewBroker(), inprocess.NewCancelBus(), nil)
	const runID = "delayed-dispatch"
	defer srv.cleanupRun(runID)
	if err := queue.Enqueue(ctx, &transport.RunAssignment{RunID: runID, RunnerKind: "kind", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var attempts atomic.Int32
	srv.SetRunLifecycle(func(context.Context, string, int64) (bool, error) {
		if attempts.Add(1) == 1 {
			close(entered)
			<-release
			return false, errors.New("delayed attempt write failed")
		}
		return true, nil
	}, nil)
	firstResult := make(chan error, 1)
	go func() {
		_, err := srv.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1})
		firstResult <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first attempt did not reach persistence")
	}
	if n, _, err := queue.ReclaimStale(ctx, 0, 0); err != nil || n != 1 {
		close(release)
		t.Fatalf("reclaim: n=%d err=%v", n, err)
	}
	response, err := srv.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1})
	close(release)
	if err != nil || response == nil || !response.HasJob {
		t.Fatalf("replacement dispatch: response=%v err=%v", response, err)
	}
	var replacement transport.RunAssignment
	if err := json.Unmarshal([]byte(response.AssignmentJson), &replacement); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-firstResult:
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("failed dispatch: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("failed dispatch did not return")
	}
	if replacement.Generation != 2 {
		t.Fatalf("replacement generation = %d", replacement.Generation)
	}
	if current, err := queue.Renew(ctx, runID, replacement.Generation); err != nil || !current {
		t.Fatalf("old dispatch removed replacement lease: current=%t err=%v", current, err)
	}
	if extra, err := queue.Dequeue(ctx, "kind", 10*time.Millisecond); err != nil || extra != nil {
		t.Fatalf("old dispatch duplicated replacement: job=%v err=%v", extra, err)
	}
}
