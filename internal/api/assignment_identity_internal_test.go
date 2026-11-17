package api

import (
	"context"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestAssignmentIdentityAdminReplayNarrowsToCapturedTenant(t *testing.T) {
	store, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ctx := tenant.ScopedContext(t.Context(), "tenant-a")
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: "thread", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	run := &models.Run{RunID: "run", ThreadID: "thread", AgentID: "agent", Status: models.RunStatusSuccess, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	plane := NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	deadline, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var terminal []*transport.RunEvent
	if err := plane.replayRunEvents(tenant.SystemContext(deadline), run.RunID, state.RunIdentity(run), nil, nil, func(event *transport.RunEvent) error {
		terminal = append(terminal, event)
		return nil
	}); err != nil {
		t.Fatal("authorized Admin replay failed to finish in captured tenant", err)
	}
	if len(terminal) != 1 || !terminal[0].IsTerminal() || terminal[0].RunIncarnationID != state.RunIdentity(run) {
		t.Fatal("durable terminal lost original identity", terminal)
	}
	if err := plane.replayRunEvents(tenant.ScopedContext(deadline, "tenant-b"), run.RunID, state.RunIdentity(run), nil, nil, func(*transport.RunEvent) error {
		t.Fatal("client crossed tenant boundary using a record proof")
		return nil
	}); err == nil {
		t.Fatal("foreign client reached Admin scope narrowing")
	}
}
