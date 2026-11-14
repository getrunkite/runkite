package inprocess_test

import (
	"testing"

	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/conformance"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestWorkloadIdentityQueue(t *testing.T) {
	conformance.RunWorkloadIdentityQueueSuite(t, func() (transport.JobQueue, transport.JobQueue) { q := inprocess.NewQueue(); return q, q })
}
