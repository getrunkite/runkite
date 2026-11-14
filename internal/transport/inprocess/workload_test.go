package inprocess_test

import (
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/conformance"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
	"testing"
)

func TestWorkloadIdentityQueue(t *testing.T) {
	conformance.RunWorkloadIdentityQueueSuite(t, func() (transport.JobQueue, transport.JobQueue) { q := inprocess.NewQueue(); return q, q })
}
