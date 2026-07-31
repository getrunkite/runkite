// Package state defines the StateStore interface for persistent storage
// of agents, threads, runs, and store items. Implemented today by SQLite
// and Postgres (internal/state/sqlite, internal/state/postgres); a
// MongoDB backend (internal/state/mongo) is the project's non-SQL
// exemplar for community-contributed backends. Any backend that
// satisfies this interface and passes internal/state/conformance's
// shared test suite is a valid implementation -- MySQL/DynamoDB remain
// possible future drivers, not currently planned.
package state

import (
	"context"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
)

// Store is the persistence interface for all control plane metadata.
type Store interface {
	// --- Agents ---
	GetAgent(ctx context.Context, agentID string) (*models.Agent, error)
	SearchAgents(ctx context.Context, req *models.AgentSearchRequest) ([]*models.Agent, error)
	UpsertAgent(ctx context.Context, agent *models.Agent) error
	GetAgentSchema(ctx context.Context, agentID string) (*models.AgentSchema, error)
	UpsertAgentSchema(ctx context.Context, schema *models.AgentSchema) error
	// ListAgentVersions returns every historical snapshot for agentID,
	// newest first, supporting full version history browsing. Written
	// by UpsertAgent itself, once per actual version bump -- see
	// models.AgentVersion's doc comment.
	ListAgentVersions(ctx context.Context, agentID string) ([]*models.AgentVersion, error)
	// GetAgentVersion returns one specific historical snapshot, or
	// ErrNotFound if that version never existed for this agent.
	GetAgentVersion(ctx context.Context, agentID string, version int) (*models.AgentVersion, error)

	// --- Threads ---
	CreateThread(ctx context.Context, thread *models.Thread) error
	GetThread(ctx context.Context, threadID string) (*models.Thread, error)
	UpdateThread(ctx context.Context, threadID string, patch *models.ThreadPatch) (*models.Thread, error)
	DeleteThread(ctx context.Context, threadID string) error
	SearchThreads(ctx context.Context, req *models.ThreadSearchRequest) ([]*models.Thread, error)
	SetThreadStatus(ctx context.Context, threadID string, status models.ThreadStatus) error

	// TryClaimThread atomically transitions a thread from any non-busy status
	// to busy in a single conditional statement, so two concurrent requests can
	// never both observe "not busy" and both proceed (TOCTOU-safe). Returns
	// false, without error, if the thread was already busy.
	TryClaimThread(ctx context.Context, threadID string) (bool, error)

	// ReleaseThreadIfNoOtherActive atomically sets the thread's status
	// (typically idle or interrupted) only when the thread is currently
	// busy AND no pending/running run exists on it other than
	// excludeRunID. Closes the check-then-act race where SearchRuns +
	// SetThreadStatus could idle a thread a newer run had already
	// claimed. Returns (true, nil) if the UPDATE applied; (false, nil)
	// if skipped (another active run, or thread not busy). Does not
	// bump Thread.Version (same as SetThreadStatus/TryClaimThread).
	ReleaseThreadIfNoOtherActive(ctx context.Context, threadID, excludeRunID string, status models.ThreadStatus) (bool, error)

	// --- Checkpoints ---
	SaveCheckpoint(ctx context.Context, threadID string, state *models.ThreadState) error
	GetLatestCheckpoint(ctx context.Context, threadID string) (*models.ThreadState, error)
	ListCheckpoints(ctx context.Context, threadID string, limit int, before string) ([]*models.ThreadState, error)

	// --- Runs ---
	CreateRun(ctx context.Context, run *models.Run) error
	GetRun(ctx context.Context, runID string) (*models.Run, error)
	UpdateRunStatus(ctx context.Context, runID string, status models.RunStatus, output []byte, errMsg string) error
	DeleteRun(ctx context.Context, runID string) error
	SearchRuns(ctx context.Context, req *models.RunSearchRequest) ([]*models.Run, error)

	// ListActiveRunsCreatedBefore returns pending/running runs whose
	// created_at is strictly before `before`, oldest first, capped at
	// limit. Backs the opt-in run-timeout sweep (cmd/run_timeout.go): a
	// genuinely hung agent (alive but stuck -- not crashed, so the
	// heartbeat/reclaim path never fires) has no other automatic
	// terminal transition. Scoped to the caller's tenant unless ctx is
	// a system context. limit <= 0 defaults to 100. Returns a non-nil
	// empty slice when nothing matches.
	ListActiveRunsCreatedBefore(ctx context.Context, before time.Time, limit int) ([]*models.Run, error)

	// TryMarkRunTimeout atomically transitions a run from pending or
	// running to timeout. Returns true if THIS call won the transition
	// (so the caller should cancel the queue lease, signal the runner,
	// release the thread, and finish bookkeeping); false if the run was
	// already terminal or another replica timed it out first. Multi-
	// instance safe: N replicas' timeout tickers racing the same overdue
	// run_id produce exactly one winner.
	TryMarkRunTimeout(ctx context.Context, runID string, errMsg string) (bool, error)

	// --- Store (key-value) ---
	// TTL (gap found when a live agent called store.aput(..., ttl=...),
	// a documented LangGraph BaseStore feature RunkiteStore never
	// implemented, so it hard-failed with "TTL is not supported by
	// RunkiteStore"). Matches LangGraph's own semantics: item.TTLMinutes
	// nil on PutItem means no expiration;
	// an expired item (now >= its stored expiry) is treated as absent by
	// GetItem/SearchItems, deleted opportunistically by a background
	// sweep (see PruneExpiredStoreItems) rather than synchronously here.
	PutItem(ctx context.Context, item *models.StoreItem) error
	// GetItem returns ErrNotFound for a missing OR expired item (the
	// HTTP layer maps that to 404; Python RunkiteStore maps 404 to
	// None). If refreshTTL is true and the item has a TTL, its expiry
	// is extended to now+ttl -- matching LangGraph's "expiration timer
	// refreshes on read operations" default (Python callers resolve
	// their own TTLConfig.refresh_on_read default before passing this
	// through).
	GetItem(ctx context.Context, namespace []string, key string, refreshTTL bool) (*models.StoreItem, error)
	DeleteItem(ctx context.Context, namespace []string, key string) error
	// SearchItems excludes expired items and, per req.RefreshTTL, refreshes
	// the TTL of every item it returns -- same semantics as GetItem.
	SearchItems(ctx context.Context, req *models.StoreSearchRequest) ([]*models.StoreItem, error)
	ListNamespaces(ctx context.Context, req *models.StoreListNamespacesRequest) ([][]string, error)

	// --- Webhook dead-letter (event hooks / webhook delivery) ---
	SaveWebhookDeadLetter(ctx context.Context, dl *models.WebhookDeadLetter) error
	// ListWebhookDeadLetters returns newest-first dead letters, scoped
	// to the caller's tenant unless ctx is a system context
	// (tenant.SystemContext), in which case it lists across every tenant.
	ListWebhookDeadLetters(ctx context.Context, limit int) ([]*models.WebhookDeadLetter, error)
	// PruneWebhookDeadLetters deletes dead letters whose failed_at is
	// older than olderThan. Same tenant-scoping rule as ListWebhookDeadLetters.
	PruneWebhookDeadLetters(ctx context.Context, olderThan time.Time) (int64, error)

	// --- Run cache (LLM response caching) ---
	// GetCachedRunResult returns ErrNotFound for a missing OR expired
	// entry -- callers never need to separately check ExpiresAt.
	GetCachedRunResult(ctx context.Context, cacheKey string) (*models.CachedRunResult, error)
	SaveCachedRunResult(ctx context.Context, result *models.CachedRunResult) error

	// --- Cron scheduler ---
	UpsertCronSchedule(ctx context.Context, sched *models.CronSchedule) error
	ListCronSchedules(ctx context.Context) ([]*models.CronSchedule, error)
	DeleteCronSchedule(ctx context.Context, name string) error
	// TryClaimCronFire atomically claims a specific (schedule, fire time)
	// pair so exactly one control-plane instance dispatches the run for
	// it, even with multiple replicas of the scheduler loop running
	// concurrently against the same database. Returns false, without
	// error, if another instance already claimed it first.
	TryClaimCronFire(ctx context.Context, scheduleName string, fireTime time.Time) (bool, error)
	// ReleaseCronClaim undoes a successful TryClaimCronFire so a later
	// tick (or peer) can retry the same fire after a transient dispatch
	// failure. No-op if the claim row is already gone.
	ReleaseCronClaim(ctx context.Context, scheduleName string, fireTime time.Time) error
	// GetLastCronFireTime returns the most recent fire_time ever claimed
	// for a schedule, and false if none exists yet. Lets the scheduler
	// distinguish "restarting a schedule that has fired before" (catch up
	// to the latest fire missed since then) from "a schedule that has
	// never fired" (start counting from now -- no surprise immediate
	// fire just because its expression's most recent occurrence already
	// passed before it was ever registered).
	GetLastCronFireTime(ctx context.Context, scheduleName string) (fireTime time.Time, found bool, err error)

	// --- Retention ---
	// No automatic cleanup existed before this: runs/checkpoints grew
	// unbounded, permanent until an explicit DELETE /threads/{id}. Both
	// methods are opt-in no-ops when unconfigured -- see cmd/retention.go
	// for the background loop and langgraph.json's "retention" section.

	// PruneRuns deletes TERMINAL runs (success/error/interrupted/timeout
	// -- matching api.isTerminalStatus; never pending/running, which
	// must never be pruned regardless of age) whose updated_at is older
	// than olderThan. Scoped to the caller's
	// tenant unless ctx is a system context (tenant.SystemContext), in
	// which case it prunes across every tenant. Returns rows deleted.
	// Cascades to that run's own data via the runs table's own FK
	// constraints where applicable; does not touch the parent thread.
	PruneRuns(ctx context.Context, olderThan time.Time) (int64, error)

	// PruneCheckpoints keeps only the keepLast most recent checkpoints
	// per thread (ordered by created_at), deleting older ones. keepLast
	// <= 0 is a no-op (prunes nothing) -- there is no "prune everything"
	// footgun via a zero/negative value. Same tenant-scoping rule as
	// PruneRuns. Never deletes a thread's own values_json (the "current
	// state" snapshot lives on the threads row itself, not here).
	PruneCheckpoints(ctx context.Context, keepLast int) (int64, error)

	// PruneCronClaims deletes cron_claims rows (the multi-instance-safe
	// fire dedup table) with fire_time older than olderThan -- README's
	// "Cron's claim table has no retention sweep" gap, closed alongside
	// the others since it's the same class of unbounded-growth problem.
	// Same tenant-scoping rule as PruneRuns.
	PruneCronClaims(ctx context.Context, olderThan time.Time) (int64, error)

	// PruneExpiredStoreItems deletes store_items rows past their TTL
	// expiry (see the Store section's TTL doc comment above). Unlike
	// PruneRuns/PruneCheckpoints/PruneCronClaims this is NOT opt-in --
	// always run by cmd/serve.go on a fixed interval regardless of
	// "retention" config, since it's deleting data that is already
	// unconditionally excluded from every read (GetItem/SearchItems),
	// not a configurable data-retention policy choice. Same
	// tenant-scoping rule as PruneRuns.
	PruneExpiredStoreItems(ctx context.Context) (int64, error)

	// --- Registry: an agent marketplace for publishing, discovering,
	// and deploying agent definitions ---
	// PublishRegistryEntry creates a new entry or republishes an
	// existing one, same increment-on-actual-change version semantics
	// as UpsertAgent (see models.RegistryEntry's doc comment).
	PublishRegistryEntry(ctx context.Context, entry *models.RegistryEntry) error
	GetRegistryEntry(ctx context.Context, name string) (*models.RegistryEntry, error)
	SearchRegistryEntries(ctx context.Context, req *models.RegistrySearchRequest) ([]*models.RegistryEntry, error)
	DeleteRegistryEntry(ctx context.Context, name string) error
	// ListRegistryEntryVersions / GetRegistryEntryVersion mirror
	// ListAgentVersions / GetAgentVersion exactly -- same append-only
	// history contract.
	ListRegistryEntryVersions(ctx context.Context, name string) ([]*models.RegistryEntryVersion, error)
	GetRegistryEntryVersion(ctx context.Context, name string, version int) (*models.RegistryEntryVersion, error)

	// --- Lifecycle ---
	Init(ctx context.Context) error // Create tables / run migrations
	Close() error

	// Ping verifies the store can actually reach its backing database
	// right now -- a real round trip, not just "was the connection pool
	// constructed successfully at startup." Backs GET /readyz: a
	// replica whose Postgres/MySQL/Mongo has since become unreachable
	// (network partition, DB restart, credentials rotated out from
	// under it) should stop receiving new traffic from a load balancer
	// even though its own process is still very much alive -- which is
	// exactly the distinction a liveness check alone can't make.
	Ping(ctx context.Context) error
}

// ErrNotFound is returned when a requested resource does not exist.
type ErrNotFound struct {
	Resource string
	ID       string
}

func (e *ErrNotFound) Error() string {
	return e.Resource + " not found: " + e.ID
}

// ErrConflict is returned for any conflicting write a caller should see as
// HTTP 409 -- both "already exists" (e.g. duplicate thread_id) and other
// conflict shapes like an optimistic-concurrency version mismatch, which
// don't fit "already exists" at all (the resource already existed and this
// write lost a race, it didn't newly appear).
type ErrConflict struct {
	Resource string
	ID       string
	// Reason overrides the default "already exists" wording when set.
	// Empty (the common case) keeps the original message unchanged for
	// every existing caller.
	Reason string
}

func (e *ErrConflict) Error() string {
	if e.Reason != "" {
		return e.Resource + " " + e.Reason + ": " + e.ID
	}
	return e.Resource + " already exists: " + e.ID
}
