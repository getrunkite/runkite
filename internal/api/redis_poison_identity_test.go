package api_test

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	redistransport "github.com/getrunkite/runkite/internal/transport/redis"
	goredis "github.com/redis/go-redis/v9"
)

func TestRedisPoisonCompletionCannotTargetAnotherTenantsPrefixRun(t *testing.T) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set; requires a disposable Redis DB15")
	}
	options, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	// This test clears rk:* keys, never arbitrary keys or another database.
	// Require the explicit disposable test DB rather than a plane's DB0.
	if options.DB != 15 {
		t.Skip("poison identity regression requires disposable Redis DB15")
	}
	rdb := goredis.NewClient(options)
	t.Cleanup(func() { _ = rdb.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	if err := redistransport.FlushAll(ctx, rdb); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := redistransport.FlushAll(cleanup, rdb); err != nil {
			t.Error(err)
		}
	})
	store, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	queue := redistransport.NewQueue(rdb)
	plane := api.NewServer(store, queue, redistransport.NewBroker(rdb), redistransport.NewCancelBus(rdb))
	const victimID = "poison-prefix-victim"
	const sourceID = victimID + ":2"
	now := time.Now().UTC()
	for _, job := range []*transport.RunAssignment{
		{RunID: victimID, ThreadID: "victim-thread", GraphID: "victim-agent", TenantID: "victim-tenant", RunnerKind: "kind", Generation: 1},
		{RunID: sourceID, ThreadID: "source-thread", GraphID: "source-agent", TenantID: "source-tenant", RunnerKind: "kind", Generation: 2},
	} {
		scoped := tenant.WithContext(ctx, job.TenantID)
		if err := store.CreateThread(scoped, &models.Thread{
			ThreadID: job.ThreadID, Status: models.ThreadStatusBusy, Values: map[string]interface{}{"owner": job.TenantID},
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateRun(scoped, &models.Run{
			RunID: job.RunID, ThreadID: job.ThreadID, AgentID: job.GraphID, Status: models.RunStatusPending,
			Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := queue.Enqueue(ctx, job); err != nil {
			t.Fatal(err)
		}
		if dispatched, err := queue.Dequeue(ctx, job.RunnerKind, time.Second); err != nil || dispatched == nil || dispatched.RunID != job.RunID {
			t.Fatalf("dispatch: assignment=%+v err=%v", dispatched, err)
		}
		if accepted, err := store.BeginRunAttempt(scoped, job.RunID, job.Generation); err != nil || !accepted {
			t.Fatalf("begin attempt: accepted=%t err=%v", accepted, err)
		}
	}
	victimCtx := tenant.WithContext(ctx, "victim-tenant")
	beforeRun, err := store.GetRun(victimCtx, victimID)
	if err != nil {
		t.Fatal(err)
	}
	beforeThread, err := store.GetThread(victimCtx, "victim-thread")
	if err != nil {
		t.Fatal(err)
	}
	// Select only the exhausted source lease; the unrelated victim stays live.
	if err := rdb.ZAdd(ctx, "rk:inflight:zset", goredis.Z{Score: 0, Member: sourceID}).Err(); err != nil {
		t.Fatal(err)
	}
	n, dead, err := queue.ReclaimStale(ctx, time.Hour, 2)
	if err != nil || n != 0 || len(dead) != 1 {
		t.Fatalf("poison selection: n=%d pills=%+v err=%v", n, dead, err)
	}
	// Exercise the trusted reaper callback before checking the pill's fields.
	// The old colon parser selected victimID at generation 1 across tenants.
	const message = "retry ceiling reached for source"
	if accepted, err := plane.CompleteReclaimedRun(ctx, dead[0], message); err != nil || !accepted {
		t.Fatalf("poison completion: accepted=%t err=%v", accepted, err)
	}
	afterRun, err := store.GetRun(victimCtx, victimID)
	if err != nil {
		t.Fatal(err)
	}
	afterThread, err := store.GetThread(victimCtx, "victim-thread")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeRun, afterRun) || !reflect.DeepEqual(beforeThread, afterThread) {
		t.Fatalf("source poison completion changed another tenant's run or thread: beforeRun=%+v afterRun=%+v beforeThread=%+v afterThread=%+v", beforeRun, afterRun, beforeThread, afterThread)
	}
	pill := dead[0]
	if pill.RunID != sourceID || pill.Generation != 3 || pill.AgentID != "source-agent" || pill.TenantID != "source-tenant" {
		t.Fatalf("source poison identity changed: %+v", pill)
	}
	source, err := store.GetRun(tenant.WithContext(ctx, "source-tenant"), sourceID)
	if err != nil || source.Status != models.RunStatusError || source.Error != message ||
		state.MetadataGeneration(source.Metadata, state.CompletionGenerationKey) != 2 {
		t.Fatalf("source was not completed at its retained generation: run=%+v err=%v", source, err)
	}
	if lease, err := queue.LookupInflight(ctx, victimID); err != nil || lease == nil || lease.Generation != 1 || lease.TenantID != "victim-tenant" {
		t.Fatalf("victim lease changed: assignment=%+v err=%v", lease, err)
	}
	if acknowledged, err := queue.Ack(ctx, sourceID, 2); err != nil || !acknowledged {
		t.Fatalf("source acknowledgement: accepted=%t err=%v", acknowledged, err)
	}
}
