package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/bridge"
	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state/sqlite"
)

type cancelFinalizationFaultStore struct {
	*sqlite.SQLiteStore
	stage string
	fail  atomic.Bool
}

func (s *cancelFinalizationFaultStore) WriteUsageEvent(ctx context.Context, event *models.UsageEvent) error {
	if s.stage == "usage" && s.fail.Load() {
		return errors.New("injected cancellation usage outage")
	}
	return s.SQLiteStore.WriteUsageEvent(ctx, event)
}

func (s *cancelFinalizationFaultStore) ReleaseUsageHold(ctx context.Context, runID string) error {
	if s.stage == "hold" && s.fail.Load() {
		return errors.New("injected cancellation hold outage")
	}
	return s.SQLiteStore.ReleaseUsageHold(ctx, runID)
}

func TestCancelFinalizationFailureRetainsLeaseAndSignalsRunner(t *testing.T) {
	for _, stage := range []string{"usage", "hold"} {
		t.Run(stage, func(t *testing.T) {
			store, q, b, c := completionFixture(t)
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
			rec := httptest.NewRecorder()
			plane.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/runs/run/cancel", nil).WithContext(ctx))
			run, err := store.GetRun(ctx, "run")
			if err != nil || run.Status != models.RunStatusInterrupted {
				t.Fatalf("durable cancellation missing: run=%+v err=%v", run, err)
			}
			inflight, err := q.LookupInflight(ctx, "run")
			if err != nil || rec.Code < 500 || inflight == nil || inflight.Generation != 1 {
				t.Fatalf("failed finalization lost recoverable lease: response=%d lease=%+v err=%v", rec.Code, inflight, err)
			}
			in, out, _, err := store.SumUsage(ctx, "default", "agent", now.Add(-time.Hour), now.Add(time.Hour))
			wantIn, wantOut := int64(0), int64(0)
			if stage == "hold" {
				wantIn, wantOut = 5, 2
			}
			if err != nil || in != wantIn || out != wantOut {
				t.Fatalf("fault did not stop the expected finalization stage: in=%d out=%d err=%v", in, out, err)
			}
			select {
			case <-cancelled:
			case <-ctx.Done():
				t.Fatal("committed cancellation did not signal the active runner")
			}

			// No caller retry: after the runner stops, ordinary reclaim/GetJob
			// finalizes the committed interruption without delivering agent work.
			fault.fail.Store(false)
			if n, dead, err := q.ReclaimStale(ctx, -time.Second, 3); err != nil || n != 1 || len(dead) != 0 {
				t.Fatal(n, dead, err)
			}
			job, err = rpc.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1})
			if err != nil || job.HasJob {
				t.Fatalf("cancelled agent was redispatched: job=%+v err=%v", job, err)
			}
			if inflight, err := q.LookupInflight(ctx, "run"); err != nil || inflight != nil {
				t.Fatalf("recovered cancellation not acknowledged: lease=%+v err=%v", inflight, err)
			}
			in, out, _, err = store.SumUsage(ctx, "default", "agent", now.Add(-time.Hour), now.Add(time.Hour))
			if err != nil || in != 5 || out != 2 {
				t.Fatalf("recovered usage was lost or billed twice: in=%d out=%d err=%v", in, out, err)
			}
			_, _, holds, err := store.SumUsageHolds(ctx, "default", "agent", now.Add(-time.Hour), now.Add(time.Hour))
			if err != nil || holds != 0 {
				t.Fatalf("recovered cancellation retained its hold: count=%d err=%v", holds, err)
			}
			thread, err := store.GetThread(ctx, "thread")
			if err != nil || thread.Status != models.ThreadStatusInterrupted || thread.Values["answer"] != nil {
				t.Fatalf("recovery committed unfinished conversation: thread=%+v err=%v", thread, err)
			}
		})
	}
}
