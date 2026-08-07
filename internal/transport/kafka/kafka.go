// Package kafkatransport implements transport.JobQueue using Kafka --
// JobQueue only, deliberately, unlike NATS's full JobQueue+EventBroker+
// CancelBroker triad (see internal/transport/nats's own package doc for
// why NATS earns the full triad and this doesn't): Kafka has no native
// pub/sub primitive suited to EventBroker's fan-out-plus-replay shape
// or CancelBroker's fire-and-forget signal, and bolting one on top of
// Kafka's own log model would be reinventing a second, unrelated
// technology rather than using Kafka for what it's actually good at.
// Pair a Kafka-backed JobQueue with Redis or the in-process transport
// for EventBroker/CancelBroker (see cmd/serve.go's initTransport).
//
// Kafka's own redelivery model is fundamentally different from Redis's
// (a Lua-scripted lease+reaper over a List) and NATS's (a native
// per-message AckWait timer): Kafka only tracks ONE committed offset
// per partition, and committing offset N implicitly commits everything
// below N too (segmentio/kafka-go's own documented behavior) -- there
// is no way to redeliver ONE specific message out of a partition's
// sequence via Kafka's own offset mechanism alone. Rather than force a
// per-message lease onto that model, this package treats Kafka's own
// offset commit as a cheap "how far can a fresh consumer skip on
// restart" hint, not the authoritative record of whether a job is
// actually done -- Dequeue commits a message's offset immediately, and
// a SEPARATE, single-partition, log-compacted topic ("<a namespace
// prefix>.state") is the actual source of truth for what's in-flight,
// its fencing generation, and its full payload for re-delivery.
// Practically: every control-plane replica tails that compacted topic
// from its own beginning at startup and keeps an in-memory materialized
// view (a well-established Kafka pattern -- the log is the source of
// truth, the in-memory map is a derived, disposable index any replica
// can rebuild), which is what makes Ack/Renew/Nack/ReclaimStale usable
// from ANY replica regardless of which one's Fetch call actually
// received a given message -- the same "shared, not process-local"
// property Redis's own transport needed a real fix for once a
// load-balanced, multi-replica deployment exposed a process-local map
// as a genuine bug, solved here with a Kafka-native mechanism instead
// of Redis's Lua scripts or NATS's KV bucket.
//
// Fencing generation is hand-rolled here too, same as NATS (Kafka's own
// consumer-group generation ID is a group-membership concept tied to
// rebalances, not a per-message counter, and using it directly would
// require staying on the exact connection that fetched a message,
// which doesn't hold across a load-balanced gRPC bridge with no session
// affinity). Generation lives in the state-topic entry, incremented by
// ReclaimStale/Nack, the same shape as Redis's own hand-rolled counter.
//
// Known trade-off versus Redis/NATS: Kafka alone has no compare-and-swap
// for ReclaimStale -- a state-topic produce is last-write-wins, so two
// replicas' reaper ticks racing the same stale run_id could both
// re-produce it. Closed for the supported HA posture (KAFKA_URL +
// REDIS_URL) by a Redis SET NX reclaim-leader lock in cmd/reclaim.go
// so only one replica's reaper runs per tick. Kafka without Redis
// remains single-instance / experimental for multi-replica reclaim
// (events/cancel are process-local there anyway).
//
// Known cold-start note: the very first consumer group EVER joined on
// a Kafka cluster forces the broker to lazily create its own internal
// __consumer_offsets topic (50 partitions by default), which can take
// noticeably longer than a normal group join -- confirmed live against
// a freshly started single-broker test cluster, long enough to miss a
// 5s Dequeue timeout on the very first call. This is a real, one-time,
// whole-cluster cost, not a per-runner_kind or per-replica one, and in
// practice only matters against a genuinely virgin cluster (a
// long-running production Kafka cluster has already paid it, often
// years before Runkite ever connects) -- deliberately not papered over
// here with a blocking warm-up at construction time, since the only
// way to force it early is to wait on an empty topic until a timeout
// elapses, which would tax every single construction, not just the
// first, for a cost that's already zero in the overwhelmingly common
// case. A fresh test or dev cluster's very first Dequeue call being
// slower than later ones is expected, not a bug.
package kafkatransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/getrunkite/runkite/internal/transport"
)

const defaultNamespace = "rk"

// readerMaxBytes matches the Writer's own raised BatchBytes -- kafka-go's
// Reader.MaxBytes (per-partition fetch cap) defaults to 1MB independent
// of the Writer side, so a >1MB run payload that got past the Writer's
// own raised limit would otherwise still get stuck unreadable here.
const readerMaxBytes = 8 << 20

// defaultJobPartitions is how many partitions a job topic gets unless
// overridden via WithJobPartitions. Kept at 1 by default deliberately,
// not as an oversight: with 1 partition, Kafka's own consumer-group
// protocol only ever assigns it to ONE member at a time, so only one
// control-plane replica actually dequeues jobs of a given runner_kind
// at once -- other replicas' own GetJob calls simply see an empty
// queue (not an error, and not a correctness gap: Ack/Renew/Nack still
// work from any replica via the shared state topic) until Kafka
// rebalances the partition to them, e.g. if the currently-assigned
// replica dies. This is an honest, real throughput/availability
// ceiling for Dequeue specifically at the default, raised via
// WithJobPartitions when multi-replica dequeue concurrency for one
// runner_kind actually matters -- see that function's own doc comment.
const defaultJobPartitions = 1

// consumerGroupID derives a stable, Kafka-safe consumer group id from a
// namespace and runner_kind -- every control-plane replica configured
// with the same Kafka cluster, namespace, and runner_kind joins the
// SAME group, so multiple replicas CAN dequeue the same runner_kind
// concurrently the normal Kafka way -- but only up to as many
// partitions as that job topic actually has (see
// defaultJobPartitions's own doc comment for the very real default-1
// ceiling this implies).
//
// Namespacing this (not just the topic name) matters: confirmed live,
// reusing a bare "runner_kind"-only group id across two independent
// Queues pointed at the SAME broker (e.g. two of this package's own
// conformance sub-tests, each with a fresh topic namespace but the same
// literal runnerKind string) caused visible consumer-group membership
// confusion -- a fresh Queue's reader would join a group the previous
// test's reader hadn't finished leaving yet, producing wrong/duplicate
// offset state despite the two Queues' job topics being completely
// separate. Folding the namespace into the group id, not just the
// topic, gives every independent Queue its own fully isolated group.
func consumerGroupID(namespace, runnerKind string) string {
	return "runkite-jobs-" + namespace + "-" + strings.ReplaceAll(runnerKind, ".", "_")
}

// stateEntry is the JSON value stored in the compacted stateTopic,
// keyed by run_id. A nil/empty Kafka message value is a tombstone
// (Ack'd or otherwise cleared) -- standard log-compaction convention,
// not a value this struct itself represents.
type stateEntry struct {
	// Canceled marks a run_id that must never be delivered (or
	// re-delivered) again -- checked by Enqueue and Dequeue, set by
	// Cancel. A canceled entry has no Job/Generation/TouchedAt.
	// Unlike Redis (TTL'd ZSET) / NATS (KV TTL), compacted Kafka state
	// retains these until a tombstone is written -- Compatible-tier
	// trade-off, not the old unbounded Redis SET bug class.
	Canceled bool `json:"canceled,omitempty"`

	RunnerKind         string          `json:"runner_kind,omitempty"`
	Generation         int64           `json:"generation,omitempty"`
	TouchedAtUnixMilli int64           `json:"touched_at_ms,omitempty"`
	Job                json.RawMessage `json:"job,omitempty"`
}

// nowMillis is a var (not a plain time.Now call) so tests can freeze/
// control it without sleeping in wall-clock time -- same convention as
// redistransport's/natstransport's own nowMillis.
var nowMillis = func() int64 { return time.Now().UnixMilli() }

// Queue implements transport.JobQueue using Kafka. See this package's
// own doc comment for the overall design (offset commit as a delivery
// hint, the compacted state topic as the authoritative in-flight
// record).
type Queue struct {
	brokers       []string
	namespace     string
	jobPartitions int
	writer        *kafka.Writer

	mu      sync.Mutex
	readers map[string]*kafka.Reader // runner_kind -> consumer-group reader

	stateMu sync.RWMutex
	state   map[string]stateEntry // run_id -> current state, materialized from stateTopic

	stopTailing context.CancelFunc
}

// Option configures a Queue at construction time.
type Option func(*Queue)

// WithJobPartitions overrides defaultJobPartitions (1) for every job
// topic this Queue creates. Only affects topics created for the first
// time -- an already-existing topic's own partition count is set once,
// at creation, by whichever replica happened to create it first; this
// has no effect against a topic another, earlier-started replica
// already created with a different count. Set consistently across every
// replica sharing a Kafka cluster (e.g. via one shared environment
// variable, as cmd/serve.go's own KAFKA_JOB_PARTITIONS wiring does) to
// avoid that ambiguity in practice.
func WithJobPartitions(n int) Option {
	return func(q *Queue) {
		if n > 0 {
			q.jobPartitions = n
		}
	}
}

func (q *Queue) jobTopic(runnerKind string) string { return q.namespace + ".jobs." + runnerKind }
func (q *Queue) stateTopic() string                { return q.namespace + ".state" }

// NewQueue creates a Kafka-backed job queue under the default topic
// namespace ("rk") -- see NewQueueWithNamespace for why a caller (only
// this package's own tests, in practice) would ever need a different
// one. opts is most commonly just WithJobPartitions -- see its own doc
// comment.
func NewQueue(ctx context.Context, brokers []string, opts ...Option) (*Queue, error) {
	return NewQueueWithNamespace(ctx, brokers, defaultNamespace, opts...)
}

// NewQueueWithNamespace is NewQueue with an explicit topic namespace
// (topics become "<namespace>.jobs.<runner_kind>" and
// "<namespace>.state") instead of the default "rk" -- exists purely so
// this package's own conformance tests can give each test run a fresh,
// uniquely-named set of topics, the same "fresh names, not a shared
// reset" convention Weaviate's/Pinecone's own tests use for their
// collections/indexes, since Kafka has no cheap, fast "wipe everything"
// primitive the way Redis's FLUSHALL or per-bucket NATS KV deletion do
// -- topic deletion is real broker-coordinated work, too slow to pay
// once per conformance sub-test.
//
// Ensures the compacted state topic exists, then starts tailing it and
// BLOCKS until this replica's own materialized view has caught up to
// the topic's state as of this call -- so a freshly constructed Queue
// never serves an Ack/Renew/Dequeue call against a stale (or empty)
// view of jobs another, longer-running replica already knows about. A
// brand-new namespace's own empty topic makes this effectively
// instant; the wait only matters for a replica joining after real
// traffic exists.
func NewQueueWithNamespace(ctx context.Context, brokers []string, namespace string, opts ...Option) (*Queue, error) {
	if namespace == "" {
		namespace = defaultNamespace
	}
	q := &Queue{
		brokers:       brokers,
		namespace:     namespace,
		jobPartitions: defaultJobPartitions,
		writer: &kafka.Writer{
			Addr:                   kafka.TCP(brokers...),
			Balancer:               &kafka.Hash{},
			AllowAutoTopicCreation: false,
			// kafka-go's own default BatchBytes (1MB) is a client-side
			// cap independent of the broker's message.max.bytes --
			// raised to match run payloads that can legitimately
			// approach a few hundred KB of JSON. Callers needing more
			// than this must raise both this and the broker's own
			// message.max.bytes together.
			BatchBytes: 8 << 20,
		},
		readers: make(map[string]*kafka.Reader),
		state:   make(map[string]stateEntry),
	}
	for _, opt := range opts {
		opt(q)
	}

	if err := ensureTopic(ctx, brokers, q.stateTopic(), 1, map[string]string{
		"cleanup.policy": "compact",
	}); err != nil {
		return nil, fmt.Errorf("kafkatransport: ensure state topic: %w", err)
	}

	targetOffset, err := readLastOffset(ctx, brokers, q.stateTopic(), 0)
	if err != nil {
		return nil, fmt.Errorf("kafkatransport: read state topic offset: %w", err)
	}

	tailCtx, cancel := context.WithCancel(context.Background())
	q.stopTailing = cancel
	caughtUp := make(chan struct{})
	go q.tailState(tailCtx, targetOffset, caughtUp)

	select {
	case <-caughtUp:
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
	return q, nil
}

// tailState continuously reads stateTopic from its own beginning (a
// direct, non-group partition reader -- every replica independently
// gets the full history, unlike the job topics' own consumer-group
// readers, which split work across replicas) and applies each record
// to q.state. Closes caughtUp once the reader has processed at least up
// through targetOffset -- see NewQueue's own doc comment for why.
func (q *Queue) tailState(ctx context.Context, targetOffset int64, caughtUp chan struct{}) {
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     q.brokers,
		Topic:       q.stateTopic(),
		Partition:   0,
		StartOffset: kafka.FirstOffset,
		MaxBytes:    readerMaxBytes,
	})
	defer reader.Close()

	notifiedCaughtUp := targetOffset <= 0
	if notifiedCaughtUp {
		close(caughtUp)
	}
	for {
		msg, err := reader.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		runID := string(msg.Key)
		q.stateMu.Lock()
		if len(msg.Value) == 0 {
			delete(q.state, runID)
		} else {
			var entry stateEntry
			if err := json.Unmarshal(msg.Value, &entry); err == nil {
				q.state[runID] = entry
			}
		}
		q.stateMu.Unlock()

		if !notifiedCaughtUp && msg.Offset >= targetOffset-1 {
			notifiedCaughtUp = true
			close(caughtUp)
		}
	}
}

func (q *Queue) getState(runID string) (stateEntry, bool) {
	q.stateMu.RLock()
	defer q.stateMu.RUnlock()
	e, ok := q.state[runID]
	return e, ok
}

// writeWithTopicRetry wraps a WriteMessages call with a bounded retry
// specifically for kafka.UnknownTopicOrPartition -- confirmed live: a
// topic created moments earlier (even one ensureTopic already verified
// exists via a direct ReadPartitions probe) can still get this error
// from a Writer for up to Transport's own default 6s cluster-metadata
// cache TTL, since kafka-go's default Transport caches topic/partition
// metadata process-wide (shared by every Writer using it, not scoped
// per-instance) and only refreshes it periodically, not reactively on
// a miss. Retrying past that window is the documented, idiomatic way
// to ride this out; ensureTopic's own readiness probe (which talks
// directly to the broker, bypassing this cache) is what actually
// guarantees the topic exists, this just waits for the Writer's cache
// to catch up to that reality.
func (q *Queue) writeWithTopicRetry(ctx context.Context, msg kafka.Message) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := q.writer.WriteMessages(ctx, msg)
		if err == nil || !errors.Is(err, kafka.UnknownTopicOrPartition) {
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// putState writes an entry (or, if entry is nil, a tombstone) to
// stateTopic, then immediately applies the same change to this
// replica's own in-memory materialized view (a write-through update)
// rather than waiting for tailState's own background consumption of
// the message it just produced.
//
// Confirmed live: without the write-through, a Dequeue immediately
// followed by an Ack/Renew/etc. on the SAME replica -- exactly the
// pattern the conformance suite's own fencing tests use, and a
// perfectly ordinary one for a real runner too -- would race
// tailState's own self-consumption lag (typically single-digit
// milliseconds, but not zero) and see a fencing check fail against a
// materialized view that hasn't caught up to a write this SAME
// process just made. The write-through only short-circuits that;
// other replicas still only learn of the change once their own
// tailState loop reads it back off the topic, same as before.
func (q *Queue) putState(ctx context.Context, runID string, entry *stateEntry) error {
	var value []byte
	if entry != nil {
		b, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		value = b
	}
	if err := q.writeWithTopicRetry(ctx, kafka.Message{
		Topic: q.stateTopic(),
		Key:   []byte(runID),
		Value: value, // nil = tombstone
	}); err != nil {
		return err
	}
	q.stateMu.Lock()
	if entry == nil {
		delete(q.state, runID)
	} else {
		q.state[runID] = *entry
	}
	q.stateMu.Unlock()
	return nil
}

func (q *Queue) readerFor(ctx context.Context, runnerKind string) (*kafka.Reader, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if r, ok := q.readers[runnerKind]; ok {
		return r, nil
	}
	if err := ensureTopic(ctx, q.brokers, q.jobTopic(runnerKind), q.jobPartitions, nil); err != nil {
		return nil, fmt.Errorf("kafkatransport: ensure job topic for runner_kind %q: %w", runnerKind, err)
	}
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  q.brokers,
		GroupID:  consumerGroupID(q.namespace, runnerKind),
		Topic:    q.jobTopic(runnerKind),
		MaxBytes: readerMaxBytes, // match Writer's own raised BatchBytes
	})
	q.readers[runnerKind] = r
	return r, nil
}

func (q *Queue) Enqueue(ctx context.Context, job *transport.RunAssignment) error {
	if entry, ok := q.getState(job.RunID); ok && entry.Canceled {
		return nil
	}
	if err := ensureTopic(ctx, q.brokers, q.jobTopic(job.RunnerKind), q.jobPartitions, nil); err != nil {
		return fmt.Errorf("kafkatransport: ensure job topic: %w", err)
	}
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return q.writeWithTopicRetry(ctx, kafka.Message{
		Topic: q.jobTopic(job.RunnerKind),
		Key:   []byte(job.RunID),
		Value: data,
	})
}

// Dequeue fetches the next message for runnerKind, writes a durable
// state-topic entry for it, and only then commits its offset -- see
// this package's own doc comment for why that's safe despite not yet
// being Acked: the state-topic entry, not Kafka's own committed offset,
// is what ReclaimStale/Nack actually use to find and re-deliver an
// unfinished job, so an offset commit "leapfrogging" a still-pending
// job (Kafka's documented behavior when a later message is committed
// before an earlier one) never causes that earlier job to be silently
// lost the way it would in a design relying on Kafka's own offset
// replay for redelivery.
//
// Order matters here, and is the opposite of this package's own first
// draft: the state-topic entry is written BEFORE the offset is
// committed, not after. Committing first (the original order) had a
// real, live-confirmable gap -- if the process died, or putState's own
// produce call failed, in the window between a successful commit and a
// successful putState, the job would be neither Kafka-redeliverable
// (its offset is already committed, so a fresh consumer resumes past
// it) nor reclaimable (no state-topic entry exists for ReclaimStale to
// find), silently lost for good. Writing state first and only
// committing once that succeeds means a crash or failure in that same
// window instead leaves the offset uncommitted -- a fresh consumer
// session (this process restarting, or a new replica taking over the
// partition) will fetch this exact message again, which is a
// duplicate-delivery risk, not a lost-job risk. Between the two, this
// package treats "occasionally redelivered" as the acceptable failure
// mode and "silently dropped" as the one to actively design out, same
// as the rest of this package's own crash-recovery reasoning.
//
// Unlike Redis's/NATS's own Dequeue, this deliberately does NOT cap and
// re-issue each FetchMessage call on a short internal timer while
// looping toward the overall deadline: confirmed live, doing so against
// a group-mode Reader can pathologically starve a still-in-progress
// consumer-group join (JoinGroup/SyncGroup) by cancelling and
// re-triggering it on every short-lived sub-call before it ever
// completes, rather than simply respecting the caller's own timeout
// once. A single FetchMessage per loop iteration, bounded by the full
// remaining budget, lets a runner_kind's first-ever join (a real,
// bounded, one-time cost per runner_kind per replica -- see this
// package's own doc comment) finish inside one call instead of never
// finishing at all. The loop itself still exists, but only to skip past
// canceled jobs and re-check ctx between messages, not to re-cap a
// single fetch.
func (q *Queue) Dequeue(ctx context.Context, runnerKind string, timeout time.Duration) (*transport.RunAssignment, error) {
	reader, err := q.readerFor(ctx, runnerKind)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil
		}
		fetchCtx, cancel := context.WithTimeout(ctx, remaining)
		msg, err := reader.FetchMessage(fetchCtx)
		cancel()
		if err != nil {
			if fetchCtx.Err() != nil {
				return nil, nil // overall deadline reached, no job available
			}
			return nil, err
		}

		var job transport.RunAssignment
		if err := json.Unmarshal(msg.Value, &job); err != nil {
			// Malformed payload: can never be processed regardless of
			// how many times it's redelivered, so commit past it now
			// rather than leaving every future Dequeue stuck refetching
			// the same unparseable message forever.
			if commitErr := reader.CommitMessages(context.Background(), msg); commitErr != nil {
				return nil, fmt.Errorf("kafkatransport: commit past malformed payload: %w", commitErr)
			}
			continue
		}

		if entry, ok := q.getState(job.RunID); ok && entry.Canceled {
			if err := reader.CommitMessages(context.Background(), msg); err != nil {
				return nil, fmt.Errorf("kafkatransport: commit past canceled job: %w", err)
			}
			continue
		}

		// A job's own generation is 0 only the very first time it's
		// ever dequeued (Enqueue never sets it); ReclaimStale/Nack bake
		// the bumped generation into the re-produced payload itself
		// (see their own comments for why), so a redelivered job
		// already carries its correct, non-zero generation here.
		if job.Generation == 0 {
			job.Generation = 1
		}
		// putState before CommitMessages -- see this function's own
		// doc comment for why the order is load-bearing. Not committing
		// on a putState failure is deliberate: it leaves this message
		// redeliverable to a future consumer session instead of losing
		// it outright.
		if err := q.putState(ctx, job.RunID, &stateEntry{
			RunnerKind:         runnerKind,
			Generation:         job.Generation,
			TouchedAtUnixMilli: nowMillis(),
			Job:                msg.Value,
		}); err != nil {
			return nil, fmt.Errorf("kafkatransport: record inflight state: %w", err)
		}
		if err := reader.CommitMessages(context.Background(), msg); err != nil {
			return nil, fmt.Errorf("kafkatransport: commit offset: %w", err)
		}
		return &job, nil
	}
}

// fenced looks up the state entry for runID and decides whether the
// presented generation is allowed to act on it -- same shape as
// natstransport's own fenced helper (generation 0 from either side
// bypasses fencing, matching transport.JobQueue's own pre-fencing-
// runner-build compat rationale).
func (q *Queue) fenced(runID string, generation int64) (stateEntry, bool) {
	entry, ok := q.getState(runID)
	if !ok || entry.Canceled {
		return stateEntry{}, false
	}
	if generation != 0 && entry.Generation != 0 && entry.Generation != generation {
		return stateEntry{}, false
	}
	return entry, true
}

func (q *Queue) Ack(ctx context.Context, runID string, generation int64) (bool, error) {
	if _, ok := q.fenced(runID, generation); !ok {
		return false, nil
	}
	if err := q.putState(ctx, runID, nil); err != nil {
		return false, err
	}
	return true, nil
}

func (q *Queue) Renew(ctx context.Context, runID string, generation int64) (bool, error) {
	entry, ok := q.fenced(runID, generation)
	if !ok {
		return false, nil
	}
	entry.TouchedAtUnixMilli = nowMillis()
	if err := q.putState(ctx, runID, &entry); err != nil {
		return false, err
	}
	return true, nil
}

// LookupInflight returns the currently tracked assignment for runID.
func (q *Queue) LookupInflight(ctx context.Context, runID string) (*transport.RunAssignment, error) {
	entry, ok := q.getState(runID)
	if !ok || entry.Canceled || len(entry.Job) == 0 {
		return nil, nil
	}
	var job transport.RunAssignment
	if err := json.Unmarshal(entry.Job, &job); err != nil {
		return nil, err
	}
	// Prefer the state topic's fencing generation over whatever is
	// embedded in the job payload -- reclaim bumps both, but reading
	// the authoritative state entry avoids a stale embedded value.
	if entry.Generation != 0 {
		job.Generation = entry.Generation
	}
	return &job, nil
}

// bumpGeneration unmarshals a job payload, sets its embedded
// generation field to newGen, and re-marshals it -- unlike Redis's own
// Lua-side generation bump (string surgery on the raw JSON, chosen
// there to stay inside a single atomic EVAL), this runs in Go with a
// real json.Unmarshal/Marshal round trip, so there's no equivalent to
// the nested-"generation"-in-input string-match bug that particular
// approach had.
func bumpGeneration(payload []byte, newGen int64) ([]byte, error) {
	var job transport.RunAssignment
	if err := json.Unmarshal(payload, &job); err != nil {
		return nil, err
	}
	job.Generation = newGen
	return json.Marshal(&job)
}

func (q *Queue) Nack(ctx context.Context, runID string) error {
	entry, ok := q.getState(runID)
	if !ok || entry.Canceled {
		return nil // not in-flight -- nothing to do, same as Redis's/NATS's Nack
	}
	payload, err := bumpGeneration(entry.Job, entry.Generation+1)
	if err != nil {
		return fmt.Errorf("kafkatransport: bump generation for nack: %w", err)
	}
	if err := q.writeWithTopicRetry(ctx, kafka.Message{
		Topic: q.jobTopic(entry.RunnerKind),
		Key:   []byte(runID),
		Value: payload,
	}); err != nil {
		return fmt.Errorf("kafkatransport: re-produce nacked job: %w", err)
	}
	return q.putState(ctx, runID, nil)
}

func (q *Queue) Cancel(ctx context.Context, runID string) error {
	return q.putState(ctx, runID, &stateEntry{Canceled: true})
}

// ReclaimStale re-enqueues (re-produces to the job topic, bumping the
// fencing generation) any job whose state entry hasn't been touched
// (Dequeue or Renew) in more than maxAge. Cross-replica mutual
// exclusion is NOT provided here -- use cmd's Redis reclaim-leader
// lock when REDIS_URL is set. The local re-check before produce only
// skips work already reflected in this process's materialized view.
func (q *Queue) ReclaimStale(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := nowMillis() - maxAge.Milliseconds()

	q.stateMu.RLock()
	var stale []struct {
		runID string
		entry stateEntry
	}
	for runID, entry := range q.state {
		if !entry.Canceled && entry.TouchedAtUnixMilli <= cutoff {
			stale = append(stale, struct {
				runID string
				entry stateEntry
			}{runID, entry})
		}
	}
	q.stateMu.RUnlock()

	reclaimed := 0
	for _, s := range stale {
		// Soft local re-check: another replica's putState may already
		// have been consumed into our map since the snapshot. Does NOT
		// close dual-replica races (both can still produce before either
		// putState lands) -- that is the reclaim-leader lock's job.
		if cur, ok := q.getState(s.runID); !ok || cur.Canceled || cur.Generation != s.entry.Generation || cur.TouchedAtUnixMilli > cutoff {
			continue
		}
		newGen := s.entry.Generation + 1
		payload, err := bumpGeneration(s.entry.Job, newGen)
		if err != nil {
			continue
		}
		if err := q.writeWithTopicRetry(ctx, kafka.Message{
			Topic: q.jobTopic(s.entry.RunnerKind),
			Key:   []byte(s.runID),
			Value: payload,
		}); err != nil {
			continue
		}
		s.entry.Generation = newGen
		s.entry.TouchedAtUnixMilli = nowMillis()
		if err := q.putState(ctx, s.runID, &s.entry); err != nil {
			continue
		}
		reclaimed++
	}
	return reclaimed, nil
}

// Len sums each of this namespace's own job topics' consumer-group lag
// (messages produced minus messages committed) -- matching Redis's/
// NATS's own Len semantics of "not yet dequeued," not "not yet Acked."
// Topics are discovered via a plain, no-topic-argument ReadPartitions
// call (which returns every partition on the whole cluster, not just
// ones this Queue instance has touched), filtered to this namespace's
// own job-topic prefix -- the same cross-replica "don't rely on this
// one process's own local knowledge" property Redis's SCAN and NATS's
// ConsumerNames listing give Len for their own backends.
func (q *Queue) Len(ctx context.Context) (int64, error) {
	conn, err := kafka.DialContext(ctx, "tcp", q.brokers[0])
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	// ReadPartitions returns one entry PER PARTITION, not one per topic
	// -- a multi-partition job topic (see WithJobPartitions) appears
	// here multiple times, once per Partition index, and every one of
	// them needs its own lag counted, not just whichever happens to be
	// first in the returned list.
	partitions, err := conn.ReadPartitions()
	if err != nil {
		return 0, err
	}

	prefix := q.namespace + ".jobs."
	var total int64
	for _, p := range partitions {
		if !strings.HasPrefix(p.Topic, prefix) {
			continue
		}
		kind := strings.TrimPrefix(p.Topic, prefix)

		last, err := readLastOffset(ctx, q.brokers, p.Topic, p.ID)
		if err != nil {
			continue
		}
		committed, err := readCommittedOffset(ctx, q.brokers, p.Topic, consumerGroupID(q.namespace, kind), p.ID)
		if err != nil {
			continue
		}
		if lag := last - committed; lag > 0 {
			total += lag
		}
	}
	return total, nil
}

// Ping verifies the Kafka cluster is reachable right now by dialing a
// broker and reading the controller's own metadata -- a cheap
// connectivity check, deliberately not the partition/offset walk Len
// does above (that's proportional to job-topic count and meant for an
// occasional gauge poll, not every readiness probe).
func (q *Queue) Ping(ctx context.Context) error {
	conn, err := kafka.DialContext(ctx, "tcp", q.brokers[0])
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Controller()
	return err
}

// Close releases this Queue's own background resources (the state
// tailer and every runner_kind reader) -- for graceful shutdown/tests,
// not part of transport.JobQueue itself.
func (q *Queue) Close() error {
	q.stopTailing()
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, r := range q.readers {
		_ = r.Close()
	}
	return q.writer.Close()
}

// --------------------------------------------------------------------------
// Admin helpers
// --------------------------------------------------------------------------

// ensureTopic creates a topic if it doesn't already exist -- idempotent
// (a "topic already exists" error from the broker is treated as
// success), same non-versioned Init convention as every other
// backend's own idempotent setup.
//
// Confirmed live: CreateTopics returning success does not mean the new
// topic's metadata has propagated to every broker/client-facing
// endpoint yet -- a Produce/Fetch immediately afterward can still see
// "Unknown Topic Or Partition" for a real, if usually short, window.
// ensureTopic polls ReadPartitions after creating a topic until the
// broker actually reports it (or a short timeout elapses) so callers
// never race this window themselves.
func ensureTopic(ctx context.Context, brokers []string, topic string, partitions int, config map[string]string) error {
	conn, err := kafka.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return err
	}
	defer conn.Close()
	controller, err := conn.Controller()
	if err != nil {
		return err
	}
	controllerConn, err := kafka.DialContext(ctx, "tcp", net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port)))
	if err != nil {
		return err
	}
	defer controllerConn.Close()

	var entries []kafka.ConfigEntry
	for k, v := range config {
		entries = append(entries, kafka.ConfigEntry{ConfigName: k, ConfigValue: v})
	}
	err = controllerConn.CreateTopics(kafka.TopicConfig{
		Topic:             topic,
		NumPartitions:     partitions,
		ReplicationFactor: 1,
		ConfigEntries:     entries,
	})
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if partitionsExist, checkErr := conn.ReadPartitions(topic); checkErr == nil && len(partitionsExist) > 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("kafkatransport: topic %q did not become ready within the wait window", topic)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

func readLastOffset(ctx context.Context, brokers []string, topic string, partition int) (int64, error) {
	conn, err := kafka.DialLeader(ctx, "tcp", brokers[0], topic, partition)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	return conn.ReadLastOffset()
}

func readCommittedOffset(ctx context.Context, brokers []string, topic, groupID string, partition int) (int64, error) {
	client := &kafka.Client{Addr: kafka.TCP(brokers...)}
	resp, err := client.OffsetFetch(ctx, &kafka.OffsetFetchRequest{
		GroupID: groupID,
		Topics:  map[string][]int{topic: {partition}},
	})
	if err != nil {
		return 0, err
	}
	for _, part := range resp.Topics[topic] {
		if part.Partition == partition {
			if part.CommittedOffset < 0 {
				return 0, nil // no committed offset yet
			}
			return part.CommittedOffset, nil
		}
	}
	return 0, nil
}

// Compile-time interface check.
var _ transport.JobQueue = (*Queue)(nil)
