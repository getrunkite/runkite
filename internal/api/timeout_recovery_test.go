package api_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/bridge"
	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/metrics"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	dto "github.com/prometheus/client_model/go"
)

type unavailableTimeoutReleaseStore struct{ *sqlite.SQLiteStore }

func (s unavailableTimeoutReleaseStore) ReleaseThreadIfNoOtherActive(context.Context, string, string, models.ThreadStatus) (bool, error) {
	return false, errors.New("injected timeout thread-release outage")
}

type timeoutSnapshotBarrierStore struct {
	*sqlite.SQLiteStore
	reads atomic.Int32
	ready chan struct{}
}

func (s *timeoutSnapshotBarrierStore) ListActiveRunsCreatedBefore(ctx context.Context, before time.Time, limit int) ([]*models.Run, error) {
	runs, err := s.SQLiteStore.ListActiveRunsCreatedBefore(ctx, before, limit)
	if err != nil {
		return nil, err
	}
	if s.reads.Add(1) == 2 {
		close(s.ready)
	}
	select {
	case <-s.ready:
		return runs, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestConcurrentTimeoutSweepsCountAndMeterOneWinner(t *testing.T) {
	store, q, b, c := completionFixture(t)
	ageTimeoutRun(t, store)
	barrier := &timeoutSnapshotBarrierStore{SQLiteStore: store, ready: make(chan struct{})}
	first := api.NewServer(barrier, q, b, c)
	second := api.NewServer(barrier, q, b, c)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	count := func() float64 {
		var value dto.Metric
		if err := metrics.RunsTotal.WithLabelValues("agent", string(models.RunStatusTimeout)).Write(&value); err != nil {
			t.Fatal(err)
		}
		return value.GetCounter().GetValue()
	}
	before := count()
	type result struct {
		count int
		err   error
	}
	done := make(chan result, 2)
	for _, plane := range []*api.Server{first, second} {
		go func() {
			n, err := plane.TimeoutOverdueRuns(ctx, time.Hour, 100)
			done <- result{n, err}
		}()
	}
	total := 0
	for range 2 {
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatal(r.err)
			}
			total += r.count
		case <-ctx.Done():
			t.Fatal("concurrent timeout sweeps did not return")
		}
	}
	after := count()
	if total != 1 || after-before != 1 {
		t.Fatalf("duplicate timeout winner: count=%d metric delta=%v", total, after-before)
	}
}

func TestTimeoutFencesReclaimAndPreservesTerminalWinner(t *testing.T) {
	for _, terminalWinner := range []bool{false, true} {
		name := "new_attempt"
		if terminalWinner {
			name = "terminal_winner"
		}
		t.Run(name, func(t *testing.T) {
			store, q, b, c := completionFixture(t)
			ageTimeoutRun(t, store)
			fault := &pausedCancelCommitStore{SQLiteStore: store, ready: make(chan struct{}), resume: make(chan struct{})}
			plane := api.NewServer(fault, q, b, c)
			rpc := bridge.NewServer(q, b, c, nil)
			rpc.SetRunLifecycle(plane.BeginRunAttempt, plane.CompleteRun)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if job, err := rpc.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1}); err != nil || !job.HasJob {
				t.Fatal(job, err)
			}
			publishReviewEvent(t, b, 1, 1, "values", `{"answer":"unfinished","usage":{"prompt_tokens":999}}`)
			type result struct {
				count int
				err   error
			}
			done := make(chan result, 1)
			go func() {
				n, err := plane.TimeoutOverdueRuns(ctx, time.Hour, 100)
				done <- result{n, err}
			}()
			select {
			case <-fault.ready:
			case <-ctx.Done():
				t.Fatal("timeout did not capture its current attempt")
			}
			if terminalWinner {
				if result, err := store.CompleteRun(ctx, &state.RunCompletion{RunID: "run", Generation: 1, Status: models.RunStatusSuccess, Values: map[string]interface{}{"answer": "winner"}}); err != nil || !result.Applied {
					close(fault.resume)
					t.Fatal(result, err)
				}
			} else if ok, err := store.BeginRunAttempt(ctx, "run", 2); err != nil || !ok {
				close(fault.resume)
				t.Fatal(ok, err)
			}
			close(fault.resume)
			r := <-done
			if r.err != nil {
				t.Fatal(r.err)
			}
			run, err := store.GetRun(ctx, "run")
			if err != nil {
				t.Fatal(err)
			}
			thread, err := store.GetThread(ctx, "thread")
			if err != nil {
				t.Fatal(err)
			}
			if terminalWinner {
				if r.count != 0 || run.Status != models.RunStatusSuccess || thread.Values["answer"] != "winner" {
					t.Fatalf("timeout overwrote terminal winner: count=%d run=%+v thread=%+v", r.count, run, thread)
				}
				if inflight, err := q.LookupInflight(ctx, "run"); err != nil || inflight == nil {
					t.Fatalf("losing timeout removed the winner's queue lease: lease=%+v err=%v", inflight, err)
				}
			} else if r.count != 1 || run.Status != models.RunStatusTimeout || state.MetadataGeneration(run.Metadata, state.CompletionGenerationKey) != 2 || len(run.Output) != 0 || thread.Values["answer"] != nil {
				t.Fatalf("timeout borrowed superseded usage or history: count=%d run=%+v thread=%+v", r.count, run, thread)
			}
		})
	}
}

func ageTimeoutRun(t *testing.T, store *sqlite.SQLiteStore) {
	t.Helper()
	ctx := context.Background()
	if err := store.DeleteRun(ctx, "run"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-2 * time.Hour)
	if err := store.CreateRun(ctx, &models.Run{
		RunID: "run", ThreadID: "thread", AgentID: "agent", Status: models.RunStatusPending,
		Metadata: map[string]interface{}{}, CreatedAt: old, UpdatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTimeoutCompletesAndReleasesThreadAtomically(t *testing.T) {
	store, q, b, c := completionFixture(t)
	ageTimeoutRun(t, store)
	plane := api.NewServer(unavailableTimeoutReleaseStore{store}, q, b, c)
	ctx := context.Background()
	n, err := plane.TimeoutOverdueRuns(ctx, time.Hour, 100)
	if err != nil || n != 1 {
		t.Fatalf("timeout result: count=%d err=%v", n, err)
	}
	run, err := store.GetRun(ctx, "run")
	if err != nil || run.Status != models.RunStatusTimeout {
		t.Fatalf("timeout did not commit: run=%+v err=%v", run, err)
	}
	thread, err := store.GetThread(ctx, "thread")
	if err != nil || thread.Status != models.ThreadStatusIdle {
		t.Fatalf("timeout stranded a busy thread after its terminal write: thread=%+v err=%v", thread, err)
	}
	if _, ok := run.Metadata[state.CompletionGenerationKey]; !ok {
		t.Fatal("timeout bypassed durable completion")
	}
}

func TestTimeoutFinalizationFailureRetainsLeaseAndSignalsRunner(t *testing.T) {
	for _, stage := range []string{"usage", "hold"} {
		t.Run(stage, func(t *testing.T) {
			store, q, b, c := completionFixture(t)
			ageTimeoutRun(t, store)
			fault := &cancelFinalizationFaultStore{SQLiteStore: store, stage: stage}
			fault.fail.Store(true)
			plane := api.NewServer(fault, q, b, c)
			rpc := bridge.NewServer(q, b, c, nil)
			rpc.SetRunLifecycle(plane.BeginRunAttempt, plane.CompleteRun)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			job, err := rpc.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1})
			if err != nil || !job.HasJob {
				t.Fatal(job, err)
			}
			cancelled, err := c.SubscribeCancel(ctx, "run")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			if err := store.UpsertUsageHold(ctx, &models.UsageHold{
				RunID: "run", TenantID: "default", AgentID: "agent", TokensHold: 7, CreatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			publishReviewEvent(t, b, 1, 1, "values", `{"answer":"unfinished","usage":{"prompt_tokens":5,"completion_tokens":2}}`)
			n, timeoutErr := plane.TimeoutOverdueRuns(ctx, time.Hour, 100)
			run, err := store.GetRun(ctx, "run")
			if err != nil || run.Status != models.RunStatusTimeout {
				t.Fatalf("durable timeout missing: run=%+v err=%v", run, err)
			}
			inflight, err := q.LookupInflight(ctx, "run")
			if err != nil || n != 1 || timeoutErr == nil || inflight == nil || inflight.Generation != 1 {
				t.Fatalf("failed timeout finalization lost recoverable lease: count=%d timeoutErr=%v lease=%+v err=%v", n, timeoutErr, inflight, err)
			}
			select {
			case <-cancelled:
			case <-ctx.Done():
				t.Fatal("committed timeout did not signal the active runner")
			}
			fault.fail.Store(false)
			if n, dead, err := q.ReclaimStale(ctx, -time.Second, 3); err != nil || n != 1 || len(dead) != 0 {
				t.Fatal(n, dead, err)
			}
			job, err = rpc.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1})
			if err != nil || job.HasJob {
				t.Fatalf("timed-out agent was redispatched: job=%+v err=%v", job, err)
			}
			if inflight, err := q.LookupInflight(ctx, "run"); err != nil || inflight != nil {
				t.Fatalf("recovered timeout not acknowledged: lease=%+v err=%v", inflight, err)
			}
			in, out, _, err := store.SumUsage(ctx, "default", "agent", now.Add(-time.Hour), now.Add(time.Hour))
			if err != nil || in != 5 || out != 2 {
				t.Fatalf("timeout usage was lost or billed twice: in=%d out=%d err=%v", in, out, err)
			}
			_, _, holds, err := store.SumUsageHolds(ctx, "default", "agent", now.Add(-time.Hour), now.Add(time.Hour))
			if err != nil || holds != 0 {
				t.Fatalf("recovered timeout retained its hold: count=%d err=%v", holds, err)
			}
			thread, err := store.GetThread(ctx, "thread")
			if err != nil || thread.Status != models.ThreadStatusIdle || thread.Values["answer"] != nil {
				t.Fatalf("timeout committed unfinished conversation: thread=%+v err=%v", thread, err)
			}
		})
	}
}
