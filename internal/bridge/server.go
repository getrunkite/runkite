// Package bridge implements the gRPC server that runners connect to.
// It bridges the transport layer (JobQueue + EventBroker) with the
// gRPC RunnerService protocol.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/transport"
)

// Server implements the RunnerService gRPC server.
type Server struct {
	pb.UnimplementedRunnerServiceServer
	queue          transport.JobQueue
	broker         transport.EventBroker
	cancelBus      transport.CancelBroker
	statusCallback func(runID, status, errorMsg string)

	mu       sync.Mutex
	watchers map[string][]chan string      // runner_kind -> channels carrying runID
	runDone  map[string]context.CancelFunc // runID -> cancel func for the cancel-watcher goroutine
}

// NewServer creates a bridge server backed by the given queue, broker, and cancel bus.
func NewServer(queue transport.JobQueue, broker transport.EventBroker, cancelBus transport.CancelBroker, onStatus func(runID, status, errorMsg string)) *Server {
	return &Server{
		queue:          queue,
		broker:         broker,
		cancelBus:      cancelBus,
		statusCallback: onStatus,
		watchers:       make(map[string][]chan string),
		runDone:        make(map[string]context.CancelFunc),
	}
}

func (s *Server) addWatcher(kind string, ch chan string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchers[kind] = append(s.watchers[kind], ch)
}

func (s *Server) removeWatcher(kind string, ch chan string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	watchers := s.watchers[kind]
	for i, w := range watchers {
		if w == ch {
			s.watchers[kind] = append(watchers[:i], watchers[i+1:]...)
			break
		}
	}
}

func (s *Server) notifyWatchers(kind, runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.watchers[kind] {
		select {
		case ch <- runID:
		default:
		}
	}
}

// cleanupRun cancels the cancel-watcher goroutine for a completed run.
func (s *Server) cleanupRun(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel, ok := s.runDone[runID]; ok {
		cancel()
		delete(s.runDone, runID)
	}
}

// GetJob implements the long-poll pattern: blocks until a job is available.
func (s *Server) GetJob(ctx context.Context, req *pb.GetJobRequest) (*pb.GetJobResponse, error) {
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	job, err := s.queue.Dequeue(ctx, req.RunnerKind, timeout)
	if err != nil {
		return nil, err
	}
	// Queue depth metric is updated on a periodic background ticker
	// (see cmd/serve.go's pollQueueDepth), not here on every dispatch.
	// Real cost found via Redis commandstats under load: Queue.Len's
	// SCAN-based lookup is call-time proportional to the TOTAL Redis
	// keyspace size (SCAN can't skip non-matching keys cheaply), and
	// calling it on every GetJob (and every HTTP enqueue) compounded
	// that into ~200 SCAN round trips per completed run at
	// concurrency=100 -- by far the largest single cost in the whole
	// request. A once-per-few-seconds gauge update doesn't need
	// per-request freshness.
	if job == nil {
		return &pb.GetJobResponse{HasJob: false}, nil
	}

	// Client already gone while we were blocked in Dequeue — put the job
	// back so a live runner can take it (zombie GetJob recovery).
	if ctx.Err() != nil {
		_ = s.queue.Nack(context.Background(), job.RunID)
		return nil, ctx.Err()
	}

	data, err := json.Marshal(job)
	if err != nil {
		_ = s.queue.Nack(context.Background(), job.RunID)
		return nil, err
	}

	slog.Info("job dispatched to runner", "run_id", job.RunID, "graph_id", job.GraphID, "runner_kind", req.RunnerKind)

	// Subscribe to cancel SYNCHRONOUSLY before returning. CancelBroker is
	// transient pub/sub with no replay — a cancel arriving before the
	// subscription is registered is silently lost.
	runnerKind := req.RunnerKind
	runID := job.RunID

	// watchCtx governs BOTH the cancel-watcher goroutine below AND
	// (critically) the underlying subscription itself -- cleanupRun
	// cancels it once the run completes (ReportStatus), and
	// CancelBroker implementations are contractually required to
	// release their own resources when this fires (see CancelBroker's
	// doc comment). This used to pass context.Background() straight
	// into SubscribeCancel instead, which never cancels -- for the
	// Redis-backed CancelBus that leaked one Pub/Sub subscription + 2
	// goroutines PER RUN forever (confirmed via pprof: 3646 leaked
	// subscriptions after ~1800 completed runs), the actual root cause
	// of the "Redis transport gets dramatically slower under
	// concurrency" finding in bench/REPORT.md.
	watchCtx, watchCancel := context.WithCancel(context.Background())
	cancelCh, subErr := s.cancelBus.SubscribeCancel(watchCtx, runID)
	if subErr != nil {
		slog.Error("failed to subscribe to cancel for dispatched run", "run_id", runID, "error", subErr)
		watchCancel()
	} else {
		s.mu.Lock()
		s.runDone[runID] = watchCancel
		s.mu.Unlock()

		go func() {
			defer watchCancel()
			select {
			case <-cancelCh:
				slog.Info("cancel signal received for dispatched run", "run_id", runID)
				s.notifyWatchers(runnerKind, runID)
			case <-watchCtx.Done():
				// Run completed normally; goroutine exits cleanly.
			}
		}()
	}

	return &pb.GetJobResponse{
		HasJob:         true,
		AssignmentJson: string(data),
	}, nil
}

// StreamEvents receives a stream of events from the runner and publishes
// them to the event broker for SSE fan-out.
func (s *Server) StreamEvents(stream pb.RunnerService_StreamEventsServer) error {
	var lastRunID string
	seenFirstEvent := map[string]bool{}
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&pb.StreamEventsResponse{Ok: true})
		}
		if err != nil {
			slog.Error("stream recv error", "run_id", lastRunID, "error", err)
			return err
		}

		lastRunID = msg.RunId
		// First event from a runner proves delivery succeeded -- Renew
		// (not Ack) the in-flight lease so the reclaimer won't re-enqueue
		// a job that's actively running. Deliberately NOT a full Ack here
		// any more (found via a live incident): removing the job from
		// in-flight tracking entirely at this point meant NOTHING
		// watched it for the rest of its execution -- a runner crash any
		// time after its first event left the run permanently stuck,
		// confirmed live. Renew keeps it in-flight; the Heartbeat RPC
		// (called periodically by the runner for the whole run, not just
		// here once) keeps renewing it throughout execution. The true,
		// final Ack now only happens in ReportStatus below, once the run
		// actually completes.
		//
		// Not fenced on the returned `current` bool here (unlike
		// Heartbeat below) -- deliberately: this is a stream, there's no
		// per-message response to carry a "you've been superseded"
		// signal back to the runner even if we wanted to reject it here,
		// and rejecting would just mean NOT renewing, which the runner
		// can't observe anyway from inside StreamEvents. Heartbeat is
		// the channel a stale runner actually learns it's been
		// superseded through and can act on (see its own handler).
		if !seenFirstEvent[msg.RunId] {
			_, _ = s.queue.Renew(stream.Context(), msg.RunId, msg.Generation)
			seenFirstEvent[msg.RunId] = true
		}

		var event transport.RunEvent
		if err := json.Unmarshal([]byte(msg.EventJson), &event); err != nil {
			slog.Error("invalid event JSON from runner", "run_id", msg.RunId, "error", err)
			continue
		}

		// A terminal event ("end"/"error") is what callers waiting on
		// this run (the HTTP long-poll path, and any SSE subscriber)
		// treat as authoritative for the run's final outcome -- unlike
		// an ordinary progress event, publishing a stale one would
		// actively corrupt a caller's view of what happened, not just
		// look confusing. So terminal events specifically are checked
		// against the current generation before publishing (using the
		// same Renew call the rest of this stream already relies on,
		// purely for its true/false answer here -- extending the lease
		// one more time this late in a run's life is harmless): a
		// runner that was reclaimed and superseded, but finishes its
		// own now-pointless execution anyway, has its terminal event
		// silently dropped instead of being believed. The gRPC call
		// itself still succeeds either way -- there's no per-message
		// response in this streaming RPC to report a rejection through,
		// and the runner doesn't need one for this to be safe.
		if event.IsTerminal() {
			current, _ := s.queue.Renew(stream.Context(), msg.RunId, msg.Generation)
			if !current {
				slog.Warn("dropping terminal event from a superseded run", "run_id", msg.RunId, "method", event.Method, "generation", msg.Generation)
				continue
			}
			slog.Info("run terminal event", "run_id", msg.RunId, "method", event.Method)
		}

		if err := s.broker.Publish(stream.Context(), msg.RunId, &event); err != nil {
			slog.Error("failed to publish event", "run_id", msg.RunId, "error", err)
		}
	}
}

// ReportStatus receives the final status from the runner. Fenced by
// generation: a runner that was reclaimed and replaced while genuinely
// still executing (a transient blip made it miss the reaper's max-age
// window) might finish its own stale work anyway and call this late,
// after a second runner already took over and may have its own status
// pending or already reported. Accepting that stale report here --
// applying its status, or Acking a slot that no longer belongs to this
// attempt -- would let it clobber or race the real outcome. See
// RunAssignment.Generation's own doc comment for the full mechanism.
func (s *Server) ReportStatus(ctx context.Context, req *pb.ReportStatusRequest) (*pb.ReportStatusResponse, error) {
	accepted, err := s.queue.Ack(ctx, req.RunId, req.Generation)
	if err != nil {
		// A queue/transport error here is not evidence that this report
		// is stale -- treating it as "superseded" would silently drop a
		// genuinely current run's real result just because the backend
		// had a transient hiccup, which is a worse outcome than the
		// rare case a stale report slips through during that same
		// outage (reclaim itself depends on the same backend, so a
		// second runner reporting concurrently during an outage is
		// unlikely anyway). Fail toward applying the status, not toward
		// dropping it.
		slog.Error("failed to ack run status via queue, applying status anyway", "run_id", req.RunId, "error", err)
		accepted = true
	}
	if !accepted {
		slog.Warn("ignored superseded status report", "run_id", req.RunId, "status", req.Status, "generation", req.Generation)
		return &pb.ReportStatusResponse{Ok: true, Superseded: true}, nil
	}

	slog.Info("run status reported", "run_id", req.RunId, "status", req.Status)

	// Clean up the cancel-watcher goroutine for this run
	s.cleanupRun(req.RunId)

	if s.statusCallback != nil {
		s.statusCallback(req.RunId, req.Status, req.ErrorMessage)
	}

	return &pb.ReportStatusResponse{Ok: true}, nil
}

// Heartbeat is called periodically by the runner while a run is actively
// executing -- extends the job's in-flight lease so the stale-job
// reaper's "time since last touch" check reflects real liveness for the
// whole run, not just the window up to the first StreamEvents message. A runner that stops
// heartbeating (crashed, or simply doesn't implement this RPC) falls
// stale after the reaper's max-age and gets reclaimed by the existing
// mechanism -- this adds no new reclaim path, just keeps resetting the
// clock during real work. Always returns ok:true even if the run_id is
// no longer in-flight (already completed, or reclaimed away) -- a late
// heartbeat racing completion is expected, not an error condition.
//
// Fenced by generation: if a NEWER generation has already been
// dispatched (this runner was reclaimed while genuinely still
// executing, then its blip turned out to be transient), Renew returns
// current=false and this response carries Superseded=true -- this is
// the runner's OWN, actionable signal that it's lost its lease and
// should stop, delivered at the next ~2s heartbeat tick rather than
// only once it eventually finishes and calls ReportStatus.
func (s *Server) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	current, err := s.queue.Renew(ctx, req.RunId, req.Generation)
	if err != nil {
		slog.Error("failed to renew heartbeat lease", "run_id", req.RunId, "error", err)
		return &pb.HeartbeatResponse{Ok: true}, nil
	}
	if !current {
		slog.Warn("heartbeat superseded, signaling runner to stop", "run_id", req.RunId, "generation", req.Generation)
		return &pb.HeartbeatResponse{Ok: true, Superseded: true}, nil
	}
	return &pb.HeartbeatResponse{Ok: true}, nil
}

// WatchCancels streams cancel signals to the runner. The runner calls this
// once at startup. The bridge sends a CancelSignal whenever a run dispatched
// to this runner_kind is cancelled.
func (s *Server) WatchCancels(req *pb.WatchCancelsRequest, stream pb.RunnerService_WatchCancelsServer) error {
	slog.Info("runner watching for cancels", "runner_kind", req.RunnerKind)

	ch := make(chan string, 64)
	s.addWatcher(req.RunnerKind, ch)
	defer s.removeWatcher(req.RunnerKind, ch)

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case runID := <-ch:
			if err := stream.Send(&pb.CancelSignal{RunId: runID}); err != nil {
				slog.Error("failed to send cancel signal", "run_id", runID, "error", err)
				return err
			}
			slog.Info("cancel signal sent to runner", "run_id", runID, "runner_kind", req.RunnerKind)
		}
	}
}
