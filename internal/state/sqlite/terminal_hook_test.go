package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
)

type pausedTerminalHookTx struct {
	sqlcompletion.Tx
	entered chan struct{}
	release chan struct{}
}

func (tx pausedTerminalHookTx) Exec(ctx context.Context, query string, args ...interface{}) error {
	if strings.HasPrefix(query, "INSERT INTO terminal_hook_claims") {
		close(tx.entered)
		select {
		case <-tx.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return tx.Tx.Exec(ctx, query, args...)
}

func TestTerminalHookClaimRetainsRunLockThroughInsertion(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	store, err := New(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	// Use a second real connection so a single-connection pool cannot hide a
	// missing database lock between original-record validation and insertion.
	store.db.SetMaxOpenConns(2)
	second, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: "thread", Status: models.ThreadStatusIdle}); err != nil {
		t.Fatal(err)
	}
	run := &models.Run{RunID: "run", ThreadID: "thread", AgentID: "agent", Status: models.RunStatusSuccess}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	driver := store.completionDriver()
	begin := driver.Begin
	driver.Begin = func(ctx context.Context) (sqlcompletion.Tx, error) {
		tx, err := begin(ctx)
		if err != nil {
			return nil, err
		}
		return pausedTerminalHookTx{Tx: tx, entered: entered, release: release}, nil
	}
	type claimResult struct {
		won bool
		err error
	}
	claimed := make(chan claimResult, 1)
	go func() {
		won, err := driver.TryClaimRunTerminalHook(ctx, run.RunID, state.RunIdentity(run))
		claimed <- claimResult{won, err}
	}()
	select {
	case <-entered:
	case result := <-claimed:
		t.Fatalf("claim did not reach insertion: %+v", result)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	started, deleted := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		_, err := second.ExecContext(ctx, `DELETE FROM runs WHERE run_id = ?`, run.RunID)
		deleted <- err
	}()
	<-started
	select {
	case err := <-deleted:
		t.Fatalf("deletion escaped the terminal-claim transaction: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	select {
	case result := <-claimed:
		if result.err != nil || !result.won {
			t.Fatalf("original claim failed: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	replacement := *run
	replacement.IncarnationID = ""
	if err := store.CreateRun(ctx, &replacement); err != nil {
		t.Fatal(err)
	}
	if won, err := store.TryClaimRunTerminalHook(ctx, run.RunID, state.RunIdentity(&replacement)); err != nil || !won {
		t.Fatalf("original claim poisoned replacement: won=%v error=%v", won, err)
	}
}
