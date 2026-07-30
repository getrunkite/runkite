package kafkatransport_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/sharanharsoor/runkite/internal/transport"
	"github.com/sharanharsoor/runkite/internal/transport/conformance"
	kafkatransport "github.com/sharanharsoor/runkite/internal/transport/kafka"
)

// TestMain pays this package's own documented "very first consumer
// group on a virgin cluster" cold-start cost exactly once, before any
// sub-test runs, instead of leaving one unlucky sub-test's own Dequeue
// timeout to absorb it by surprise -- see kafka.go's own package doc
// comment for why this isn't done inside NewQueueWithNamespace itself.
// A no-op (fast return) against any Kafka cluster that's already
// processed a consumer group before, which in practice is every
// cluster except a container started seconds ago for this test run.
func TestMain(m *testing.M) {
	if url := os.Getenv("KAFKA_URL"); url != "" {
		warmUpConsumerGroups(strings.Split(url, ","))
	}
	os.Exit(m.Run())
}

func warmUpConsumerGroups(brokers []string) {
	topic := fmt.Sprintf("warmup-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn, err := kafka.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return
	}
	_ = conn.CreateTopics(kafka.TopicConfig{Topic: topic, NumPartitions: 1, ReplicationFactor: 1})
	conn.Close()

	reader := kafka.NewReader(kafka.ReaderConfig{Brokers: brokers, GroupID: "runkite-test-warmup", Topic: topic})
	defer reader.Close()
	_, _ = reader.FetchMessage(ctx) // expected to time out on an empty topic; the group join itself is the point
}

func getKafkaBrokers(t *testing.T) []string {
	t.Helper()
	url := os.Getenv("KAFKA_URL")
	if url == "" {
		t.Skip("KAFKA_URL not set — skipping Kafka conformance tests")
	}
	return strings.Split(url, ",")
}

func TestKafkaJobQueue(t *testing.T) {
	brokers := getKafkaBrokers(t)

	conformance.RunJobQueueSuite(t, func() transport.JobQueue {
		// Fresh, uniquely-named topics per test -- see
		// NewQueueWithNamespace's own doc comment for why (no cheap
		// "wipe everything" primitive in Kafka the way Redis/NATS have).
		namespace := fmt.Sprintf("test-%d", time.Now().UnixNano())
		q, err := kafkatransport.NewQueueWithNamespace(context.Background(), brokers, namespace)
		if err != nil {
			t.Fatalf("NewQueueWithNamespace: %v", err)
		}
		t.Cleanup(func() { q.Close() })
		return q
	})
}
