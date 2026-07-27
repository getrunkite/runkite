package inprocess_test

import (
	"testing"

	"github.com/runkite/runkite/internal/transport"
	"github.com/runkite/runkite/internal/transport/conformance"
	"github.com/runkite/runkite/internal/transport/inprocess"
)

func TestInMemoryJobQueue(t *testing.T) {
	conformance.RunJobQueueSuite(t, func() transport.JobQueue {
		return inprocess.NewQueue()
	})
}

func TestInMemoryEventBroker(t *testing.T) {
	conformance.RunEventBrokerSuite(t, func() transport.EventBroker {
		return inprocess.NewBroker()
	})
}

func TestInMemoryCancelBroker(t *testing.T) {
	conformance.RunCancelBrokerSuite(t, func() transport.CancelBroker {
		return inprocess.NewCancelBus()
	})
}
