// Package bridge implements the gRPC server that runners connect to.
// It bridges the transport layer (JobQueue + EventBroker) with the
// gRPC RunnerService protocol.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/transport"
)

// Server implements the RunnerService gRPC server.
type Server struct {
	pb.UnimplementedRunnerServiceServer
	queue              transport.JobQueue
	broker             transport.EventBroker
	cancelBus          transport.CancelBroker
	statusCallback     func(runID, status, errorMsg string)
	attemptCallback    func(context.Context, string, int64) (bool, error)
	completionCallback func(context.Context, *state.RunCompletion) (bool, error)

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

// SetRunLifecycle attaches durable attempt fencing and completion persistence.
func (s *Server) SetRunLifecycle(attempt func(context.Context, string, int64) (bool, error), complete func(context.Context, *state.RunCompletion) (bool, error)) {
	s.attemptCallback = attempt
	s.completionCallback = complete
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
	if err := requireRunnerKind(ctx, req.RunnerKind); err != nil {
		return nil, err
	}
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

	if s.attemptCallback != nil {
		accepted, err := s.attemptCallback(ctx, job.RunID, job.Generation)
		if err != nil {
			_ = s.queue.Nack(context.Background(), job.RunID)
			return nil, status.Error(codes.Unavailable, "failed to persist dispatch attempt")
		}
		if !accepted {
			_, err := s.queue.Ack(ctx, job.RunID, job.Generation)
			if err != nil {
				return nil, status.Error(codes.Unavailable, "failed to discard completed assignment")
			}
			return &pb.GetJobResponse{HasJob: false}, nil
		}
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
		if old := s.runDone[runID]; old != nil {
			old()
		}
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
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&pb.StreamEventsResponse{Ok: true})
		}
		if err != nil {
			return err
		}
		_, err = s.requireRunOwner(stream.Context(), msg.RunId)
		if err != nil {
			return err
		}
		lastRunID = msg.RunId
		current, err := s.queue.Renew(stream.Context(), msg.RunId, msg.Generation)
		if err != nil {
			return status.Error(codes.Unavailable, "failed to fence run event")
		}
		if !current {
			slog.Warn("dropping event from superseded run", "run_id", lastRunID, "generation", msg.Generation)
			continue
		}
		var event transport.RunEvent
		if err := json.Unmarshal([]byte(msg.EventJson), &event); err != nil {
			return status.Error(codes.InvalidArgument, "invalid run event JSON")
		}
		event.Generation = msg.Generation
		if event.Generation > 1 {
			event.EventID = fmt.Sprintf("%s:g%d", event.EventID, event.Generation)
		}
		if err := s.broker.Publish(stream.Context(), msg.RunId, &event); err != nil {
			slog.Error("failed to publish event", "run_id", lastRunID, "error", err)
			return status.Error(codes.Unavailable, "failed to persist run event")
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
	if !state.TerminalRunStatus(models.RunStatus(req.Status)) {
		return nil, status.Error(codes.InvalidArgument, "invalid terminal run status")
	}
	assignment, err := s.requireRunOwner(ctx, req.RunId)
	if err != nil {
		return nil, err
	}
	if assignment == nil && s.completionCallback == nil {
		return &pb.ReportStatusResponse{Ok: true, Superseded: true}, nil
	}
	if assignment != nil {
		current, err := s.queue.Renew(ctx, req.RunId, req.Generation)
		if err != nil {
			return nil, status.Error(codes.Unavailable, "failed to fence run completion")
		}
		if !current {
			return &pb.ReportStatusResponse{Ok: true, Superseded: true}, nil
		}
	}
	if s.completionCallback != nil {
		accepted, err := s.completionCallback(ctx, &state.RunCompletion{RunID: req.RunId, Generation: req.Generation, Status: models.RunStatus(req.Status), Error: req.ErrorMessage, RunnerKind: authenticatedRunner(ctx), ReplayOnly: assignment == nil})
		if err != nil {
			slog.Error("durable completion failed", "run_id", req.RunId, "error", err)
			return nil, status.Error(codes.Unavailable, "failed to persist run completion")
		}
		if !accepted {
			return &pb.ReportStatusResponse{Ok: true, Superseded: true}, nil
		}
	} else if s.statusCallback != nil {
		s.statusCallback(req.RunId, req.Status, req.ErrorMessage)
	}
	if assignment != nil {
		if _, err := s.queue.Ack(ctx, req.RunId, req.Generation); err != nil {
			return nil, status.Error(codes.Unavailable, "failed to acknowledge persisted completion")
		}
	}
	s.cleanupRun(req.RunId)
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
	if _, err := s.requireRunOwner(ctx, req.RunId); err != nil {
		return nil, err
	}
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
	if err := requireRunnerKind(stream.Context(), req.RunnerKind); err != nil {
		return err
	}
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

// requireRunnerKind binds request fields to the identity authenticated by the
// interceptor. An absent identity is the explicit local development mode.
func requireRunnerKind(ctx context.Context, kind string) error {
	if authenticated := authenticatedRunner(ctx); authenticated != "" && authenticated != kind {
		return status.Error(codes.PermissionDenied, "runner kind does not match authenticated identity")
	}
	return nil
}

func (s *Server) requireRunOwner(ctx context.Context, runID string) (*transport.RunAssignment, error) {
	assignment, err := s.queue.LookupInflight(ctx, runID)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "failed to resolve run assignment")
	}
	if assignment != nil {
		if err := requireRunnerKind(ctx, assignment.RunnerKind); err != nil {
			return nil, err
		}
	}
	return assignment, nil
}
