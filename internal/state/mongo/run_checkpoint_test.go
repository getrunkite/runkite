package mongo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type checkpointMonitorContextKey struct{}

// Pause a checkpoint transaction immediately after its run read. The newer run
// transaction then commits before the old checkpoint transaction resumes. A
// read-only run check would accept its stale snapshot and mutate the blob; the
// run revision write must instead force a retry and reject the old binding.
func TestBoundCheckpointRejectsRunChangedAfterSnapshotRead(t *testing.T) {
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		t.Skip("MONGO_URI not set; Mongo replica-set checkpoint race test requires it")
	}
	for _, mutation := range []string{"put", "delete"} {
		for _, change := range []string{"generation", "terminal"} {
			t.Run(mutation+"_"+change, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				read := make(chan struct{})
				release := make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				defer unblock()
				var paused atomic.Bool
				monitor := &event.CommandMonitor{Succeeded: func(commandCtx context.Context, evt *event.CommandSucceededEvent) {
					if commandCtx.Value(checkpointMonitorContextKey{}) != true || evt.CommandName != "find" || !paused.CompareAndSwap(false, true) {
						return
					}
					close(read)
					select {
					case <-release:
					case <-ctx.Done():
					}
				}}
				client, err := mongo.Connect(options.Client().ApplyURI(uri).
					SetBSONOptions(&options.BSONOptions{DefaultDocumentM: true}).SetMonitor(monitor))
				if err != nil {
					t.Fatal(err)
				}
				db := client.Database(fmt.Sprintf("runkite_checkpoint_fence_%d", time.Now().UnixNano()))
				store := &Store{client: client, db: db}
				defer func() {
					unblock()
					cancel()
					cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cleanupCancel()
					_ = db.Drop(cleanupCtx)
					_ = client.Disconnect(cleanupCtx)
				}()
				if err := store.Init(ctx); err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				if err := store.CreateThread(ctx, &models.Thread{ThreadID: "thread", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
				if err := store.CreateRun(ctx, &models.Run{RunID: "run", ThreadID: "thread", AgentID: "agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
				if accepted, err := store.BeginRunAttempt(ctx, "run", 1); err != nil || !accepted {
					t.Fatalf("begin first attempt: accepted=%t err=%v", accepted, err)
				}
				if _, err := store.PutOpaqueCheckpoint(ctx, "thread", "state", []byte("committed history"), "adapter", nil); err != nil {
					t.Fatal(err)
				}
				binding := state.RunCheckpointBinding{RunID: "run", Generation: 1, ThreadID: "thread"}
				result := make(chan error, 1)
				go func() {
					oldCtx := context.WithValue(ctx, checkpointMonitorContextKey{}, true)
					if mutation == "put" {
						_, err := store.PutBoundOpaqueCheckpoint(oldCtx, binding, "state", []byte("superseded history"), "adapter", nil)
						result <- err
					} else {
						result <- store.DeleteBoundOpaqueCheckpoint(oldCtx, binding, "state")
					}
				}()
				select {
				case <-read:
				case <-ctx.Done():
					t.Fatal("checkpoint transaction did not reach its run read")
				}
				wantReason := "run_generation_mismatch"
				if change == "generation" {
					if accepted, err := store.BeginRunAttempt(ctx, "run", 2); err != nil || !accepted {
						t.Fatalf("begin replacement: accepted=%t err=%v", accepted, err)
					}
				} else {
					wantReason = "run_not_inflight"
					completion, err := store.CompleteRun(ctx, &state.RunCompletion{RunID: "run", Generation: 1, Status: models.RunStatusInterrupted})
					if err != nil || !completion.Applied {
						t.Fatalf("cancel run: result=%+v err=%v", completion, err)
					}
				}
				unblock()
				select {
				case err := <-result:
					var fence *state.ErrRunCheckpointFence
					if !errors.As(err, &fence) || fence.ReasonCode != wantReason {
						t.Fatalf("stale checkpoint mutation: err=%v want %s", err, wantReason)
					}
				case <-ctx.Done():
					t.Fatal("checkpoint transaction did not finish")
				}
				checkpoint, err := store.GetOpaqueCheckpoint(ctx, "thread", "state")
				if err != nil || string(checkpoint.Data) != "committed history" {
					t.Fatalf("checkpoint changed after rejected mutation: checkpoint=%+v err=%v", checkpoint, err)
				}
			})
		}
	}
}
