package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

type cronAcknowledgementQueue struct {
	*inprocess.Queue
	plane          *api.Server
	t              *testing.T
	mode, tenantID string
	effects, calls int
	cause          error
}

func (q *cronAcknowledgementQueue) Enqueue(ctx context.Context, assignment *transport.RunAssignment) error {
	q.calls++
	if tenant.IsSystem(ctx) || tenant.FromContext(ctx) != q.tenantID || assignment.TenantID != q.tenantID {
		q.t.Fatal("cron dispatch lost its exact tenant")
	}
	if q.mode == "rejected" {
		return q.cause
	}
	if err := q.Queue.Enqueue(ctx, assignment); err != nil {
		return err
	}
	if q.mode == "unknown" {
		return q.cause
	}
	job, err := q.Dequeue(ctx, assignment.RunnerKind, time.Second)
	if err != nil || job == nil || job.RunID != assignment.RunID || job.RunIncarnationID != assignment.RunIncarnationID || job.Generation != assignment.Generation {
		q.t.Fatal("fixture did not accept the scheduled assignment")
	}
	if accepted, err := q.plane.BeginRunAttempt(ctx, job.RunID, job.RunIncarnationID, job.Generation); err != nil || !accepted {
		q.t.Fatal("fixture did not admit the scheduled attempt")
	}
	q.effects++
	if q.mode == "active" {
		return q.cause
	}
	if accepted, err := q.plane.CompleteRun(ctx, &state.RunCompletion{TrustedSystem: true, ExpectedIncarnationID: job.RunIncarnationID, RunID: job.RunID, Generation: job.Generation, Status: models.RunStatusSuccess}); err != nil || !accepted {
		q.t.Fatal("fixture did not complete the scheduled attempt")
	}
	return q.cause
}

func TestCronAcknowledgementLossDoesNotRepeatCompletedFire(t *testing.T) {
	for _, profile := range []string{"default", "named", "owned_legacy"} {
		for _, expression := range []string{"* * * * *", "@every 1m"} {
			for _, mode := range []string{"completed", "active", "unknown", "rejected"} {
				t.Run(profile+"/"+expression+"/"+mode, func(t *testing.T) {
					store := newCronTestStore(t)
					tenantID := " tenant-a "
					if profile == "default" {
						tenantID = tenant.DefaultTenant
					}
					ctx := tenant.ScopedContext(context.Background(), tenantID)
					if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "agent", Name: "agent"}); err != nil {
						t.Fatal(err)
					}
					if profile == "owned_legacy" {
						now := time.Now().UTC()
						if err := store.CreateThread(ctx, &models.Thread{ThreadID: "cron:daily", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
							t.Fatal(err)
						}
					}
					queue := &cronAcknowledgementQueue{Queue: inprocess.NewQueue(), t: t, mode: mode, tenantID: tenantID, cause: fmt.Errorf("synthetic queue acknowledgement: %w", &state.ErrConflict{Resource: "queue", ID: "synthetic"})}
					var planeStore state.Store = store
					if mode == "unknown" {
						planeStore = &cronClaimCompletionFailureStore{Store: store}
					}
					queue.plane = api.NewServer(planeStore, queue, inprocess.NewBroker(), inprocess.NewCancelBus())
					schedule := &models.CronSchedule{TenantID: tenantID, Name: "daily", AgentID: "agent", Expression: expression, Enabled: true}
					c := &compiledCronSchedule{schedule: mustParse(t, expression), location: time.UTC}
					initial := time.Date(2026, 11, 17, 12, 0, 15, 0, time.UTC)
					last := initial
					var occurrence time.Time
					foreign := tenant.ScopedContext(ctx, "foreign")
					for index, now := range []time.Time{initial.Add(70 * time.Second), initial.Add(85 * time.Second)} {
						due, fire := checkSchedule(c, last, now)
						if !due {
							continue
						}
						if index == 0 {
							occurrence = fire
							if won, err := store.TryClaimCronFire(foreign, schedule.Name, fire); err != nil || !won {
								t.Fatal("foreign claim fixture failed")
							}
						}
						won, err := store.TryClaimCronFire(ctx, schedule.Name, fire)
						if err != nil {
							t.Fatal(err)
						}
						if !won || dispatchClaimedCronFire(ctx, store, cronWrappedDispatcher{queue.plane}, schedule, fire) {
							last = anchorAfterHandledFire(c, fire, now)
						}
						if index == 0 {
							want := initial
							if mode != "rejected" {
								want = anchorAfterHandledFire(c, fire, now)
							}
							if !last.Equal(want) {
								t.Fatal("delivery disposition changed the wrong calendar or interval anchor")
							}
						}
					}
					wantCalls, wantEffects := 1, 1
					if mode == "unknown" {
						wantEffects = 0
					}
					if mode == "rejected" {
						wantCalls, wantEffects = 2, 0
					}
					if queue.calls != wantCalls || queue.effects != wantEffects {
						t.Fatalf("same occurrence: calls=%d effects=%d, want %d/%d", queue.calls, queue.effects, wantCalls, wantEffects)
					}
					lastFire, found, err := store.GetLastCronFireTime(ctx, schedule.Name)
					if err != nil || found != (mode != "rejected") || (found && !lastFire.Equal(occurrence)) {
						t.Fatal("wrong fire-claim retention after delivery outcome")
					}
					if mode != "rejected" {
						// A peer retaining the old local anchor must also lose this
						// exact fire, independently of this replica's advanced anchor.
						if won, err := store.TryClaimCronFire(ctx, schedule.Name, occurrence); err != nil || won {
							t.Fatal("a peer could redispatch the retained occurrence")
						}
					}
					if _, found, err := store.GetLastCronFireTime(foreign, schedule.Name); err != nil || !found {
						t.Fatal("dispatch settlement released another tenant's claim")
					}
					runs, err := store.SearchRuns(ctx, &models.RunSearchRequest{Limit: 10})
					if err != nil || len(runs) != wantCalls {
						t.Fatal("unexpected retained scheduled run count")
					}
					for _, run := range runs {
						if profile == "named" {
							if !strings.HasPrefix(run.ThreadID, "cron-tenant:v1:") {
								t.Fatal("named dispatch lost canonical routing")
							}
						} else if run.ThreadID != "cron:daily" {
							t.Fatal("default or owned legacy routing changed")
						}
					}
				})
			}
		}
	}
}

func TestCronPrecreationAndBusyOverlapKeepTheirClaimDisposition(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprint(busy), func(t *testing.T) {
			store := newCronTestStore(t)
			ctx := tenant.ScopedContext(context.Background(), "tenant-a")
			if busy {
				if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "agent", Name: "agent"}); err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				if err := store.CreateThread(ctx, &models.Thread{ThreadID: "cron:daily", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
			}
			plane := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
			fire := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
			schedule := &models.CronSchedule{TenantID: "tenant-a", Name: "daily", AgentID: "agent"}
			if won, err := store.TryClaimCronFire(ctx, schedule.Name, fire); err != nil || !won {
				t.Fatal("fixture claim failed")
			}
			if handled := dispatchClaimedCronFire(ctx, store, plane, schedule, fire); handled != busy {
				t.Fatal("precreation retry or busy-overlap behavior changed")
			}
			if _, found, err := store.GetLastCronFireTime(ctx, schedule.Name); err != nil || found != busy {
				t.Fatal("wrong claim disposition for precreation or busy-overlap failure")
			}
		})
	}
}

type cronClaimCompletionFailureStore struct{ state.Store }

func (*cronClaimCompletionFailureStore) CompleteRun(context.Context, *state.RunCompletion) (state.CompletionResult, error) {
	return state.CompletionResult{}, errors.New("synthetic completion store unavailable")
}

type cronWrappedDispatcher struct{ cronRunDispatcher }

func (d cronWrappedDispatcher) DispatchScheduledRun(ctx context.Context, name, agent string, input, config json.RawMessage) (*models.Run, error) {
	run, err := d.cronRunDispatcher.DispatchScheduledRun(ctx, name, agent, input, config)
	if err != nil {
		return run, fmt.Errorf("synthetic outer wrapper: %w", err)
	}
	return run, nil
}
