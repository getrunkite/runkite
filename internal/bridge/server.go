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

	pb "github.com/runkite/runkite/internal/bridge/runnerpb"
	"github.com/runkite/runkite/internal/transport"
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
	acked := map[string]bool{}
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
		// First event from a runner proves delivery succeeded — Ack so the
		// reclaimer won't re-enqueue a job that's actively running.
		if !acked[msg.RunId] {
			_ = s.queue.Ack(stream.Context(), msg.RunId)
			acked[msg.RunId] = true
		}

		var event transport.RunEvent
		if err := json.Unmarshal([]byte(msg.EventJson), &event); err != nil {
			slog.Error("invalid event JSON from runner", "run_id", msg.RunId, "error", err)
			continue
		}

		if err := s.broker.Publish(stream.Context(), msg.RunId, &event); err != nil {
			slog.Error("failed to publish event", "run_id", msg.RunId, "error", err)
		}

		if event.IsTerminal() {
			slog.Info("run terminal event", "run_id", msg.RunId, "method", event.Method)
		}
	}
}

// ReportStatus receives the final status from the runner.
func (s *Server) ReportStatus(ctx context.Context, req *pb.ReportStatusRequest) (*pb.ReportStatusResponse, error) {
	slog.Info("run status reported", "run_id", req.RunId, "status", req.Status)

	_ = s.queue.Ack(ctx, req.RunId)

	// Clean up the cancel-watcher goroutine for this run
	s.cleanupRun(req.RunId)

	if s.statusCallback != nil {
		s.statusCallback(req.RunId, req.Status, req.ErrorMessage)
	}

	return &pb.ReportStatusResponse{Ok: true}, nil
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
