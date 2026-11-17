package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/finops"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

type unboundReservationStore struct {
	state.Store
	usageHoldStore
}

type unavailableReservationStore struct{ *sqlite.SQLiteStore }

func (s unavailableReservationStore) UpsertRunUsageHold(context.Context, string, *models.UsageHold) error {
	return errors.New("synthetic reservation store unavailable")
}

func TestReservationAuthorityMustSucceedBeforeAssignmentEscapes(t *testing.T) {
	for _, mode := range []string{"current", "unsupported", "unavailable", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			store, err := sqlite.New("")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			if err := store.Init(ctx); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			if err := store.CreateThread(ctx, &models.Thread{ThreadID: "thread", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "agent", Name: "Agent", Capabilities: map[string]interface{}{}}); err != nil {
				t.Fatal(err)
			}
			var backend state.Store = store
			switch mode {
			case "unsupported":
				backend = unboundReservationStore{Store: store, usageHoldStore: store}
			case "unavailable":
				backend = unavailableReservationStore{SQLiteStore: store}
			case "legacy":
				if err := store.UpsertUsageHold(ctx, &models.UsageHold{RunID: "reserved-run", TenantID: "default", AgentID: "agent", TokensHold: 37}); err != nil {
					t.Fatal(err)
				}
			}
			server := NewServer(backend, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
			server.SetFinOps(&finops.Config{Reservation: finops.ReservationConfig{TokensPerRun: 11}})
			run, assignment, createErr := server.createRunCtx(ctx, "thread", &models.RunCreate{RunID: "reserved-run", AgentID: "agent", Input: json.RawMessage(`{}`)})
			if mode == "current" {
				if createErr != nil || run == nil || assignment == nil {
					t.Fatal("matching reservation failed admission", run, assignment, createErr)
				}
			} else {
				if createErr == nil || run != nil || assignment != nil {
					t.Fatal("unconfirmed reservation escaped admission", run, assignment, createErr)
				}
				stored, readErr := store.GetRun(ctx, "reserved-run")
				if mode == "unsupported" {
					if !errors.As(readErr, new(*state.ErrNotFound)) {
						t.Fatal("unsupported store created a durable run", stored, readErr)
					}
				} else if readErr != nil || stored.Status != models.RunStatusError {
					t.Fatal("failed reservation did not close its own pending row", stored, readErr)
				}
				thread, err := store.GetThread(ctx, "thread")
				if err != nil || thread.Status != models.ThreadStatusIdle {
					t.Fatal("failed reservation stranded its thread", thread, err)
				}
			}
			_, tokens, count, err := store.SumUsageHolds(ctx, "default", "agent", now.Add(-time.Hour), now.Add(time.Hour))
			wantTokens, wantCount := int64(0), int64(0)
			switch mode {
			case "current":
				wantTokens, wantCount = 11, 1
			case "legacy":
				wantTokens, wantCount = 37, 1
			}
			if err != nil || tokens != wantTokens || count != wantCount {
				t.Fatal("reservation accounting changed unexpectedly", tokens, count, err)
			}
		})
	}
}
