package conformance

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func runRecordTerminalHookTests(t *testing.T, factory StoreFactory) {
	seed := func(t *testing.T, status models.RunStatus) (state.Store, state.RunTerminalHookStore, context.Context, *models.Run) {
		t.Helper()
		store := factory(t)
		claims, ok := store.(state.RunTerminalHookStore)
		if !ok {
			t.Fatal("built-in store lacks record-bound terminal hook claims")
		}
		ctx := tenant.ScopedContext(t.Context(), "hook-owner")
		now := time.Now().UTC()
		if err := store.CreateThread(ctx, &models.Thread{ThreadID: "hook-thread", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		run := &models.Run{RunID: "hook-run", ThreadID: "hook-thread", AgentID: "agent", Status: status, CreatedAt: now, UpdatedAt: now}
		if err := store.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		if state.RunIdentity(run) == "" {
			t.Fatal("created run lacks immutable identity")
		}
		return store, claims, ctx, run
	}
	wantConflict := func(t *testing.T, won bool, err error) {
		t.Helper()
		var conflict *state.ErrConflict
		if won || !errors.As(err, &conflict) {
			t.Fatalf("expected fail-closed conflict, won=%v error=%v", won, err)
		}
	}
	t.Run("matching_terminal_claim_has_one_concurrent_winner", func(t *testing.T) {
		_, claims, ctx, run := seed(t, models.RunStatusSuccess)
		var winners atomic.Int64
		var group sync.WaitGroup
		start := make(chan struct{})
		for range 12 {
			group.Go(func() {
				<-start
				won, err := claims.TryClaimRunTerminalHook(ctx, run.RunID, state.RunIdentity(run))
				if err != nil {
					t.Errorf("claim: %v", err)
				}
				if won {
					winners.Add(1)
				}
			})
		}
		close(start)
		group.Wait()
		if winners.Load() != 1 {
			t.Fatalf("expected one original-record winner, got %d", winners.Load())
		}
	})
	for _, oldClaimed := range []bool{false, true} {
		name := "old_unclaimed"
		if oldClaimed {
			name = "old_claim_retained"
		}
		t.Run("same_id_recreation/"+name, func(t *testing.T) {
			store, claims, ctx, old := seed(t, models.RunStatusSuccess)
			oldIdentity := state.RunIdentity(old)
			if oldClaimed {
				if won, err := claims.TryClaimRunTerminalHook(ctx, old.RunID, oldIdentity); err != nil || !won {
					t.Fatal(won, err)
				}
			}
			if err := store.DeleteRun(ctx, old.RunID); err != nil {
				t.Fatal(err)
			}
			replacement := *old
			replacement.IncarnationID, replacement.StorageIdentity = "", ""
			if err := store.CreateRun(ctx, &replacement); err != nil {
				t.Fatal(err)
			}
			identity := state.RunIdentity(&replacement)
			if identity == "" || identity == oldIdentity {
				t.Fatal("replacement did not acquire a distinct original identity")
			}
			won, err := claims.TryClaimRunTerminalHook(ctx, old.RunID, oldIdentity)
			wantConflict(t, won, err)
			if won, err := claims.TryClaimRunTerminalHook(ctx, replacement.RunID, identity); err != nil || !won {
				t.Fatalf("old authority/claim suppressed replacement: won=%v error=%v", won, err)
			}
			if won, err := claims.TryClaimRunTerminalHook(ctx, replacement.RunID, identity); err != nil || won {
				t.Fatalf("replacement claim replay was not idempotent: won=%v error=%v", won, err)
			}
		})
	}
	t.Run("legacy_bare_claim_is_ambiguous", func(t *testing.T) {
		store, claims, ctx, run := seed(t, models.RunStatusSuccess)
		if won, err := store.TryClaimTerminalHook(ctx, run.RunID); err != nil || !won {
			t.Fatal(won, err)
		}
		won, err := claims.TryClaimRunTerminalHook(ctx, run.RunID, state.RunIdentity(run))
		wantConflict(t, won, err)
		if !strings.Contains(err.Error(), "fresh operation IDs") {
			t.Fatalf("legacy refusal lacks repair guidance: %v", err)
		}
	})
	t.Run("missing_foreign_nonterminal_and_missing_proof_fail_closed", func(t *testing.T) {
		store, claims, ctx, run := seed(t, models.RunStatusRunning)
		identity := state.RunIdentity(run)
		won, err := claims.TryClaimRunTerminalHook(ctx, run.RunID, identity)
		wantConflict(t, won, err)
		won, err = claims.TryClaimRunTerminalHook(ctx, run.RunID, "")
		wantConflict(t, won, err)
		for _, request := range []struct {
			ctx context.Context
			id  string
		}{{tenant.ScopedContext(ctx, "foreign"), run.RunID}, {ctx, "missing"}} {
			won, err := claims.TryClaimRunTerminalHook(request.ctx, request.id, identity)
			var missing *state.ErrNotFound
			if won || !errors.As(err, &missing) {
				t.Fatalf("missing/foreign run accepted: won=%v error=%v", won, err)
			}
		}
		if err := store.UpdateRunStatus(ctx, run.RunID, models.RunStatusSuccess, nil, ""); err != nil {
			t.Fatal(err)
		}
		if won, err := claims.TryClaimRunTerminalHook(tenant.SystemContext(ctx), run.RunID, identity); err != nil || !won {
			t.Fatalf("failed calls consumed current terminal claim: won=%v error=%v", won, err)
		}
	})
}
