// Package natstransport implements the full transport.JobQueue,
// transport.EventBroker, and transport.CancelBroker triad using NATS
// JetStream (queue and events) and NATS core pub/sub (cancel signals) --
// the same three-interface completeness Redis's own transport package
// provides. Kafka and SQS, by contrast, only get transport.JobQueue --
// see their own package docs for why NATS specifically gets the full
// triad: it has native pub/sub and a durable stream primitive in one
// system, so it doesn't need a second, unrelated technology bolted on
// just for events/cancel the way a pure queue system would.
//
// JobQueue uses one JetStream stream (subjects "rk.jobs.<runner_kind>")
// with WorkQueuePolicy retention, one durable pull consumer per
// runner_kind. NATS gives real, native primitives for two of the three
// crash-recovery problems this project had to hand-build for
// Redis: AckWait (a per-message redelivery timer, closing the
// "ongoing liveness" problem) and InProgress (heartbeat that resets
// that timer without acking, backing Renew directly). Fencing is the
// one piece NOT native -- confirmed via NATS's own still-open
// nats-server#4786: JetStream currently accepts an ack arriving after
// a message has already been redelivered to a different consumer,
// exactly the same "stale runner's late report clobbers the current
// attempt" gap Redis needed generation fencing for. This package closes
// it the same way conceptually, but keyed off NATS's own delivery
// metadata instead of a hand-rolled counter: a message's NumDelivered
// (1 on first delivery, incremented natively by the server on every
// redelivery) IS this package's fencing generation -- no separate
// generation counter to keep in sync with NATS's own redelivery state,
// since it literally is that state.
//
// A JetStream KV bucket ("RUNKITE_INFLIGHT") tracks, per run_id, the
// reply subject and generation of whichever delivery is currently
// in-flight -- the same "shared, not process-local" requirement the
// Redis transport uses for multi-replica reclaim, and for the same
// reason: the control-plane replica that later needs to Ack/Renew/Nack
// a job is not necessarily the same one whose Fetch call originally
// received it (no session affinity across a load-balanced gRPC bridge).
// A NATS reply subject is just a string, so any replica holding it can
// publish directly to it (the same raw "+ACK"/"-NAK"/"+WPI"/"+TERM"
// bytes jetstream.Msg.Ack/Nak/InProgress/Term publish internally)
// without needing the original in-process Msg handle that received it.
package natstransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/getrunkite/runkite/internal/transport"
)

// --------------------------------------------------------------------------
// JobQueue
// --------------------------------------------------------------------------

const (
	jobsStreamName   = "RUNKITE_JOBS"
	inflightBucket   = "RUNKITE_INFLIGHT"
	canceledBucket   = "RUNKITE_CANCELED"
	jobSubjectPrefix = "rk.jobs."
)

func jobSubject(runnerKind string) string { return jobSubjectPrefix + runnerKind }

// consumerName derives a durable consumer name from a runner_kind --
// NATS consumer/stream names may not contain '.', unlike subjects, so a
// runner_kind containing one (unlikely in practice, but not otherwise
// forbidden by this project's own conventions) is sanitized rather than
// silently producing an invalid consumer name the server would reject.
func consumerName(runnerKind string) string {
	return "jobs-" + strings.ReplaceAll(runnerKind, ".", "_")
}

// inflightEntry is the JSON value stored in inflightBucket, keyed by
// run_id. Generation is NumDelivered at the time this entry was written
// (see this package's own doc comment for why that's the fencing value,
// not a separately-maintained counter) -- Ack/Renew compare a caller's
// presented generation against THIS stored value, the same fencing
// shape as transport.JobQueue's own doc comments describe, just backed
// by JetStream's native delivery count instead of Redis's hand-rolled
// HINCRBY one.
type inflightEntry struct {
	Reply      string `json:"reply"`
	Generation int64  `json:"generation"`
	// TouchedAtUnixMilli drives ReclaimStale's own staleness check --
	// independent of (and not necessarily equal to) whatever AckWait
	// the consumer happens to be configured with, since ReclaimStale
	// takes its OWN maxAge per call, same as Redis's version.
	TouchedAtUnixMilli int64           `json:"touched_at_ms"`
	Job                json.RawMessage `json:"job"`
}

// nowMillis is a var (not a plain time.Now call) so tests can freeze/
// control it without sleeping in wall-clock time -- same convention as
// redistransport's own nowMillis.
var nowMillis = func() int64 { return time.Now().UnixMilli() }

// Queue implements transport.JobQueue using JetStream. Consumers are
// created lazily (on first Enqueue OR Dequeue for a given runner_kind --
// both, not just Dequeue, so Len can report an accurate pending count
// for a runner_kind that's had jobs enqueued but never yet dequeued in
// this process's lifetime; CreateOrUpdateConsumer is idempotent, so
// calling it from both places is harmless).
type Queue struct {
	nc *nats.Conn
	js jetstream.JetStream

	mu        sync.Mutex
	stream    jetstream.Stream
	inflight  jetstream.KeyValue
	canceled  jetstream.KeyValue
	consumers map[string]jetstream.Consumer // runner_kind -> consumer
}

// NewQueue creates a JetStream-backed job queue, creating the
// underlying stream and KV buckets if they don't already exist (all
// idempotent -- safe to call from every control-plane replica on
// startup, same convention as every other transport's own Init/New).
func NewQueue(ctx context.Context, nc *nats.Conn) (*Queue, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("natstransport: create jetstream context: %w", err)
	}
	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      jobsStreamName,
		Subjects:  []string{jobSubjectPrefix + ">"},
		Retention: jetstream.WorkQueuePolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("natstransport: create jobs stream: %w", err)
	}
	inflight, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: inflightBucket})
	if err != nil {
		return nil, fmt.Errorf("natstransport: create inflight bucket: %w", err)
	}
	// TTL mirrors Redis canceledMemberTTL (24h): cancel markers only need
	// to outlive reclaim; without TTL the KV bucket grows without bound
	// for every canceled run_id over the life of the stream.
	canceled, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: canceledBucket,
		TTL:    24 * time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("natstransport: create canceled bucket: %w", err)
	}
	return &Queue{
		nc:        nc,
		js:        js,
		stream:    stream,
		inflight:  inflight,
		canceled:  canceled,
		consumers: make(map[string]jetstream.Consumer),
	}, nil
}

// consumerFor returns (creating if needed) the durable pull consumer
// for runnerKind. MaxDeliver is left unbounded (-1, the server's own
// default): this package's own fencing is what tells a superseded
// runner to stop, not JetStream's poison-pill cutoff, which would
// otherwise permanently drop a job after N stale-timeout cycles
// regardless of whether a runner ever actually picks it up.
func (q *Queue) consumerFor(ctx context.Context, runnerKind string) (jetstream.Consumer, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if c, ok := q.consumers[runnerKind]; ok {
		return c, nil
	}
	c, err := q.js.CreateOrUpdateConsumer(ctx, jobsStreamName, jetstream.ConsumerConfig{
		Durable:       consumerName(runnerKind),
		FilterSubject: jobSubject(runnerKind),
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    -1,
	})
	if err != nil {
		return nil, fmt.Errorf("natstransport: create consumer for runner_kind %q: %w", runnerKind, err)
	}
	q.consumers[runnerKind] = c
	return c, nil
}

func (q *Queue) isCanceled(ctx context.Context, runID string) bool {
	_, err := q.canceled.Get(ctx, runID)
	return err == nil
}

func (q *Queue) Enqueue(ctx context.Context, job *transport.RunAssignment) error {
	if q.isCanceled(ctx, job.RunID) {
		return nil
	}
	if _, err := q.consumerFor(ctx, job.RunnerKind); err != nil {
		return err
	}
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	_, err = q.js.Publish(ctx, jobSubject(job.RunnerKind), data)
	return err
}

// dequeueFetchCap bounds a single Fetch call's wait, same reasoning as
// redistransport's own dequeueBlockCap: a zombie GetJob call (runner
// connection already dead, ctx not yet cancelled server-side) must not
// hold a single blocking call for the entire caller-supplied timeout.
const dequeueFetchCap = 2 * time.Second

func (q *Queue) Dequeue(ctx context.Context, runnerKind string, timeout time.Duration) (*transport.RunAssignment, error) {
	consumer, err := q.consumerFor(ctx, runnerKind)
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
		wait := remaining
		if wait > dequeueFetchCap {
			wait = dequeueFetchCap
		}

		batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(wait))
		if err != nil {
			return nil, err
		}
		msg, ok := <-batch.Messages()
		if !ok {
			if err := batch.Error(); err != nil && !errors.Is(err, jetstream.ErrNoMessages) && !errors.Is(err, nats.ErrTimeout) {
				return nil, err
			}
			continue // no message within this Fetch's wait -- retry until the overall deadline
		}

		var job transport.RunAssignment
		if err := json.Unmarshal(msg.Data(), &job); err != nil {
			_ = msg.Term() // malformed payload can never succeed -- don't leave it redelivering forever
			continue
		}

		if q.isCanceled(ctx, job.RunID) {
			_ = msg.Term()
			continue
		}

		meta, err := msg.Metadata()
		if err != nil {
			return nil, fmt.Errorf("natstransport: read message metadata: %w", err)
		}
		job.Generation = int64(meta.NumDelivered)

		entry := inflightEntry{Reply: msg.Reply(), Generation: job.Generation, TouchedAtUnixMilli: nowMillis(), Job: msg.Data()}
		entryJSON, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		if _, err := q.inflight.Put(ctx, job.RunID, entryJSON); err != nil {
			return nil, fmt.Errorf("natstransport: record inflight entry: %w", err)
		}
		return &job, nil
	}
}

// fenced looks up the inflight entry for runID and decides whether the
// presented generation is allowed to act on it -- shared logic between
// Ack and Renew, mirroring redistransport's ackScript/renewScript
// fencing check (generation 0, from either side, bypasses fencing --
// see transport.JobQueue's own doc comment for the pre-fencing-runner
// compat rationale this preserves).
func (q *Queue) fenced(ctx context.Context, runID string, generation int64) (entry inflightEntry, revision uint64, ok bool) {
	kve, err := q.inflight.Get(ctx, runID)
	if err != nil {
		return inflightEntry{}, 0, false
	}
	if err := json.Unmarshal(kve.Value(), &entry); err != nil {
		return inflightEntry{}, 0, false
	}
	if generation != 0 && entry.Generation != 0 && entry.Generation != generation {
		return inflightEntry{}, 0, false
	}
	return entry, kve.Revision(), true
}

func (q *Queue) Ack(ctx context.Context, runID string, generation int64) (bool, error) {
	entry, revision, ok := q.fenced(ctx, runID, generation)
	if !ok {
		return false, nil
	}
	if err := q.nc.Publish(entry.Reply, []byte("+ACK")); err != nil {
		return false, err
	}
	// Delete only if nothing else touched this entry since the read
	// above (a concurrent Renew/ReclaimStale on another replica) --
	// same optimistic-concurrency guard Update below uses. If deletion
	// loses that race, the entry belongs to whatever superseded this
	// generation anyway, so leaving it for that path to manage is
	// correct, not a leak.
	_ = q.inflight.Delete(ctx, runID, jetstream.LastRevision(revision))
	return true, nil
}

func (q *Queue) Renew(ctx context.Context, runID string, generation int64) (bool, error) {
	entry, revision, ok := q.fenced(ctx, runID, generation)
	if !ok {
		return false, nil
	}
	if err := q.nc.Publish(entry.Reply, []byte("+WPI")); err != nil {
		return false, err
	}
	entry.TouchedAtUnixMilli = nowMillis()
	updated, err := json.Marshal(entry)
	if err != nil {
		return false, err
	}
	// Update (not Put): if a concurrent ReclaimStale/Ack on another
	// replica already changed or removed this entry since fenced's own
	// read above, this fails instead of resurrecting/overwriting
	// whatever that concurrent call did -- the same race Redis's own
	// renewScript closes by doing its check-then-write as one atomic
	// Lua script, done here via JetStream KV's native
	// compare-and-swap-by-revision instead.
	if _, err := q.inflight.Update(ctx, runID, updated, revision); err != nil {
		return false, nil
	}
	return true, nil
}

func (q *Queue) Nack(ctx context.Context, runID string) error {
	kve, err := q.inflight.Get(ctx, runID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil // not in-flight -- nothing to do, same as Redis's Nack
		}
		return err
	}
	var entry inflightEntry
	if err := json.Unmarshal(kve.Value(), &entry); err != nil {
		return err
	}
	// -NAK triggers immediate redelivery (no backoff/delay applied),
	// unlike letting AckWait expire -- the message reappears for the
	// NEXT Fetch on this runner_kind's consumer with NumDelivered
	// incremented by the server itself, which Dequeue picks up as the
	// new fencing generation automatically.
	if err := q.nc.Publish(entry.Reply, []byte("-NAK")); err != nil {
		return err
	}
	return q.inflight.Delete(ctx, runID, jetstream.LastRevision(kve.Revision()))
}

func (q *Queue) Cancel(ctx context.Context, runID string) error {
	if kve, err := q.inflight.Get(ctx, runID); err == nil {
		var entry inflightEntry
		if json.Unmarshal(kve.Value(), &entry) == nil {
			// +TERM stops redelivery permanently (unlike NAK, which
			// immediately redelivers) -- the job must not come back
			// for this or any later runner_kind consumer.
			_ = q.nc.Publish(entry.Reply, []byte("+TERM"))
		}
		_ = q.inflight.Delete(ctx, runID, jetstream.LastRevision(kve.Revision()))
	}
	_, err := q.canceled.Put(ctx, runID, []byte("1"))
	return err
}

// ReclaimStale re-enqueues (via NAK, triggering JetStream's own
// immediate redelivery -- see Nack's own doc comment) any job whose
// inflight entry hasn't been touched (Dequeue or Renew) in more than
// maxAge. Unlike redistransport's own ReclaimStale, this is NOT the
// only thing standing between a crashed runner and a lost job -- NATS's
// own AckWait timer independently redelivers on its own schedule
// regardless of whether anything ever calls this method. This exists
// for the same reason Redis's version does anyway: an operator-driven
// maxAge independent of whatever AckWait a consumer happens to be
// configured with, and a deterministic, on-demand way to force a
// reclaim (what the shared conformance suite's own fencing tests rely
// on) rather than only ever waiting out a real timer.
func (q *Queue) ReclaimStale(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := nowMillis() - maxAge.Milliseconds()
	keys, err := q.inflight.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return 0, nil
		}
		return 0, err
	}
	reclaimed := 0
	for _, key := range keys {
		kve, err := q.inflight.Get(ctx, key)
		if err != nil {
			continue // deleted concurrently (Ack'd/Nack'd elsewhere) -- not stale, just gone
		}
		var entry inflightEntry
		if err := json.Unmarshal(kve.Value(), &entry); err != nil {
			continue
		}
		if entry.TouchedAtUnixMilli > cutoff {
			continue
		}
		if err := q.nc.Publish(entry.Reply, []byte("-NAK")); err != nil {
			continue
		}
		if err := q.inflight.Delete(ctx, key, jetstream.LastRevision(kve.Revision())); err == nil {
			reclaimed++
		}
	}
	return reclaimed, nil
}

// Len sums NumPending (delivered-to-nobody-yet, matching Redis's own
// Len semantics of "not yet dequeued" rather than "not yet acked")
// across every runner_kind consumer discovered on the stream --
// mirroring redistransport's own SCAN-based discovery rather than
// relying on this one process's own in-memory consumers map, which
// (matching a real bug Redis's own local map once had before being
// fixed the same way) wouldn't see runner_kinds another replica
// created consumers for.
func (q *Queue) Len(ctx context.Context) (int64, error) {
	var total int64
	lister := q.stream.ConsumerNames(ctx)
	for name := range lister.Name() {
		info, err := q.js.Consumer(ctx, jobsStreamName, name)
		if err != nil {
			continue
		}
		ci, err := info.Info(ctx)
		if err != nil {
			continue
		}
		total += int64(ci.NumPending)
	}
	return total, lister.Err()
}

// Ping verifies the NATS connection is actually up by round-tripping a
// real PING/PONG -- not just checking the client's own cached connection
// state, since the client's background reconnect logic could be mid-
// retry without having flipped that state yet. The broker/cancelbus
// halves of this same package share this one *nats.Conn, so this single
// check covers all three transport.* interfaces this package implements
// (unlike Redis, which needs Ping on Broker/CancelBus too because it can
// be paired with a different queue backend -- see redis.Broker.Ping).
func (q *Queue) Ping(ctx context.Context) error {
	_, err := q.nc.RTT()
	return err
}

// --------------------------------------------------------------------------
// EventBroker
// --------------------------------------------------------------------------

const (
	eventsStreamName   = "RUNKITE_EVENTS"
	eventSubjectPrefix = "rk.events."
)

func eventSubject(runID string) string { return eventSubjectPrefix + runID }

// eventStreamMaxAge bounds how long an individual event message survives
// on the stream before JetStream's own per-message expiry removes it --
// applied message-by-message, not as a whole-stream/whole-run TTL the
// way Redis's EXPIRE-based approach works. This is a meaningfully
// different (and arguably more naturally correct) failure mode than
// Redis needed a two-tier eventStreamTTL/hungRunStreamTTL safety net
// for: a run that's still genuinely running keeps producing fresh
// messages with their own fresh expiry, so only a run's OLDEST events
// age out on this schedule, never its whole history at once the way an
// entire Redis key expiring would -- there's no equivalent of Redis's
// confirmed "5,476 accumulated keys with no expiry at all" gap to close
// here, since every message gets one from the moment it's stored.
const eventStreamMaxAge = 24 * time.Hour

// Broker implements transport.EventBroker using a single JetStream
// stream shared across every run (subjects "rk.events.<run_id>"), the
// same one-shared-stream/per-run-subject shape Redis's own Streams-based
// Broker uses (one Redis Stream key per run there; one subject on one
// NATS stream here -- functionally equivalent partitioning, adapted to
// each system's own idiom).
type Broker struct {
	js     jetstream.JetStream
	stream jetstream.Stream
}

// NewBroker creates a JetStream-backed event broker, creating the
// underlying stream if it doesn't already exist.
func NewBroker(ctx context.Context, nc *nats.Conn) (*Broker, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("natstransport: create jetstream context: %w", err)
	}
	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      eventsStreamName,
		Subjects:  []string{eventSubjectPrefix + ">"},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    eventStreamMaxAge,
	})
	if err != nil {
		return nil, fmt.Errorf("natstransport: create events stream: %w", err)
	}
	return &Broker{js: js, stream: stream}, nil
}

func (b *Broker) Publish(ctx context.Context, runID string, event *transport.RunEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = b.js.Publish(ctx, eventSubject(runID), data)
	return err
}

// Subscribe tails runID's subject via an ordered consumer starting
// strictly after whatever's already on the stream at the moment this
// call captures the stream's own last sequence -- synchronously, before
// the consumer is created, same race Redis's own Subscribe doc comment
// describes closing (a symbolic "start from new" policy would resolve
// once the consumer actually starts server-side, not when this call
// returns, and any Publish landing in that gap would be silently lost).
// Ordered consumers need no explicit Ack -- they're a client-managed,
// AckNone-policy read cursor by design, matching Redis Streams' own
// XREAD being a pure read with no ack concept either.
func (b *Broker) Subscribe(ctx context.Context, runID string) (<-chan *transport.RunEvent, error) {
	ch := make(chan *transport.RunEvent, 4096)

	info, err := b.stream.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("natstransport: stream info: %w", err)
	}
	startSeq := info.State.LastSeq + 1

	consumer, err := b.js.OrderedConsumer(ctx, eventsStreamName, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{eventSubject(runID)},
		DeliverPolicy:  jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:    startSeq,
	})
	if err != nil {
		return nil, fmt.Errorf("natstransport: create ordered consumer: %w", err)
	}

	subCtx, cancel := context.WithCancel(ctx)
	consumeCtx, err := consumer.Consume(func(msg jetstream.Msg) {
		var event transport.RunEvent
		if err := json.Unmarshal(msg.Data(), &event); err != nil {
			return
		}
		select {
		case ch <- &event:
		case <-subCtx.Done():
			return
		}
		if event.IsTerminal() {
			cancel()
		}
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("natstransport: consume: %w", err)
	}

	go func() {
		<-subCtx.Done()
		consumeCtx.Stop()
		close(ch)
	}()

	return ch, nil
}

// Replay fetches every stored event for runID and filters to sinceSeq
// client-side, the same shape as Redis's own XRange-then-filter Replay.
//
// Deliberately a plain ephemeral pull consumer (CreateConsumer with no
// Durable name), NOT an OrderedConsumer despite Subscribe using one --
// confirmed live that OrderedConsumer's own self-healing reset logic
// (designed for a long-lived continuous tail, which is what Subscribe
// actually needs) does not behave well driven by repeated FetchNoWait
// calls for a one-shot "read everything currently there, then stop"
// batch job: it hung retrying an internal consumer reset indefinitely
// in exactly this usage. A plain ephemeral consumer has none of that
// machinery -- explicitly deleted once this call is done rather than
// relying on InactiveThreshold to eventually clean it up.
func (b *Broker) Replay(ctx context.Context, runID string, sinceSeq int64) ([]*transport.RunEvent, error) {
	consumer, err := b.js.CreateConsumer(ctx, eventsStreamName, jetstream.ConsumerConfig{
		FilterSubject: eventSubject(runID),
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckPolicy:     jetstream.AckNonePolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("natstransport: create replay consumer: %w", err)
	}
	defer func() {
		info, infoErr := consumer.Info(context.Background())
		if infoErr == nil {
			_ = b.js.DeleteConsumer(context.Background(), eventsStreamName, info.Name)
		}
	}()

	var events []*transport.RunEvent
	for {
		batch, err := consumer.FetchNoWait(256)
		if err != nil {
			return nil, err
		}
		n := 0
		for msg := range batch.Messages() {
			n++
			var event transport.RunEvent
			if err := json.Unmarshal(msg.Data(), &event); err != nil {
				continue
			}
			if event.Seq > sinceSeq {
				events = append(events, &event)
			}
		}
		if err := batch.Error(); err != nil && !errors.Is(err, jetstream.ErrNoMessages) {
			return nil, err
		}
		if n == 0 {
			break
		}
	}
	return events, nil
}

// Close is a documented no-op: unlike Redis (which sets an explicit
// "closed" marker so a late Subscribe on an already-finished run gets
// an immediately-closed channel), a NATS ordered consumer created after
// a run's terminal event simply has nothing left to deliver -- Consume
// never fires its handler, and the channel this package returns is
// never closed on its own for that case. This is a real, narrower
// behavioral gap versus Redis: a late Subscribe here returns an open
// channel that silently never receives anything, rather than one a
// caller can distinguish as "already closed" via a receive-with-ok
// check. Acceptable for now since every current caller (waitForRunResult,
// the run event WebSocket/SSE handlers) already has its own independent
// terminal-status check against the run's stored state before or
// alongside subscribing, so a silently-empty channel here doesn't strand
// a caller waiting forever in practice -- but a future caller relying
// solely on this channel's own closure to detect "already done" would
// be wrong to assume Close's behavior matches Redis's.
func (b *Broker) Close(runID string) error { return nil }

// --------------------------------------------------------------------------
// CancelBroker
// --------------------------------------------------------------------------

// CancelBus implements transport.CancelBroker using plain NATS core
// pub/sub (no JetStream) -- a cancel signal is transient, at-most-once,
// no-replay-needed by design (see transport.CancelBroker's own doc
// comment), the exact shape core NATS pub/sub already is, same
// reasoning Redis uses its own Pub/Sub (not Streams) for this.
type CancelBus struct {
	nc *nats.Conn
}

// NewCancelBus creates a NATS-core-backed cancel broker.
func NewCancelBus(nc *nats.Conn) *CancelBus {
	return &CancelBus{nc: nc}
}

func cancelSubject(runID string) string { return "rk.cancel." + runID }

func (c *CancelBus) PublishCancel(ctx context.Context, runID string) error {
	return c.nc.Publish(cancelSubject(runID), []byte("cancel"))
}

// SubscribeCancel creates a core NATS subscription synchronously (NATS
// core subscriptions take effect immediately on the call that creates
// them -- no separate confirmation round trip the way Redis's own
// pubsub.Receive() needs) then spawns a goroutine to wait for the
// cancel message.
//
// ctx cancellation MUST release this subscription without closing ch --
// same contract, same reasoning, and the same real leak this closes
// that redistransport.CancelBus's own doc comment describes finding via
// pprof under load (every caller used to pass context.Background(),
// so a normally-completing run's subscription -- and, there, its
// underlying Redis connection plus two goroutines -- leaked forever).
func (c *CancelBus) SubscribeCancel(ctx context.Context, runID string) (<-chan struct{}, error) {
	ch := make(chan struct{}, 1)

	sub, err := c.nc.SubscribeSync(cancelSubject(runID))
	if err != nil {
		return nil, err
	}

	go func() {
		defer func() { _ = sub.Unsubscribe() }()
		// NextMsgWithContext returns as soon as EITHER a message
		// arrives OR ctx is done, whichever first -- no separate
		// select/inner-goroutine needed to race the two.
		if _, err := sub.NextMsgWithContext(ctx); err != nil {
			// ctx cancelled (the run completed normally) or the
			// subscription otherwise ended without a real cancel
			// message -- release without closing ch, same "real
			// cancel" vs "stopped waiting" distinction
			// redistransport's own SubscribeCancel preserves.
			return
		}
		select {
		case ch <- struct{}{}:
		default:
		}
		close(ch)
	}()

	return ch, nil
}

// FlushAll deletes every stream/KV bucket this package creates. For
// testing only -- mirrors redistransport.FlushAll's own role of giving
// each conformance test a genuinely clean slate, since (unlike Redis's
// single FLUSHALL-able keyspace) this package's state lives in several
// separately-named JetStream resources.
func FlushAll(ctx context.Context, nc *nats.Conn) error {
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	for _, name := range []string{jobsStreamName, eventsStreamName} {
		if err := js.DeleteStream(ctx, name); err != nil && !errors.Is(err, jetstream.ErrStreamNotFound) {
			return err
		}
	}
	for _, name := range []string{inflightBucket, canceledBucket} {
		if err := js.DeleteKeyValue(ctx, name); err != nil && !errors.Is(err, jetstream.ErrBucketNotFound) {
			return err
		}
	}
	return nil
}

// Compile-time interface checks.
var (
	_ transport.JobQueue     = (*Queue)(nil)
	_ transport.EventBroker  = (*Broker)(nil)
	_ transport.CancelBroker = (*CancelBus)(nil)
)
