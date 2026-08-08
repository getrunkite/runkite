// Package models defines the data types for the Agent Protocol spec.
// Types match the schemas in spec/openapi.json (Agent Protocol v0.1.6
// plus Runkite extensions). Admin and runner-only shapes live in
// spec/openapi-admin.json and spec/openapi-internal.json.
package models

import (
	"encoding/json"
	"time"
)

// --------------------------------------------------------------------------
// Agents
// --------------------------------------------------------------------------

// Agent represents a registered agent (graph) in the control plane.
type Agent struct {
	// TenantID is populated on read (see models.Run's TenantID for the
	// same convention) so cross-tenant callers -- the Admin API's system
	// context -- can tell which tenant owns each row. Hidden from the
	// public Agent Protocol response shape.
	TenantID     string                 `json:"-"`
	AgentID      string                 `json:"agent_id"`
	Name         string                 `json:"name"`
	Description  string                 `json:"description,omitempty"`
	Metadata     map[string]interface{} `json:"metadata,omitempty"`
	Capabilities map[string]interface{} `json:"capabilities"`
	// Version is basic version tracking: incremented on update, with the
	// latest version served by default. Starts at 1, increments only
	// when name/description/metadata/capabilities
	// actually change on an UpsertAgent call -- not on every bootstrap/
	// restart with an unchanged langgraph.json. Full version history
	// browsing, rollback, and A/B deployment are post-launch scope.
	Version int `json:"version"`
}

// AgentVersion is one immutable, historical snapshot of an agent's
// definition, supporting full agent versioning: version history browsing
// and rollback to arbitrary past versions. Written once, at the
// moment UpsertAgent bumps Agent.Version -- never updated afterward,
// which is what makes it a reliable history rather than another mutable
// copy of the current row. A rollback (see RollbackAgentVersion) doesn't
// delete or rewrite any AgentVersion row -- it re-applies an old
// snapshot's content via a normal UpsertAgent call, which itself creates
// a NEW version whose CONTENT matches the old one. This keeps the
// version history strictly linear and append-only (like `git revert`,
// not `git reset`) -- "we rolled back to v2's content" is itself a
// visible, auditable event (v6 exists and says so), not something that
// erases v3/v4/v5 from history.
type AgentVersion struct {
	TenantID     string                 `json:"-"`
	AgentID      string                 `json:"agent_id"`
	Version      int                    `json:"version"`
	Name         string                 `json:"name"`
	Description  string                 `json:"description,omitempty"`
	Metadata     map[string]interface{} `json:"metadata,omitempty"`
	Capabilities map[string]interface{} `json:"capabilities"`
	CreatedAt    time.Time              `json:"created_at"`
}

// AgentSchema contains the input/output/state/config schemas for an agent.
type AgentSchema struct {
	AgentID      string                 `json:"agent_id"`
	InputSchema  map[string]interface{} `json:"input_schema"`
	OutputSchema map[string]interface{} `json:"output_schema"`
	StateSchema  map[string]interface{} `json:"state_schema,omitempty"`
	ConfigSchema map[string]interface{} `json:"config_schema,omitempty"`
}

// AgentSearchRequest is the body for POST /agents/search.
type AgentSearchRequest struct {
	Name     string                 `json:"name,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
	Limit    int                    `json:"limit,omitempty"`
	Offset   int                    `json:"offset,omitempty"`
	// Cursor is an opaque keyset token (Admin lists). When set, Offset is
	// ignored. Client Protocol search never sets this.
	Cursor string `json:"cursor,omitempty"`
}

// --------------------------------------------------------------------------
// Threads
// --------------------------------------------------------------------------

// ThreadStatus represents the status of a thread.
type ThreadStatus string

const (
	ThreadStatusIdle        ThreadStatus = "idle"
	ThreadStatusBusy        ThreadStatus = "busy"
	ThreadStatusInterrupted ThreadStatus = "interrupted"
	ThreadStatusError       ThreadStatus = "error"
)

// Thread represents a conversation thread.
type Thread struct {
	// TenantID -- see Agent.TenantID's doc comment for the convention.
	TenantID  string                 `json:"-"`
	ThreadID  string                 `json:"thread_id"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
	Metadata  map[string]interface{} `json:"metadata"`
	Status    ThreadStatus           `json:"status"`
	Values    map[string]interface{} `json:"values,omitempty"`
	// Version is a plain increment-on-write counter for optimistic
	// concurrency on PATCH /threads -- a client that wants to detect
	// "someone else changed this since I last read it" round-trips this
	// value back as ThreadPatch.IfMatchVersion. Starts at 1 on creation,
	// +1 on every successful UpdateThread (metadata/values changes only).
	// Status transitions via SetThreadStatus/TryClaimThread/
	// ReleaseThreadIfNoOtherActive deliberately do NOT bump it -- this
	// guards metadata/values edits specifically,
	// not "any row change" -- so a client polling status alone won't see
	// spurious version bumps unrelated to what it's trying to protect.
	// Not the same "version" concept as agent versioning (models.Agent)
	// -- purely a per-thread write counter.
	Version int `json:"version"`
}

// ThreadCreate is the body for POST /threads.
type ThreadCreate struct {
	ThreadID string                 `json:"thread_id,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
	IfExists string                 `json:"if_exists,omitempty"` // "raise" or "do_nothing"
}

// ThreadPatch is the body for PATCH /threads/{id}.
type ThreadPatch struct {
	Metadata map[string]interface{} `json:"metadata,omitempty"`
	Values   map[string]interface{} `json:"values,omitempty"`
	// IfMatchVersion, if set, makes this PATCH conditional (optimistic
	// concurrency, see Thread.Version's own doc comment): the
	// update only applies if the thread's CURRENT version still equals
	// this value at write time, checked atomically in the same UPDATE
	// statement (not a separate read-then-compare, which would still
	// race). A mismatch (someone else updated the thread first) returns
	// state.ErrConflict, not a silent overwrite. nil (the default,
	// unchanged from before this field existed) means "apply
	// unconditionally," preserving the original last-write-wins
	// behavior for callers that don't need this protection.
	IfMatchVersion *int `json:"if_match_version,omitempty"`
}

// ThreadSearchRequest is the body for POST /threads/search.
type ThreadSearchRequest struct {
	Metadata map[string]interface{} `json:"metadata,omitempty"`
	Values   map[string]interface{} `json:"values,omitempty"`
	Status   *ThreadStatus          `json:"status,omitempty"`
	Limit    int                    `json:"limit,omitempty"`
	Offset   int                    `json:"offset,omitempty"`
	// Cursor is an opaque keyset token (Admin lists). When set, Offset is
	// ignored. Client Protocol search never sets this.
	Cursor string `json:"cursor,omitempty"`
}

// ThreadCheckpoint identifies a specific point in thread history.
type ThreadCheckpoint struct {
	ThreadID      string                 `json:"thread_id"`
	CheckpointNS  string                 `json:"checkpoint_ns"`
	CheckpointID  string                 `json:"checkpoint_id"`
	CheckpointMap map[string]interface{} `json:"checkpoint_map,omitempty"`
}

// ThreadState represents a checkpoint in a thread's history.
type ThreadState struct {
	Values           map[string]interface{} `json:"values"`
	Next             []string               `json:"next"`
	Checkpoint       ThreadCheckpoint       `json:"checkpoint"`
	Metadata         map[string]interface{} `json:"metadata"`
	CreatedAt        *string                `json:"created_at"`
	ParentCheckpoint *ThreadCheckpoint      `json:"parent_checkpoint"`
	Tasks            []interface{}          `json:"tasks"`
	Interrupts       []interface{}          `json:"interrupts"`
}

// ThreadUpdateStateResponse is the response for POST /threads/{id}/state.
type ThreadUpdateStateResponse struct {
	Checkpoint ThreadCheckpoint `json:"checkpoint"`
}

// ThreadHistoryRequest is the body for POST /threads/{id}/history.
type ThreadHistoryRequest struct {
	Limit  int                    `json:"limit,omitempty"`
	Before map[string]interface{} `json:"before,omitempty"`
}

// --------------------------------------------------------------------------
// Runs
// --------------------------------------------------------------------------

// RunStatus represents the status of a run.
type RunStatus string

const (
	RunStatusPending     RunStatus = "pending"
	RunStatusRunning     RunStatus = "running" // Platform extension: not in Agent Protocol v0.1.6 spec
	RunStatusError       RunStatus = "error"
	RunStatusSuccess     RunStatus = "success"
	RunStatusTimeout     RunStatus = "timeout"
	RunStatusInterrupted RunStatus = "interrupted"
)

// Run represents an execution of an agent.
type Run struct {
	// TenantID is populated on read so internal, non-HTTP callers (the
	// gRPC bridge's StatusCallback, driven by a runID with no
	// authenticated request context to derive a tenant from) can
	// discover which tenant a run belongs to and re-derive a correctly
	// scoped context for subsequent writes. Not part of the public
	// Agent Protocol response shape.
	TenantID    string                 `json:"-"`
	RunID       string                 `json:"run_id"`
	ThreadID    string                 `json:"thread_id,omitempty"`
	AgentID     string                 `json:"agent_id,omitempty"`
	AssistantID string                 `json:"assistant_id,omitempty"` // SDK compat: mirrors AgentID
	Status      RunStatus              `json:"status"`
	CreatedAt   time.Time              `json:"created_at"`
	UpdatedAt   time.Time              `json:"updated_at"`
	Metadata    map[string]interface{} `json:"metadata"`
	Input       json.RawMessage        `json:"input,omitempty"`
	Config      json.RawMessage        `json:"config,omitempty"`
	Output      json.RawMessage        `json:"output,omitempty"`
	Error       string                 `json:"error,omitempty"`
	// ParentRunID, RootRunID, Depth support Agent-to-Agent (A2A)
	// delegation, where an agent calls another agent via the same Agent
	// Protocol API. ParentRunID is nil for a normal, top-level run --
	// set only when this run was created via POST /internal/a2a/runs on
	// behalf of another run. RootRunID is the top of the chain (a run's
	// own RunID if it has no parent, otherwise copied from the parent),
	// letting a caller find every run in a delegation tree with one
	// query (WHERE root_run_id = ?) instead of walking parent pointers.
	// Depth is 0 for a top-level run, parent.Depth+1 for a delegated
	// one -- enforced against a2a.max_depth at creation time to prevent
	// runaway/cyclic delegation chains. Direct children per parent are
	// capped separately by a2a.max_breadth.
	ParentRunID *string `json:"parent_run_id,omitempty"`
	RootRunID   *string `json:"root_run_id,omitempty"`
	Depth       int     `json:"depth,omitempty"`
}

// RunCreate is the body for POST /runs and POST /threads/{id}/runs.
type RunCreate struct {
	// RunID lets a client supply its own run ID instead of the server
	// generating one, specifically to make run creation idempotent
	// against network retries: a client that sent a create-run request
	// but never received the response (timeout, dropped connection) can
	// safely retry with the SAME RunID -- the retry returns the run that's already there
	// instead of creating a duplicate. Empty (the common case) means
	// "generate a fresh server-side ID," unchanged from before this
	// field existed. Any non-empty string is accepted, no format
	// validation, matching ThreadID's own existing precedent.
	RunID        string          `json:"run_id,omitempty"`
	ThreadID     string          `json:"thread_id,omitempty"`
	AgentID      string          `json:"agent_id,omitempty"`
	AssistantID  string          `json:"assistant_id,omitempty"` // SDK compat: alias for AgentID
	Input        json.RawMessage `json:"input,omitempty"`
	Config       json.RawMessage `json:"config,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	StreamMode   json.RawMessage `json:"stream_mode,omitempty"` // string or []string
	Webhook      string          `json:"webhook,omitempty"`
	OnCompletion string          `json:"on_completion,omitempty"` // "delete" or "keep"
	OnDisconnect string          `json:"on_disconnect,omitempty"` // "cancel" or "continue"
	IfNotExists  string          `json:"if_not_exists,omitempty"` // "create" or "reject"
	// CheckpointRef is an opaque past-checkpoint id for time-travel
	// resume. Empty/whitespace is treated as absent. LangGraph runners
	// map a non-empty value to configurable.checkpoint_id; non-LangGraph
	// runners reject it rather than ignore it. The control plane does
	// not verify the id exists before dispatch.
	CheckpointRef *string         `json:"checkpoint_ref,omitempty"`
	ResumeCommand json.RawMessage `json:"resume_command,omitempty"`
	Command       json.RawMessage `json:"command,omitempty"` // SDK compat: {"resume": ...}
	// ParentRunID is set only by the internal A2A endpoint
	// (POST /internal/a2a/runs) -- never accepted from a client-facing
	// request, so a normal caller can't forge a delegation chain or
	// bypass the depth limit by claiming an arbitrary parent.
	ParentRunID *string `json:"-"`
}

// RunSearchRequest is the body for POST /runs/search.
type RunSearchRequest struct {
	Metadata map[string]interface{} `json:"metadata,omitempty"`
	Status   *RunStatus             `json:"status,omitempty"`
	ThreadID string                 `json:"thread_id,omitempty"`
	AgentID  string                 `json:"agent_id,omitempty"`
	// RootRunID finds every run in an A2A delegation tree with one
	// query (see Run.RootRunID's doc comment) -- pass any run's own
	// RootRunID (or its RunID, if it IS the tree's root) to list every
	// run delegated from it, directly or transitively. Matches exactly,
	// no partial/prefix semantics.
	RootRunID string `json:"root_run_id,omitempty"`
	// ParentRunID finds direct children of one run (one hop). Used by
	// a2a.max_breadth admission; also available on POST /runs/search.
	ParentRunID string `json:"parent_run_id,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	Offset      int    `json:"offset,omitempty"`
	// Cursor is an opaque keyset token (Admin lists). When set, Offset is
	// ignored. Client Protocol search never sets this.
	Cursor string `json:"cursor,omitempty"`
}

// RunWaitResponse is returned by GET /runs/{id}/wait.
type RunWaitResponse struct {
	Run    *Run                   `json:"run"`
	Values map[string]interface{} `json:"values,omitempty"`
}

// --------------------------------------------------------------------------
// Store
// --------------------------------------------------------------------------

// StoreItem represents an item in the key-value store.
//
// TTLMinutes is a write-only field: it's the desired TTL to apply on a
// PutItem call, mirroring LangGraph BaseStore's PutOp.ttl semantics
// exactly -- nil means no expiration for this item (not "leave any
// existing TTL alone"; the caller has already resolved "not specified"
// vs "explicitly none" upstream, same as LangGraph's own store
// implementations do). It's never populated on a value returned from
// GetItem/SearchItems, matching the Python SDK's Item/SearchItem types,
// which don't expose TTL either.
type StoreItem struct {
	Namespace  []string               `json:"namespace"`
	Key        string                 `json:"key"`
	Value      map[string]interface{} `json:"value"`
	CreatedAt  time.Time              `json:"created_at"`
	UpdatedAt  time.Time              `json:"updated_at"`
	TTLMinutes *float64               `json:"-"`
}

// StorePutRequest is the body for PUT /store/items.
type StorePutRequest struct {
	Namespace  []string               `json:"namespace"`
	Key        string                 `json:"key"`
	Value      map[string]interface{} `json:"value"`
	TTLMinutes *float64               `json:"ttl_minutes,omitempty"`
}

// StoreDeleteRequest is the body for DELETE /store/items.
type StoreDeleteRequest struct {
	Namespace []string `json:"namespace,omitempty"`
	Key       string   `json:"key"`
}

// StoreSearchRequest is the body for POST /store/items/search.
//
// RefreshTTL is a *bool (not bool) so an absent field can default to
// true -- matching LangGraph BaseStore's own TTLConfig default
// (refresh_on_read=True) -- while still letting an explicit "false"
// opt out. A plain bool couldn't distinguish "not sent" from "sent as
// false" over JSON.
type StoreSearchRequest struct {
	NamespacePrefix []string               `json:"namespace_prefix,omitempty"`
	Filter          map[string]interface{} `json:"filter,omitempty"`
	Limit           int                    `json:"limit,omitempty"`
	Offset          int                    `json:"offset,omitempty"`
	RefreshTTL      *bool                  `json:"refresh_ttl,omitempty"`
}

// RefreshTTLOrDefault returns the resolved refresh_ttl behavior: the
// explicit value if the request set one, true otherwise.
func (r *StoreSearchRequest) RefreshTTLOrDefault() bool {
	if r.RefreshTTL == nil {
		return true
	}
	return *r.RefreshTTL
}

// StoreSearchResponse is returned by POST /store/items/search.
type StoreSearchResponse struct {
	Items []*StoreItem `json:"items"`
}

// --------------------------------------------------------------------------
// Vector Store (semantic search)
// --------------------------------------------------------------------------

// VectorItem is one embedded item in the vector store. Namespace is a
// single flat collection name (e.g. "docs", "faq") rather than the
// key-value store's hierarchical segments above -- vector collections are
// conventionally flat (an index/collection per embedding space), and nested
// namespacing doesn't map naturally onto ANN search.
type VectorItem struct {
	Namespace string                 `json:"namespace"`
	ID        string                 `json:"id"`
	Content   string                 `json:"content,omitempty"`
	Embedding []float32              `json:"embedding,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
}

// VectorUpsertRequest is the body for PUT /vectors/items. Upsert (not
// separate create/update) because re-embedding and re-indexing the same
// ID -- e.g. a document that changed -- is the common case, not an error.
type VectorUpsertRequest struct {
	Namespace string                 `json:"namespace"`
	ID        string                 `json:"id"`
	Content   string                 `json:"content,omitempty"`
	Embedding []float32              `json:"embedding"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// VectorDeleteRequest is the body for DELETE /vectors/items.
type VectorDeleteRequest struct {
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
}

// VectorSearchRequest is the body for POST /vectors/search. Filter is an
// exact-match filter over Metadata keys, same convention as the key-value
// store's StoreSearchRequest.Filter.
type VectorSearchRequest struct {
	Namespace string                 `json:"namespace"`
	Embedding []float32              `json:"embedding"`
	TopK      int                    `json:"top_k,omitempty"`
	Filter    map[string]interface{} `json:"filter,omitempty"`
}

// VectorSearchResult is one ranked hit. Score is cosine similarity in
// [-1, 1] (1 = identical direction), not a raw distance -- higher is
// always better, regardless of which distance metric the backend computes
// internally.
type VectorSearchResult struct {
	Item  *VectorItem `json:"item"`
	Score float64     `json:"score"`
}

// VectorSearchResponse is returned by POST /vectors/search.
type VectorSearchResponse struct {
	Results []*VectorSearchResult `json:"results"`
}

// StoreListNamespacesRequest is the body for POST /store/namespaces.
type StoreListNamespacesRequest struct {
	Prefix   []string `json:"prefix,omitempty"`
	Suffix   []string `json:"suffix,omitempty"`
	MaxDepth *int     `json:"max_depth,omitempty"`
	Limit    int      `json:"limit,omitempty"`
	Offset   int      `json:"offset,omitempty"`
}

// CachedRunResult is a memoized run output, part of the LLM response
// caching layer: a configurable per-agent TTL with the cache key derived
// from a hash of the input. Keyed by a hash of (agent_id, input, config) computed in createRun --
// the control plane caches whole-run results, not individual LLM calls
// (it never sees inside a runner's execution to do the latter without
// becoming framework-aware).
type CachedRunResult struct {
	CacheKey  string                 `json:"cache_key"`
	AgentID   string                 `json:"agent_id"`
	Output    map[string]interface{} `json:"output"`
	CreatedAt time.Time              `json:"created_at"`
	ExpiresAt time.Time              `json:"expires_at"`
}

// CronSchedule is one cron-scheduled run definition: cron-expression
// scheduling with multi-instance-safe claiming (a Postgres claim window)
// and timezone support.
type CronSchedule struct {
	// TenantID is populated on read (state.Store) so callers that need to
	// act across every tenant (the scheduler loop, via a system context)
	// know which tenant each schedule belongs to. Not settable by config
	// bootstrap -- always DefaultTenant for schedules loaded from
	// langgraph.json (see cmd/cron.go's bootstrapCronSchedules).
	TenantID   string          `json:"tenant_id,omitempty"`
	Name       string          `json:"name"`
	AgentID    string          `json:"agent_id"`
	Expression string          `json:"expression"` // standard 5-field cron expression
	Timezone   string          `json:"timezone"`   // IANA name, e.g. "America/New_York"; empty means UTC
	Input      json.RawMessage `json:"input,omitempty"`
	Config     json.RawMessage `json:"config,omitempty"`
	Enabled    bool            `json:"enabled"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// WebhookDeadLetter records a webhook delivery that failed all retry
// attempts, part of webhook delivery's retry-and-dead-letter handling.
// Persisted so a failed delivery is inspectable/replayable after the fact,
// not just logged and lost. TenantID scopes the row so a tenant-scoped
// list cannot see another tenant's payloads; admin (system context) still
// lists across every tenant.
type WebhookDeadLetter struct {
	ID        string          `json:"id"`
	TenantID  string          `json:"tenant_id"`
	URL       string          `json:"url"`
	EventType string          `json:"event_type"`
	RunID     string          `json:"run_id"`
	Payload   json.RawMessage `json:"payload"`
	Error     string          `json:"error"`
	Attempts  int             `json:"attempts"`
	FailedAt  time.Time       `json:"failed_at"`
}

// --------------------------------------------------------------------------
// Streaming
// --------------------------------------------------------------------------

// EventStreamRequest is the body for POST /threads/{id}/stream (SSE).
type EventStreamRequest struct {
	Channels   []string   `json:"channels"`
	Namespaces [][]string `json:"namespaces,omitempty"`
	Depth      *int       `json:"depth,omitempty"`
	Since      *int64     `json:"since,omitempty"`
}

// StreamingCommand is the body for POST /threads/{id}/commands.
type StreamingCommand struct {
	ID     int                    `json:"id"`
	Method string                 `json:"method"`
	Params map[string]interface{} `json:"params,omitempty"`
}

// StreamingEvent is a server-to-client event in the streaming protocol.
type StreamingEvent struct {
	Type    string                 `json:"type"` // always "event"
	EventID string                 `json:"eventId,omitempty"`
	Seq     int64                  `json:"seq,omitempty"`
	Method  string                 `json:"method"`
	Params  map[string]interface{} `json:"params"`
}

// StreamingCommandResponse is the response to a streaming command.
type StreamingCommandResponse struct {
	Type    string                 `json:"type"` // "success" or "error"
	ID      int                    `json:"id"`
	Result  map[string]interface{} `json:"result,omitempty"`  // for success
	Error   string                 `json:"error,omitempty"`   // for error
	Message string                 `json:"message,omitempty"` // for error
}

// --------------------------------------------------------------------------
// Registry
// --------------------------------------------------------------------------

// RegistryEntry is a published, discoverable agent DEFINITION for the
// agent marketplace / registry: publish, discover, and deploy agent
// definitions. Deliberately a metadata catalog record, not
// executable code the control plane runs directly -- "minimal viable
// registry" scope: publish/list/discover/version-history via API +
// Admin UI, no security review workflow, no automatic
// clone-and-execute "deploy" pipeline (running arbitrary fetched code is a
// different, much bigger trust and sandboxing problem than a catalog
// is). SourceRef is where a human (or their own tooling) goes to
// actually deploy it -- a git repo, a URL, or an inline langgraph.json
// snippet -- exactly like a package registry's listing page, not the
// package manager's install step.
type RegistryEntry struct {
	// TenantID is populated on read, same convention as models.Agent --
	// a registry is private to the tenant that published it ("an
	// internal/private registry for one's own agents", not a shared
	// public catalog across tenants).
	TenantID    string                 `json:"-"`
	Name        string                 `json:"name"` // unique slug within a tenant, e.g. "sales-qualifier"
	DisplayName string                 `json:"display_name,omitempty"`
	Description string                 `json:"description,omitempty"`
	Author      string                 `json:"author,omitempty"`
	Tags        []string               `json:"tags,omitempty"`
	SourceType  string                 `json:"source_type"` // "git" | "url" | "inline"
	SourceRef   string                 `json:"source_ref"`  // git URL+ref, a URL, or an inline langgraph.json snippet
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
	// Version follows the exact same convention as models.Agent.Version:
	// starts at 1, increments only when the entry's actual content
	// changes on a republish, and every bump writes an immutable
	// snapshot to its own version history -- same append-only,
	// git-revert-not-git-reset model as agent versioning.
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RegistryEntryVersion is one immutable, historical snapshot of a
// registry entry's content -- same role AgentVersion plays for Agent,
// see its doc comment for the full append-only rationale.
type RegistryEntryVersion struct {
	TenantID    string                 `json:"-"`
	Name        string                 `json:"name"`
	Version     int                    `json:"version"`
	DisplayName string                 `json:"display_name,omitempty"`
	Description string                 `json:"description,omitempty"`
	Author      string                 `json:"author,omitempty"`
	Tags        []string               `json:"tags,omitempty"`
	SourceType  string                 `json:"source_type"`
	SourceRef   string                 `json:"source_ref"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt   time.Time              `json:"created_at"`
}

// RegistrySearchRequest is the body for POST /registry/search.
type RegistrySearchRequest struct {
	Name   string   `json:"name,omitempty"`   // substring match, case-insensitive
	Tags   []string `json:"tags,omitempty"`   // entry must have ALL listed tags
	Author string   `json:"author,omitempty"` // exact match
	Limit  int      `json:"limit,omitempty"`
	Offset int      `json:"offset,omitempty"`
	// Cursor is an opaque keyset token (Admin lists). When set, Offset is
	// ignored. Client Protocol search never sets this.
	Cursor string `json:"cursor,omitempty"`
}

// --------------------------------------------------------------------------
// Errors
// --------------------------------------------------------------------------

// ErrorResponse is the standard error response format.
type ErrorResponse struct {
	Code    string                 `json:"code,omitempty"`
	Message string                 `json:"message"`
	Details map[string]interface{} `json:"metadata,omitempty"`
}
