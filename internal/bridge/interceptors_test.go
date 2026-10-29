package bridge

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/getrunkite/runkite/internal/auth"
	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		old, existed := os.LookupEnv(k)
		os.Setenv(k, v)
		t.Cleanup(func() {
			if existed {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		})
	}
}

// startTestBridge boots a real gRPC server (over an in-memory bufconn, not a
// TCP port) with the given RunnerTokens wired through both interceptors,
// exactly as cmd/serve.go does. Returns a connected client and a cleanup func.
func startTestBridge(t *testing.T, rt *auth.RunnerTokens) pb.RunnerServiceClient {
	t.Helper()
	return startTestBridgeWithQueue(t, rt, inprocess.NewQueue())
}

func startTestBridgeWithQueue(t *testing.T, rt *auth.RunnerTokens, queue *inprocess.Queue) pb.RunnerServiceClient {
	t.Helper()
	broker := inprocess.NewBroker()
	cancelBus := inprocess.NewCancelBus()
	srv := NewServer(queue, broker, cancelBus, nil)

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(UnaryAuthInterceptor(rt)),
		grpc.ChainStreamInterceptor(StreamAuthInterceptor(rt)),
	)
	pb.RegisterRunnerServiceServer(grpcServer, srv)
	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewRunnerServiceClient(conn)
}

func ctxWithRunnerAuth(kind, token string) context.Context {
	if kind == "" && token == "" {
		return context.Background()
	}
	return metadata.NewOutgoingContext(context.Background(), metadata.Pairs(
		metaRunnerKind, kind, metaRunnerToken, token,
	))
}

func TestGRPCAuth_LocalModeAllowsAnyRunner(t *testing.T) {
	client := startTestBridge(t, &auth.RunnerTokens{}) // disabled -- local mode

	_, err := client.GetJob(ctxWithRunnerAuth("", ""), &pb.GetJobRequest{RunnerKind: "python-langgraph", TimeoutSeconds: 1})
	if err != nil {
		t.Fatalf("local mode should allow GetJob without credentials: %v", err)
	}
}

func TestGRPCAuth_ProductionModeRejectsMissingCredentials(t *testing.T) {
	withEnv(t, map[string]string{"RUNNER_TOKEN_PYTHON_LANGGRAPH": "secret-tok"})
	rt := auth.LoadRunnerTokensFromEnv()
	client := startTestBridge(t, rt)

	_, err := client.GetJob(ctxWithRunnerAuth("", ""), &pb.GetJobRequest{RunnerKind: "python-langgraph", TimeoutSeconds: 1})
	if err == nil {
		t.Fatal("expected error without runner credentials in production mode")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
	}
}

func TestGRPCAuth_ProductionModeRejectsWrongToken(t *testing.T) {
	withEnv(t, map[string]string{"RUNNER_TOKEN_PYTHON_LANGGRAPH": "secret-tok"})
	rt := auth.LoadRunnerTokensFromEnv()
	client := startTestBridge(t, rt)

	_, err := client.GetJob(ctxWithRunnerAuth("python-langgraph", "wrong"), &pb.GetJobRequest{RunnerKind: "python-langgraph", TimeoutSeconds: 1})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated for wrong token, got %v", status.Code(err))
	}
}

func TestGRPCAuth_ProductionModeAcceptsValidToken(t *testing.T) {
	withEnv(t, map[string]string{"RUNNER_TOKEN_PYTHON_LANGGRAPH": "secret-tok"})
	rt := auth.LoadRunnerTokensFromEnv()
	client := startTestBridge(t, rt)

	_, err := client.GetJob(ctxWithRunnerAuth("python-langgraph", "secret-tok"), &pb.GetJobRequest{RunnerKind: "python-langgraph", TimeoutSeconds: 1})
	if err != nil {
		t.Fatalf("expected valid token to succeed: %v", err)
	}
}

func TestGRPCAuth_CrossKindTokenRejected(t *testing.T) {
	withEnv(t, map[string]string{
		"RUNNER_TOKEN_PYTHON_LANGGRAPH":       "tok-py",
		"RUNNER_TOKEN_TYPESCRIPT_LANGGRAPHJS": "tok-ts",
	})
	rt := auth.LoadRunnerTokensFromEnv()
	client := startTestBridge(t, rt)

	// tok-ts is valid for typescript-langgraphjs, must not work for python-langgraph.
	_, err := client.GetJob(ctxWithRunnerAuth("python-langgraph", "tok-ts"), &pb.GetJobRequest{RunnerKind: "python-langgraph", TimeoutSeconds: 1})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated for cross-kind token reuse, got %v", status.Code(err))
	}
}

func TestGRPCAuth_StreamingRPCEnforced(t *testing.T) {
	withEnv(t, map[string]string{"RUNNER_TOKEN_PYTHON_LANGGRAPH": "secret-tok"})
	rt := auth.LoadRunnerTokensFromEnv()
	client := startTestBridge(t, rt)

	// WatchCancels is server-streaming -- auth must be enforced on stream open.
	stream, err := client.WatchCancels(ctxWithRunnerAuth("python-langgraph", "wrong"), &pb.WatchCancelsRequest{RunnerKind: "python-langgraph"})
	if err != nil {
		// Some client-side setups return the error immediately.
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
		}
		return
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated on first Recv, got %v", status.Code(err))
	}
}

func TestGRPCAuth_AuthenticatedKindCannotSelectAnotherQueue(t *testing.T) {
	withEnv(t, map[string]string{"RUNNER_TOKEN_PYTHON_LANGGRAPH": "tok-py"})
	client := startTestBridge(t, auth.LoadRunnerTokensFromEnv())
	ctx := ctxWithRunnerAuth("python-langgraph", "tok-py")
	if _, err := client.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "typescript-langgraphjs", TimeoutSeconds: 1}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("cross-kind queue selection: %v", err)
	}
	stream, err := client.WatchCancels(ctx, &pb.WatchCancelsRequest{RunnerKind: "typescript-langgraphjs"})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("cross-kind cancel watch: %v", err)
	}
}

func TestGRPCAuth_RunMutationRequiresAssignmentOwner(t *testing.T) {
	withEnv(t, map[string]string{"RUNNER_TOKEN_PYTHON_LANGGRAPH": "tok-py"})
	q := inprocess.NewQueue()
	ctx := context.Background()
	if err := q.Enqueue(ctx, &transport.RunAssignment{RunID: "foreign", RunnerKind: "typescript-langgraphjs", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Dequeue(ctx, "typescript-langgraphjs", time.Second); err != nil {
		t.Fatal(err)
	}
	client := startTestBridgeWithQueue(t, auth.LoadRunnerTokensFromEnv(), q)
	caller := ctxWithRunnerAuth("python-langgraph", "tok-py")
	if _, err := client.Heartbeat(caller, &pb.HeartbeatRequest{RunId: "foreign", Generation: 1}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign heartbeat: %v", err)
	}
	if _, err := client.ReportStatus(caller, &pb.ReportStatusRequest{RunId: "foreign", Generation: 1, Status: "success"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign completion: %v", err)
	}
	stream, err := client.StreamEvents(caller)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(&pb.RunEventProto{RunId: "foreign", Generation: 1, EventJson: `{"method":"values","data":{"stolen":true}}`})
	if _, err := stream.CloseAndRecv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign event injection: %v", err)
	}
	assignment, _ := q.LookupInflight(ctx, "foreign")
	if assignment == nil {
		t.Fatal("unauthorized completion removed foreign lease")
	}
}
