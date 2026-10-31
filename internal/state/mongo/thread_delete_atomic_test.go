package mongo

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestDeleteThreadChildFailureRollsBackParent(t *testing.T) {
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		t.Skip("MONGO_URI not set")
	}
	ctx := tenant.WithContext(context.Background(), "delete-owner")
	deleteCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var armed atomic.Bool
	monitor := &event.CommandMonitor{Started: func(_ context.Context, command *event.CommandStartedEvent) {
		if armed.Load() && command.CommandName == "delete" && command.Command.Lookup("delete").StringValue() == "runs" {
			cancel()
		}
	}}
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetMonitor(monitor).SetBSONOptions(&options.BSONOptions{DefaultDocumentM: true}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	store := &Store{client: client, db: client.Database("runkite_delete_atomic_" + uuid.NewString())}
	t.Cleanup(func() { _ = store.db.Drop(context.Background()) })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: "parent", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, &models.Run{RunID: "child", ThreadID: "parent", AgentID: "agent", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	if err := store.DeleteThread(deleteCtx, "parent"); err == nil {
		t.Fatal("injected cascade failure was ignored")
	}
	armed.Store(false)
	if deleteCtx.Err() == nil {
		t.Fatal("test never injected the child-delete failure")
	}
	if _, err := store.GetThread(ctx, "parent"); err != nil {
		t.Fatalf("failed cascade deleted parent: %v", err)
	}
	if _, err := store.GetRun(ctx, "child"); err != nil {
		t.Fatalf("failed cascade deleted child: %v", err)
	}
}
