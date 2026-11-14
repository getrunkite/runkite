package redistransport_test

import (
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/conformance"
	redistransport "github.com/getrunkite/runkite/internal/transport/redis"
	"testing"
)

func TestWorkloadIdentityQueueCrossReplica(t *testing.T) {
	rdb := getRedisClient(t)
	t.Cleanup(func() { _ = rdb.Close() })
	conformance.RunWorkloadIdentityQueueSuite(t, func() (transport.JobQueue, transport.JobQueue) {
		return redistransport.NewQueue(rdb), redistransport.NewQueue(rdb)
	})
}
