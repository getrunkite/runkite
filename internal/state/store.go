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

	"github.com/getrunkite/runkite/internal/models"
)

// Store is the persistence interface for all control plane metadata.
type Store interface {
	// --- Agents ---
	GetAgent(ctx context.Context, agentID string) (*models.Agent, error)
	SearchAgents(ctx context.Context, req *models.AgentSearchRequest) ([]*models.Agent, error)
	// CountAgents returns the number of agents visible under ctx
	// (tenant-scoped, or every tenant when ctx is SystemContext). Used
	// by the Admin overview so totals are real COUNT aggregates, not
	// len(SearchAgents) capped by a list page size.
	CountAgents(ctx context.Context) (int, error)
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
	// CountThreadsByStatus returns per-status counts for threads
	// visible under ctx (system = all tenants). Empty statuses are
	// omitted; total threads is the sum of the map values.
	CountThreadsByStatus(ctx context.Context) (map[string]int, error)
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

	// --- Checkpoints (Agent Protocol thread history / time-travel view) ---
	SaveCheckpoint(ctx context.Context, threadID string, state *models.ThreadState) error
	GetLatestCheckpoint(ctx context.Context, threadID string) (*models.ThreadState, error)
	ListCheckpoints(ctx context.Context, threadID string, limit int, before string) ([]*models.ThreadState, error)

	// --- Opaque runner checkpoints (proxy-mode BaseCheckpointSaver blobs) ---
	// Distinct from ThreadState history above: these are framework-owned
	// bytes the control plane never parses (LangGraph ProxyCheckpointSaver,
	// future CrewAI adapters, etc.). See runner-protocol §6.2.
	//
	// PutOpaqueCheckpoint upserts the blob and returns the new version
	// (ETag). When ifMatch is non-nil:
	//   - *ifMatch >= 1: UPDATE-only CAS (version must equal *ifMatch)
	//   - *ifMatch == OpaqueCreateOnly (0): INSERT-only (If-None-Match: *);
	//     returns *ErrConflict if the row already exists
	// nil ifMatch = unconditional upsert (bumps version when the row
	// already existed).
	PutOpaqueCheckpoint(ctx context.Context, threadID, checkpointID string, data []byte, framework string, ifMatch *int64) (int64, error)
	GetOpaqueCheckpoint(ctx context.Context, threadID, checkpointID string) (*models.OpaqueCheckpoint, error)
	// GetLatestOpaqueCheckpoint returns the newest blob for threadID,
	// ordered by checkpoint_id DESC (LangGraph time-sortable UUIDs —
	// same as AsyncPostgresSaver). Not by created_at: parent blobs often
	// receive late aput_writes that bump write time, and Mongo Date is
	// only ms-precise so concurrent tip/parent writes routinely tie.
	// namespace "" matches root keys (checkpoint_id with no "\x1f");
	// a non-empty namespace matches keys prefixed with namespace+"\x1f".
	GetLatestOpaqueCheckpoint(ctx context.Context, threadID, namespace string) (*models.OpaqueCheckpoint, error)
	ListOpaqueCheckpoints(ctx context.Context, threadID string, limit int) ([]models.OpaqueCheckpointMeta, error)
	DeleteOpaqueCheckpoint(ctx context.Context, threadID, checkpointID string) error
	// PruneOpaqueCheckpoints keeps only keepLast most recent opaque
	// checkpoints per thread (by created_at), deleting older ones.
	// Same retention knob as PruneCheckpoints (checkpoints_keep_last).
	PruneOpaqueCheckpoints(ctx context.Context, keepLast int) (int64, error)

	// --- Runs ---
	CreateRun(ctx context.Context, run *models.Run) error
	// CreateRunAdmitted inserts a run only if caps allow. When caps is
	// nil or disabled, identical to CreateRun. Otherwise serializes
	// per-tenant / per-agent scope (DB advisory lock or equivalent),
	// COUNTs, then INSERTs on the same connection/transaction so a
	// burst of concurrent creates cannot all pass a stale COUNT.
	// Returns *ErrAdmissionLimitExceeded when over ceiling.
	CreateRunAdmitted(ctx context.Context, run *models.Run, caps *RunAdmissionCaps) error
	GetRun(ctx context.Context, runID string) (*models.Run, error)
	UpdateRunStatus(ctx context.Context, runID string, status models.RunStatus, output []byte, errMsg string) error
	DeleteRun(ctx context.Context, runID string) error
	SearchRuns(ctx context.Context, req *models.RunSearchRequest) ([]*models.Run, error)
	// CountRunsByStatus returns per-status counts for runs visible
	// under ctx (system = all tenants). Empty statuses are omitted;
	// total runs is the sum of the map values.
	CountRunsByStatus(ctx context.Context) (map[string]int, error)
	// CountActiveRuns returns pending+running runs visible under ctx.
	// When agentID is non-empty, only that agent is counted. Used by
	// admission_limits.max_concurrent.
	CountActiveRuns(ctx context.Context, agentID string) (int, error)
	// CountRunsCreatedSince returns runs with created_at >= since
	// (inclusive), tenant-scoped like CountActiveRuns. Used by
	// admission_limits.max_daily (UTC day start).
	CountRunsCreatedSince(ctx context.Context, since time.Time, agentID string) (int, error)
	// TryClaimTerminalHook atomically claims the right to dispatch the
	// terminal webhook (run_complete/error/interrupt) for runID so
	// exactly one control-plane replica fires it when cancel and
	// ReportStatus race across an LB. Returns false, without error, if
	// another replica (or an earlier call) already claimed it. run_id is
	// globally unique (UUID); no tenant scoping. Failures from the
	// underlying store are returned to the caller -- api.finishRun
	// fail-opens (still dispatches) on error so a blip cannot silence
	// every terminal webhook. api.finishRun only calls this when
	// hooks.HasSinks() is true so unconfigured deployments pay nothing.
	TryClaimTerminalHook(ctx context.Context, runID string) (bool, error)

	// PruneTerminalHookClaims deletes terminal_hook_claims rows whose
	// claimed_at is older than olderThan. Not tenant-scoped (the table
	// keys only on run_id). Opt-in via retention.terminal_hook_claims_max_age.
	PruneTerminalHookClaims(ctx context.Context, olderThan time.Time) (int64, error)

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
	Init(ctx context.Context) error      // Apply pending numbered schema migrations
	Downgrade(ctx context.Context) error // Roll back the most recently applied migration
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
// OpaqueCreateOnly is the ifMatch sentinel for INSERT-only puts
// (HTTP If-None-Match: *). Real opaque versions always start at 1, so 0
// cannot collide with a CAS If-Match value.
const OpaqueCreateOnly int64 = 0

// ErrConflict is returned for optimistic-concurrency failures and
// "already exists" collisions (e.g. duplicate thread_id, or create-only
// put when the opaque checkpoint row is already present). Mapped to
// HTTP 409 or 412 depending on the route.
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
