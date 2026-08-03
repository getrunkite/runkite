package bridge

import (
	"context"
	"net"
	"strings"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/getrunkite/runkite/internal/auth"
	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestGRPCOtel_GetJobEmitsServerSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})

	queue := inprocess.NewQueue()
	broker := inprocess.NewBroker()
	cancelBus := inprocess.NewCancelBus()
	srv := NewServer(queue, broker, cancelBus, nil)

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(UnaryAuthInterceptor(&auth.RunnerTokens{})),
		grpc.ChainStreamInterceptor(StreamAuthInterceptor(&auth.RunnerTokens{})),
	)
	pb.RegisterRunnerServiceServer(grpcServer, srv)
	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := pb.NewRunnerServiceClient(conn)

	_, err = client.GetJob(context.Background(), &pb.GetJobRequest{
		RunnerKind:     "python-langgraph",
		TimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	spans := exp.GetSpans()
	var found bool
	for _, s := range spans {
		if strings.Contains(s.Name, "GetJob") || strings.Contains(s.Name, "RunnerService") {
			found = true
			break
		}
	}
	if !found {
		names := make([]string, 0, len(spans))
		for _, s := range spans {
			names = append(names, s.Name)
		}
		t.Fatalf("expected a gRPC server span for GetJob, got spans %v", names)
	}
}
