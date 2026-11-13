package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/execution"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestPoolGetJobRejectsForeignScopeBeforeInputAndAttempt(t *testing.T) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(raw[:])
	t.Setenv("RUNNER_TOKEN_POOL_FINANCE", token)
	tokens := auth.LoadRunnerTokensFromEnv()
	catalog, err := execution.NewCatalog(map[string]execution.Pool{"finance": {RunnerKind: "python-langgraph", Tenants: []string{"acme"}, Agents: []string{"finance"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = tokens.ConfigureExecutionPools(catalog); err != nil {
		t.Fatal(err)
	}
	ctx, err := authenticateRunner(metadata.NewIncomingContext(context.Background(), metadata.Pairs("runner-kind", "pool-finance", "runner-token", token)), tokens)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []struct{ tenant, agent string }{{"other", "finance"}, {"acme", "other"}} {
		q := inprocess.NewQueue()
		job := &transport.RunAssignment{RunID: "synthetic-private", RunnerKind: "pool-finance", TenantID: scope.tenant, GraphID: scope.agent, Generation: 1, Input: json.RawMessage(`{"private":"synthetic"}`)}
		if err := q.Enqueue(ctx, job); err != nil {
			t.Fatal(err)
		}
		s := NewServer(q, inprocess.NewBroker(), inprocess.NewCancelBus(), nil)
		attempts := 0
		s.SetRunLifecycle(func(context.Context, string, int64) (bool, error) { attempts++; return true, nil }, nil)
		response, err := s.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "pool-finance", TimeoutSeconds: 1})
		if status.Code(err) != codes.PermissionDenied || response != nil || attempts != 0 {
			t.Fatalf("foreign data exposed: response=%v err=%v attempts=%d", response, err, attempts)
		}
	}
	if _, err := NewServer(inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus(), nil).GetJob(context.Background(), &pb.GetJobRequest{RunnerKind: "pool-unknown"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unknown pool accepted in local mode: %v", err)
	}
	queue := inprocess.NewQueue()
	private := &transport.RunAssignment{RunID: "other-pool-run", RunnerKind: "pool-other", GraphID: "finance", TenantID: "acme", Generation: 1, Input: json.RawMessage(`{"private":"other pool input"}`)}
	if err := queue.Enqueue(ctx, private); err != nil {
		t.Fatal(err)
	}
	server := NewServer(queue, inprocess.NewBroker(), inprocess.NewCancelBus(), nil)
	if result, err := server.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "pool-other", TimeoutSeconds: 1}); status.Code(err) != codes.PermissionDenied || result != nil {
		t.Fatalf("another pool received input: %v %v", result, err)
	}
	retained, err := queue.Dequeue(context.Background(), "pool-other", time.Millisecond)
	if err != nil || retained == nil || retained.RunID != private.RunID {
		t.Fatalf("wrong pool changed the other queue: %v %v", retained, err)
	}
}
