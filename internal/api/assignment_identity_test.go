package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/bridge"
	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type retiredIdentityReadStore struct {
	state.Store
	mode string
}

func (s *retiredIdentityReadStore) GetRun(ctx context.Context, id string) (*models.Run, error) {
	if s.mode == "unavailable" {
		return nil, errors.New("synthetic unavailable authoritative read")
	}
	run, err := s.Store.GetRun(ctx, id)
	if err == nil && (s.mode == "missing_identity" || s.mode == "malformed_identity") {
		copy := *run
		copy.IncarnationID, copy.StorageIdentity = "", ""
		if s.mode == "malformed_identity" {
			copy.StorageIdentity = "invalid identity"
		}
		return &copy, nil
	}
	return run, err
}

func TestAssignmentIdentityRetiredPoisonReleasesOnlyOriginalLease(t *testing.T) {
	for _, mode := range []string{"replacement", "missing", "unavailable", "missing_identity", "malformed_identity"} {
		t.Run(mode, func(t *testing.T) {
			store, queue, broker, cancels := completionFixture(t)
			original, err := store.GetRun(t.Context(), "run")
			if err != nil {
				t.Fatal(err)
			}
			oldIdentity := state.RunIdentity(original)
			if assignment, err := queue.Dequeue(t.Context(), "kind", time.Second); err != nil || assignment == nil {
				t.Fatal("initial assignment", assignment, err)
			}
			_, dead, err := queue.ReclaimStale(t.Context(), -time.Second, 1)
			if err != nil || len(dead) != 1 {
				t.Fatal("poison selection", dead, err)
			}
			pill := dead[0]
			if err := store.DeleteRun(t.Context(), "run"); err != nil {
				t.Fatal(err)
			}
			replacement := *original
			replacement.IncarnationID, replacement.StorageIdentity = "", ""
			if mode != "missing" {
				if err := store.CreateRun(t.Context(), &replacement); err != nil {
					t.Fatal(err)
				}
			}
			plane := api.NewServer(&retiredIdentityReadStore{Store: store, mode: mode}, queue, broker, cancels)
			disposable, err := plane.CompleteReclaimedRun(t.Context(), pill, "exhausted")
			if mode == "unavailable" || mode == "missing_identity" || mode == "malformed_identity" {
				if disposable {
					t.Fatal("unknown physical record identity authorized disposal", disposable, err)
				}
				if lease, lookupErr := queue.LookupInflight(t.Context(), "run"); lookupErr != nil || lease == nil || lease.RunIncarnationID != oldIdentity {
					t.Fatal("unconfirmed retirement lost original recoverable lease", lease, lookupErr)
				}
				return
			}
			if err != nil || !disposable {
				t.Fatal("definitively retired assignment retained forever", disposable, err)
			}
			if accepted, err := queue.Ack(t.Context(), pill.RunID, pill.RunIncarnationID, pill.Generation-1); err != nil || !accepted {
				t.Fatal("retired assignment disposal", accepted, err)
			}
			if mode == "missing" {
				if err := store.CreateRun(t.Context(), &replacement); err != nil {
					t.Fatal(err)
				}
			}
			newIdentity := state.RunIdentity(&replacement)
			if err := queue.Enqueue(t.Context(), &transport.RunAssignment{RunID: "run", RunIncarnationID: newIdentity, RunnerKind: "kind", Generation: 1}); err != nil {
				t.Fatal(err)
			}
			if current, err := queue.Dequeue(t.Context(), "kind", time.Second); err != nil || current == nil || current.RunIncarnationID != newIdentity {
				t.Fatal("retired lease blocked replacement dispatch", current, err)
			}
			if accepted, err := queue.Ack(t.Context(), pill.RunID, pill.RunIncarnationID, pill.Generation-1); err != nil || accepted {
				t.Fatal("old acknowledgement disposed replacement", accepted, err)
			}
			if err := queue.Nack(t.Context(), pill.RunID, pill.RunIncarnationID, pill.Generation-1); err != nil {
				t.Fatal(err)
			}
			if disposable, err := plane.CompleteReclaimedRun(t.Context(), pill, "exhausted"); err != nil || disposable {
				t.Fatal("old poison selected replacement for disposal", disposable, err)
			}
			lease, err := queue.LookupInflight(t.Context(), "run")
			if err != nil || lease == nil || lease.RunIncarnationID != newIdentity {
				t.Fatal("old poison changed replacement lease", lease, err)
			}
			current, err := store.GetRun(t.Context(), "run")
			if err != nil || current.Status != models.RunStatusPending || len(current.Output) != 0 || current.Metadata[state.CompletionGenerationKey] != nil {
				t.Fatal("retired disposal completed replacement", current, err)
			}
		})
	}
}

type pausedReplayIdentityStore struct {
	state.Store
	paused atomic.Bool
	read   chan struct{}
	resume chan struct{}
}

func (s *pausedReplayIdentityStore) GetRun(ctx context.Context, id string) (*models.Run, error) {
	run, err := s.Store.GetRun(ctx, id)
	if err == nil && s.paused.CompareAndSwap(false, true) {
		close(s.read)
		select {
		case <-s.resume:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return run, err
}

func TestAssignmentIdentityCommittedReplayRechecksOriginalUnderStoreLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	store, queue, broker, cancels := completionFixture(t)
	original, err := store.GetRun(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	identity := state.RunIdentity(original)
	if accepted, err := store.BeginRunAttempt(t.Context(), "run", identity, 1); err != nil || !accepted {
		t.Fatal(accepted, err)
	}
	if result, err := store.CompleteRun(t.Context(), &state.RunCompletion{RunID: "run", ExpectedIncarnationID: identity, Generation: 1, Status: models.RunStatusSuccess}); err != nil || !result.Applied {
		t.Fatal(result, err)
	}
	paused := &pausedReplayIdentityStore{Store: store, read: make(chan struct{}), resume: make(chan struct{})}
	plane := api.NewServer(paused, queue, broker, cancels)
	type replayResult struct {
		accepted bool
		err      error
	}
	result := make(chan replayResult, 1)
	go func() {
		accepted, err := plane.CompleteRun(ctx, &state.RunCompletion{RunID: "run", ExpectedIncarnationID: identity, Generation: 1, Status: models.RunStatusSuccess, ReplayOnly: true})
		result <- replayResult{accepted, err}
	}()
	select {
	case <-paused.read:
	case <-ctx.Done():
		t.Fatal("replay did not capture its original record")
	}
	if err := store.DeleteRun(t.Context(), "run"); err != nil {
		close(paused.resume)
		t.Fatal(err)
	}
	replacement := *original
	replacement.IncarnationID, replacement.StorageIdentity = "", ""
	if err := store.CreateRun(t.Context(), &replacement); err != nil {
		close(paused.resume)
		t.Fatal(err)
	}
	close(paused.resume)
	select {
	case replay := <-result:
		if replay.err == nil || replay.accepted {
			t.Fatal("old committed replay crossed physical record boundary", replay)
		}
	case <-ctx.Done():
		t.Fatal("replay did not finish after replacement")
	}
	current, err := store.GetRun(t.Context(), "run")
	if err != nil || state.RunIdentity(current) != state.RunIdentity(&replacement) || current.Status != models.RunStatusPending {
		t.Fatal("stale replay changed replacement", current, err)
	}
}

type identityRenewBoundaryQueue struct {
	*inprocess.Queue
	afterRenew func()
}

func (q *identityRenewBoundaryQueue) Renew(ctx context.Context, id, incarnation string, generation int64) (bool, error) {
	matched, err := q.Queue.Renew(ctx, id, incarnation, generation)
	if matched && err == nil && q.afterRenew != nil {
		after := q.afterRenew
		q.afterRenew = nil
		after()
	}
	return matched, err
}

func TestAssignmentIdentitySurvivesRunReuseAndPublicationRaces(t *testing.T) {
	for _, boundary := range []string{"fresh_old_messages", "after_event_renew", "after_status_renew", "before_dispatch"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			store, queue, broker, cancels := completionFixture(t)
			wrapped := &identityRenewBoundaryQueue{Queue: queue}
			plane := api.NewServer(store, wrapped, broker, cancels)
			rpc := bridge.NewServer(wrapped, broker, cancels, nil)
			rpc.SetRunLifecycle(plane.BeginRunAttempt, plane.CompleteRun)
			original, err := store.GetRun(ctx, "run")
			if err != nil {
				t.Fatal(err)
			}
			oldID := state.RunIdentity(original)
			getJob := func() *pb.GetJobResponse {
				t.Helper()
				response, err := rpc.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1, AssignmentIdentityVersion: 1})
				if err != nil {
					t.Fatal(err)
				}
				return response
			}
			if !getJob().HasJob {
				t.Fatal("original dispatch unavailable")
			}
			var newID string
			var newEvents <-chan *transport.RunEvent
			replace := func(deliver bool) {
				t.Helper()
				accepted, err := plane.CompleteRun(ctx, &state.RunCompletion{TrustedSystem: true, ExpectedIncarnationID: oldID, RunID: "run", Generation: 1, Status: models.RunStatusSuccess})
				if err != nil || !accepted {
					t.Fatal("original completion failed", accepted, err)
				}
				if _, err := queue.Ack(ctx, "run", oldID, 1); err != nil {
					t.Fatal(err)
				}
				if err := store.DeleteRun(ctx, "run"); err != nil {
					t.Fatal(err)
				}
				copy := *original
				copy.IncarnationID, copy.StorageIdentity, copy.Metadata = "", "", map[string]interface{}{}
				if err := store.CreateRun(ctx, &copy); err != nil {
					t.Fatal(err)
				}
				newID = state.RunIdentity(&copy)
				if newID == "" || newID == oldID {
					t.Fatal("replacement identity did not change")
				}
				if err := store.SetThreadStatus(ctx, "thread", models.ThreadStatusBusy); err != nil {
					t.Fatal(err)
				}
				if deliver {
					if err := queue.Enqueue(ctx, &transport.RunAssignment{RunID: "run", RunIncarnationID: newID, ThreadID: "thread", GraphID: "agent", RunnerKind: "kind", Generation: 1}); err != nil {
						t.Fatal(err)
					}
					if !getJob().HasJob {
						t.Fatal("replacement dispatch unavailable")
					}
				}
				newEvents, err = broker.Subscribe(ctx, "run", newID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if boundary == "before_dispatch" {
				replace(false)
				if err := queue.Enqueue(ctx, &transport.RunAssignment{RunID: "run", RunIncarnationID: oldID, ThreadID: "thread", GraphID: "agent", RunnerKind: "kind", Generation: 1}); err != nil {
					t.Fatal(err)
				}
				if getJob().HasJob {
					t.Fatal("old queued work was handed to an agent")
				}
				current, err := store.GetRun(ctx, "run")
				if err != nil || current.Status != models.RunStatusPending || current.Metadata[state.AttemptGenerationKey] != nil {
					t.Fatal("old queued work advanced its replacement", current, err)
				}
				return
			}
			if boundary == "fresh_old_messages" {
				replace(true)
				hb, err := rpc.Heartbeat(ctx, &pb.HeartbeatRequest{RunId: "run", RunIncarnationId: oldID, Generation: 1})
				if err != nil || !hb.Superseded {
					t.Fatal("old heartbeat borrowed replacement", hb, err)
				}
				report, err := rpc.ReportStatus(ctx, &pb.ReportStatusRequest{RunId: "run", RunIncarnationId: oldID, Generation: 1, Status: "success"})
				if err != nil || !report.Superseded {
					t.Fatal("old completion borrowed replacement", report, err)
				}
				request := httptest.NewRequest(http.MethodPut, "/internal/checkpoints/thread/adapter-state", bytes.NewBufferString("stale"))
				request.Header.Set(auth.HeaderRunID, "run")
				request.Header.Set(auth.HeaderGeneration, "1")
				request.Header.Set(auth.HeaderRunIncarnation, oldID)
				response := httptest.NewRecorder()
				auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: queue}, plane.Handler()).ServeHTTP(response, request)
				if response.Code != http.StatusForbidden {
					t.Fatal("old HTTP proof borrowed replacement", response.Code, response.Body)
				}
			} else {
				wrapped.afterRenew = func() { replace(true) }
			}
			if boundary == "after_status_renew" {
				if _, err := rpc.ReportStatus(ctx, &pb.ReportStatusRequest{RunId: "run", RunIncarnationId: oldID, Generation: 1, Status: "success"}); err == nil {
					t.Fatal("completion admitted before replacement crossed its database fence")
				}
			} else {
				stream := &completionStream{msgs: []*pb.RunEventProto{
					{RunId: "run", RunIncarnationId: oldID, Generation: 1, EventJson: `{"event_id":"old-values","seq":1,"method":"values","data":{"answer":"old"}}`},
					{RunId: "run", RunIncarnationId: oldID, Generation: 1, EventJson: `{"event_id":"old-end","seq":2,"method":"end","data":{"status":"success"}}`},
				}}
				if err := rpc.StreamEvents(stream); err != nil {
					t.Fatal(err)
				}
			}
			current, err := store.GetRun(ctx, "run")
			if err != nil || state.RunIdentity(current) != newID || current.Status != models.RunStatusRunning || len(current.Output) != 0 {
				t.Fatal("old work changed replacement", current, err)
			}
			live, err := queue.LookupInflight(ctx, "run")
			if err != nil || live == nil || live.RunIncarnationID != newID {
				t.Fatal("old work acknowledged replacement", live, err)
			}
			past, err := broker.Replay(ctx, "run", newID, 0)
			if err != nil || len(past) != 0 {
				t.Fatal("old work entered replacement replay", past, err)
			}
			select {
			case _, open := <-newEvents:
				if !open {
					t.Fatal("old terminal closed replacement stream")
				}
				t.Fatal("replacement stream received old work")
			default:
			}
			if accepted, err := plane.CompleteRun(ctx, &state.RunCompletion{ExpectedIncarnationID: oldID, RunID: "run", Generation: 1, Status: models.RunStatusSuccess, ReplayOnly: true}); err == nil || accepted {
				t.Fatal("completed-result replay accepted another record", accepted, err)
			}
		})
	}
}

func TestAssignmentIdentityCapabilityRequiredBeforeDequeue(t *testing.T) {
	store, queue, broker, cancels := completionFixture(t)
	plane := api.NewServer(store, queue, broker, cancels)
	rpc := bridge.NewServer(queue, broker, cancels, nil)
	rpc.SetRunLifecycle(plane.BeginRunAttempt, plane.CompleteRun)
	for _, version := range []uint32{0, 2} {
		if _, err := rpc.GetJob(t.Context(), &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1, AssignmentIdentityVersion: version}); status.Code(err) != codes.FailedPrecondition {
			t.Fatal("unsupported identity protocol accepted", version, err)
		}
	}
	if ready, err := queue.Len(t.Context()); err != nil || ready != 1 {
		t.Fatal("unsupported client consumed a queued assignment", ready, err)
	}
}

func TestRunStatusRequiresOriginalRecordAfterQueueLeaseIsGone(t *testing.T) {
	store, queue, broker, cancels := completionFixture(t)
	plane := api.NewServer(store, queue, broker, cancels)
	run, err := store.GetRun(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	identity := state.RunIdentity(run)
	if accepted, err := store.BeginRunAttempt(t.Context(), "run", identity, 1); err != nil || !accepted {
		t.Fatal(accepted, err)
	}
	if accepted, err := plane.CompleteRun(t.Context(), &state.RunCompletion{TrustedSystem: true, ExpectedIncarnationID: identity, RunID: "run", Generation: 1, Status: models.RunStatusSuccess}); err != nil || !accepted {
		t.Fatal(accepted, err)
	}
	for _, proof := range []string{"", "other-record", identity} {
		request := httptest.NewRequest(http.MethodGet, "/internal/runs/run/status", nil)
		request.Header.Set(auth.HeaderRunIncarnation, proof)
		request.Header.Set(auth.HeaderGeneration, "1")
		response := httptest.NewRecorder()
		plane.Handler().ServeHTTP(response, request)
		if proof != identity {
			if response.Code != http.StatusUnauthorized && response.Code != http.StatusForbidden {
				t.Fatal("unproven terminal read accepted", response.Code, response.Body)
			}
			continue
		}
		var value map[string]string
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &value) != nil || value["status"] != "success" {
			t.Fatal("matched terminal read rejected", response.Code, response.Body)
		}
	}
}
