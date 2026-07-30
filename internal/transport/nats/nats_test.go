package natstransport_test

import (
	"context"
	"os"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sharanharsoor/runkite/internal/transport"
	"github.com/sharanharsoor/runkite/internal/transport/conformance"
	natstransport "github.com/sharanharsoor/runkite/internal/transport/nats"
)

func getNatsConn(t *testing.T) *nats.Conn {
	t.Helper()
	url := os.Getenv("NATS_URL")
	if url == "" {
		t.Skip("NATS_URL not set — skipping NATS conformance tests")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	return nc
}

func TestNATSJobQueue(t *testing.T) {
	nc := getNatsConn(t)
	defer nc.Close()

	conformance.RunJobQueueSuite(t, func() transport.JobQueue {
		if err := natstransport.FlushAll(context.Background(), nc); err != nil {
			t.Fatalf("FlushAll: %v", err)
		}
		q, err := natstransport.NewQueue(context.Background(), nc)
		if err != nil {
			t.Fatalf("NewQueue: %v", err)
		}
		return q
	})
}

func TestNATSEventBroker(t *testing.T) {
	nc := getNatsConn(t)
	defer nc.Close()

	conformance.RunEventBrokerSuite(t, func() transport.EventBroker {
		if err := natstransport.FlushAll(context.Background(), nc); err != nil {
			t.Fatalf("FlushAll: %v", err)
		}
		b, err := natstransport.NewBroker(context.Background(), nc)
		if err != nil {
			t.Fatalf("NewBroker: %v", err)
		}
		return b
	})
}

func TestNATSCancelBroker(t *testing.T) {
	nc := getNatsConn(t)
	defer nc.Close()

	conformance.RunCancelBrokerSuite(t, func() transport.CancelBroker {
		return natstransport.NewCancelBus(nc)
	})
}
