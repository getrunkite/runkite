// Package api implements the Agent Protocol HTTP endpoints.
// Each resource group (agents, threads, runs, store, streaming) is in its own file.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/getrunkite/runkite/internal/adminui"
	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/customroutes"
	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/metrics"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/ratelimit"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/vectorstore"
)

// Server is the HTTP API server for the Agent Protocol.
type Server struct {
	store             state.Store
	queue             transport.JobQueue
	broker            transport.EventBroker
	cancel            transport.CancelBroker
	connectors        *connector.Registry             // nil if no connectors configured
	connectorSessions connector.ConnectorSessionStore // MCP capability tokens; nil = /mcp fail-closed
	policy            *policy.Engine                  // nil = V1 open (no policy configured)
	policyRunEvents   bool                            // emit tool_auth RunEvents on policy denials
	rateLimit         *ratelimit.Limiter              // nil-safe: nil behaves as disabled
	hooks             *hooks.Dispatcher               // nil-safe: nil Dispatch/HasSinks are no-ops
	customProxy       http.Handler                    // nil if no custom_routes configured
	customMount       string                          // external prefix (default /custom); read on every request
	vectors           vectorstore.VectorStore         // nil if no vector_store configured; /vectors/* 501s
	a2aMaxDepth       int                             // 0 means "use the default" -- see SetA2AMaxDepth
	a2aMaxBreadth     int                             // 0 means "use the default" -- see SetA2AMaxBreadth
	admissionLimits   *AdmissionLimits                // nil/disabled = unlimited occupancy/quota
	aliases           *AliasResolver                  // nil-safe: nil Resolve is a pass-through
	// wsOriginPatterns, when non-empty, restricts WebSocket upgrades to
	// those Origin values (same list as cors.allow_origins). Empty means
	// coder/websocket's default any-Origin accept (token auth still applies).
	wsOriginPatterns []string

	// adminSessions serves POST/GET/DELETE /admin-api/session for the
	// Admin UI httpOnly cookie login. nil means those routes 404 (tests
	// that never call SetAdminSessions); production always sets it.
	adminSessions *auth.AdminSessionHandlers

	// runSpans holds the in-flight OTel span for each run, from createRun
	// until finishRun closes it out. Process-local on purpose: if the
	// control plane restarts mid-run, that run's span is never End()'d
	// and is dropped with the process -- a missing trace segment, not
	// corrupted data. Losing a trace segment on crash (or when
	// StatusCallback lands on a different replica than createRun) is an
	// accepted observability trade-off; terminal webhooks must NOT be
	// gated on this map -- see finishRun / finishedRuns.
	runSpans sync.Map // run_id -> trace.Span

	// finishedRuns is a process-local fast path that skips a second DB
	// claim when cancel and StatusCallback race on the same replica.
	// Cross-replica exactly-once is state.Store.TryClaimTerminalHook.
	finishedRuns sync.Map // run_id -> struct{}
}

// NewServer creates the HTTP API server.
func NewServer(store state.Store, queue transport.JobQueue, broker transport.EventBroker, cancel transport.CancelBroker) *Server {
	return &Server{
		store:  store,
		queue:  queue,
		broker: broker,
		cancel: cancel,
	}
}

// SetConnectorRegistry attaches a connector registry to the server.
// Called after NewServer when connectors are configured.
func (s *Server) SetConnectorRegistry(r *connector.Registry) {
	s.connectors = r
}

// SetConnectorSessionStore wires short-lived MCP capability tokens
// (minted on GetSession, required on /mcp). Production always sets this
// (Redis when REDIS_URL is set, else memory).
func (s *Server) SetConnectorSessionStore(store connector.ConnectorSessionStore) {
	s.connectorSessions = store
}

// SetPolicyEngine attaches the connector/tool policy engine. Nil keeps
// V1 open connector access (after runner auth + run-binding).
func (s *Server) SetPolicyEngine(e *policy.Engine) {
	s.policy = e
}

// SetPolicyRunEvents toggles publishing method "tool_auth" RunEvents
// when a connector session or tools/call is denied by policy.
func (s *Server) SetPolicyRunEvents(on bool) {
	s.policyRunEvents = on
}

// SetRateLimiter attaches a rate limiter to the server. Called after
// NewServer when a rate_limit config is present; a nil/never-set limiter
// means per-agent limiting in createRun is a no-op (global/per-user
// limiting is a separate HTTP middleware layer -- see ratelimit.Middleware
// in cmd/serve.go).
func (s *Server) SetRateLimiter(l *ratelimit.Limiter) {
	s.rateLimit = l
}

// defaultA2AMaxDepth applies when a2a.max_depth is unset or <= 0 --
// see A2AEntry's doc comment in internal/config/loader.go.
const defaultA2AMaxDepth = 10

// defaultA2AMaxBreadth applies when a2a.max_breadth is unset or <= 0.
const defaultA2AMaxBreadth = 20

// SetA2AMaxDepth configures the maximum Agent-to-Agent delegation chain
// depth. Called after NewServer when an "a2a" config section is present;
// never called (or called with <= 0) means defaultA2AMaxDepth applies.
func (s *Server) SetA2AMaxDepth(depth int) {
	s.a2aMaxDepth = depth
}

func (s *Server) a2aMaxDepthOrDefault() int {
	if s.a2aMaxDepth <= 0 {
		return defaultA2AMaxDepth
	}
	return s.a2aMaxDepth
}

// SetA2AMaxBreadth configures the maximum number of direct children one
// parent run may create via A2A. Never called (or <= 0) → defaultA2AMaxBreadth.
func (s *Server) SetA2AMaxBreadth(breadth int) {
	s.a2aMaxBreadth = breadth
}

func (s *Server) a2aMaxBreadthOrDefault() int {
	if s.a2aMaxBreadth <= 0 {
		return defaultA2AMaxBreadth
	}
	return s.a2aMaxBreadth
}

// SetAdmissionLimits attaches occupancy/quota ceilings from
// admission_limits config. Nil or all-zero disables checks.
func (s *Server) SetAdmissionLimits(l *AdmissionLimits) {
	s.admissionLimits = l
}

// SetAliasResolver attaches A/B deployment routing (see alias.go).
// Called after NewServer when an "agent_aliases" config section is
// present; a never-set resolver means every agent_id passes through
// unchanged, same as before this feature existed.
func (s *Server) SetAliasResolver(r *AliasResolver) {
	s.aliases = r
}

// SetAdminSessions wires Admin UI httpOnly cookie session endpoints.
func (s *Server) SetAdminSessions(h *auth.AdminSessionHandlers) {
	s.adminSessions = h
}

// SetWSOriginPatterns configures WebSocket Origin checks for
// /threads/{id}/websocket. Pass the same list as cors.allow_origins.
// A nil/empty list leaves AcceptOptions nil (any Origin).
func (s *Server) SetWSOriginPatterns(origins []string) {
	s.wsOriginPatterns = origins
}

// SetHookDispatcher attaches an event-hook dispatcher to the server.
// Called after NewServer when webhooks (or other hook sinks) are configured.
func (s *Server) SetHookDispatcher(d *hooks.Dispatcher) {
	s.hooks = d
}

// SetCustomRoutesProxy attaches a reverse proxy for user-defined routes.
// mount is the external URL prefix (e.g. "/custom" or "/sales-assistant");
// empty means customroutes.DefaultMount. Called after NewServer when
// custom_routes is configured; nil proxy (the default) means the mount 404s.
//
// Mount is read on every request (same reason as the proxy field itself —
// Handler() is often built before SetCustomRoutesProxy in tests).
func (s *Server) SetCustomRoutesProxy(proxy http.Handler, mount string) {
	s.customProxy = proxy
	if mount == "" {
		mount = customroutes.DefaultMount
	}
	s.customMount = mount
}

// SetVectorStore attaches a vector/semantic store. Called after NewServer
// when vector_store is configured; a nil vectors field (the default) means
// /vectors/* responds 501 Not Implemented instead of silently 404ing --
// "this feature isn't turned on" is a different, more actionable signal
// than "this route doesn't exist".
func (s *Server) SetVectorStore(vs vectorstore.VectorStore) {
	s.vectors = vs
}

// enqueue pushes a job onto the queue. A nil assignment (an
// LLM-response-cache hit in createRun -- see tryServeCachedRun) is a
// no-op: the run already completed synchronously, there's nothing to
// dispatch to a runner. This is the single choke point all 8 of
// createRun's callers go through, so cache hits never need individual
// nil-checks scattered across every call site.
//
// Queue depth is NOT sampled here. For Redis, Queue.Len is a SCAN over
// the whole keyspace; calling it on every enqueue (and previously also
// on every GetJob) was the dominant Redis cost under load. The gauge is
// refreshed by cmd/serve.go's pollQueueDepth ticker instead.
func (s *Server) enqueue(ctx context.Context, assignment *transport.RunAssignment) error {
	if assignment == nil {
		return nil
	}
	return s.queue.Enqueue(ctx, assignment)
}

// Handler returns an http.Handler with all Agent Protocol routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Health -- see handleHealth/handleLivez/handleReadyz's own doc
	// comments for why there are three of these: /health is kept
	// exactly as it always was (backward compat), /livez is the same
	// cheap check under the Kubernetes-conventional name, /readyz is
	// the new one that actually verifies dependency connectivity.
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /livez", s.handleLivez)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// Agents (AP-001..007)
	mux.HandleFunc("POST /agents/search", s.handleSearchAgents)
	mux.HandleFunc("GET /agents/{agentID}", s.handleGetAgent)
	mux.HandleFunc("GET /agents/{agentID}/schemas", s.handleGetAgentSchemas)
	// Full agent versioning: version history browsing and rollback to
	// arbitrary past versions.
	mux.HandleFunc("GET /agents/{agentID}/versions", s.handleListAgentVersions)
	mux.HandleFunc("POST /agents/{agentID}/versions/{version}/rollback", s.handleRollbackAgent)

	// Agent marketplace / registry -- publish, discover, and deploy agent
	// definitions; see registry.go's package doc.
	mux.HandleFunc("PUT /registry/entries/{name}", s.handlePublishRegistryEntry)
	mux.HandleFunc("GET /registry/entries/{name}", s.handleGetRegistryEntry)
	mux.HandleFunc("DELETE /registry/entries/{name}", s.handleDeleteRegistryEntry)
	mux.HandleFunc("POST /registry/search", s.handleSearchRegistryEntries)
	mux.HandleFunc("GET /registry/entries/{name}/versions", s.handleListRegistryEntryVersions)
	mux.HandleFunc("GET /registry/entries/{name}/versions/{version}", s.handleGetRegistryEntryVersion)

	// LangGraph SDK compatibility: SDK calls /assistants/* not /agents/*
	// These return the SDK-expected response shape (assistant_id, graph_id, etc.)
	mux.HandleFunc("POST /assistants/search", s.handleSearchAssistants)
	mux.HandleFunc("GET /assistants/{agentID}", s.handleGetAssistant)
	mux.HandleFunc("GET /assistants/{agentID}/schemas", s.handleGetAssistantSchemas)

	// Threads (AP-010..026)
	mux.HandleFunc("POST /threads", s.handleCreateThread)
	mux.HandleFunc("GET /threads/{threadID}", s.handleGetThread)
	mux.HandleFunc("DELETE /threads/{threadID}", s.handleDeleteThread)
	mux.HandleFunc("PATCH /threads/{threadID}", s.handlePatchThread)
	mux.HandleFunc("POST /threads/search", s.handleSearchThreads)
	mux.HandleFunc("POST /threads/{threadID}/copy", s.handleCopyThread)
	mux.HandleFunc("GET /threads/{threadID}/state", s.handleGetThreadState)
	mux.HandleFunc("POST /threads/{threadID}/state", s.handleUpdateThreadState)
	mux.HandleFunc("GET /threads/{threadID}/history", s.handleGetThreadHistory)
	mux.HandleFunc("POST /threads/{threadID}/history", s.handleGetThreadHistory)

	// Thread Runs (AP-030..042, TS-001..012)
	mux.HandleFunc("POST /threads/{threadID}/runs", s.handleCreateThreadRun)
	mux.HandleFunc("POST /threads/{threadID}/runs/stream", s.handleCreateAndStreamThreadRun)
	mux.HandleFunc("POST /threads/{threadID}/runs/wait", s.handleCreateAndWaitThreadRun)
	mux.HandleFunc("GET /threads/{threadID}/runs", s.handleListThreadRuns)
	mux.HandleFunc("GET /threads/{threadID}/runs/{runID}", s.handleGetThreadRun)
	mux.HandleFunc("GET /threads/{threadID}/runs/{runID}/stream", s.handleStreamThreadRun)
	mux.HandleFunc("GET /threads/{threadID}/runs/{runID}/wait", s.handleWaitThreadRun)
	mux.HandleFunc("POST /threads/{threadID}/runs/{runID}/cancel", s.handleCancelThreadRun)
	mux.HandleFunc("DELETE /threads/{threadID}/runs/{runID}", s.handleDeleteThreadRun)

	// Background Runs (AP-030..042)
	mux.HandleFunc("POST /runs", s.handleCreateBackgroundRun)
	mux.HandleFunc("POST /runs/search", s.handleSearchRuns)
	mux.HandleFunc("POST /runs/stream", s.handleCreateAndStreamRun)
	mux.HandleFunc("POST /runs/wait", s.handleCreateAndWaitRun)
	mux.HandleFunc("GET /runs/{runID}", s.handleGetRun)
	mux.HandleFunc("DELETE /runs/{runID}", s.handleDeleteRun)
	mux.HandleFunc("GET /runs/{runID}/wait", s.handleWaitRun)
	mux.HandleFunc("GET /runs/{runID}/stream", s.handleStreamRun)
	mux.HandleFunc("POST /runs/{runID}/cancel", s.handleCancelRun)
	// A2A cost aggregation (see a2a.go) -- any run in a delegation tree
	// resolves to the same rollup across the whole tree.
	mux.HandleFunc("GET /runs/{runID}/cost", s.handleGetRunCost)

	// Store (AP-070..081) -- client-facing, client auth (API key/JWT)
	mux.HandleFunc("PUT /store/items", s.handlePutItem)
	mux.HandleFunc("GET /store/items", s.handleGetItem)
	mux.HandleFunc("DELETE /store/items", s.handleDeleteItem)
	mux.HandleFunc("POST /store/items/search", s.handleSearchItems)
	mux.HandleFunc("POST /store/namespaces", s.handleListNamespaces)

	// Vector Store (semantic search) -- client-facing, client auth (API
	// key/JWT). 501s if vector_store isn't configured.
	mux.HandleFunc("PUT /vectors/items", s.handleUpsertVectorItem)
	mux.HandleFunc("DELETE /vectors/items", s.handleDeleteVectorItem)
	mux.HandleFunc("POST /vectors/search", s.handleSearchVectors)

	// Streaming (AP-060..068)
	mux.HandleFunc("POST /threads/{threadID}/stream", s.handleOpenThreadStream)
	mux.HandleFunc("POST /threads/{threadID}/commands", s.handleThreadCommand)
	mux.HandleFunc("GET /threads/{threadID}/websocket", s.handleThreadWebSocket)

	// Internal APIs (for runners)
	mux.HandleFunc("GET /internal/runs/{runID}/status", s.handleGetRunStatus)
	mux.HandleFunc("PUT /internal/agents/{agentID}/schema", s.handleReportAgentSchema)

	// Connector Registry (internal, for runners)
	mux.HandleFunc("GET /internal/connectors", s.handleListConnectors)
	mux.HandleFunc("GET /internal/connectors/{name}", s.handleGetConnector)
	mux.HandleFunc("POST /internal/connectors/{name}/session", s.handleGetConnectorSession)
	mux.HandleFunc("POST /internal/connectors/{name}/mcp", s.handleProxyMCPRequest)
	mux.HandleFunc("GET /internal/cron", s.handleListCronSchedules)

	// Store, proxy mode -- same handlers as the client-facing /store/*
	// routes above, mounted again under /internal/ so non-Python (or
	// POSTGRES_DSN-less) runners can reach the store using their runner
	// token instead of a client credential they may not have.
	mux.HandleFunc("PUT /internal/store/items", s.handlePutItem)
	mux.HandleFunc("GET /internal/store/items", s.handleGetItem)
	mux.HandleFunc("DELETE /internal/store/items", s.handleDeleteItem)
	mux.HandleFunc("POST /internal/store/items/search", s.handleSearchItems)
	mux.HandleFunc("POST /internal/store/namespaces", s.handleListNamespaces)

	// Vector Store, proxy mode -- same dual-mode convention as Store above,
	// mounted again under /internal/ so a runner reaches it with its
	// runner token instead of a client credential it may not have.
	mux.HandleFunc("PUT /internal/vectors/items", s.handleUpsertVectorItem)
	mux.HandleFunc("DELETE /internal/vectors/items", s.handleDeleteVectorItem)
	mux.HandleFunc("POST /internal/vectors/search", s.handleSearchVectors)

	// Opaque checkpoint proxy (runner-protocol §6.2) -- framework-owned
	// blobs for ProxyCheckpointSaver when the runner has no POSTGRES_DSN.
	// Distinct from Agent Protocol /threads/{id}/history (ThreadState).
	mux.HandleFunc("PUT /internal/checkpoints/{threadID}/{checkpointID}", s.handlePutOpaqueCheckpoint)
	// /latest must be registered before /{checkpointID} so the literal path
	// is not captured as a checkpoint id.
	mux.HandleFunc("GET /internal/checkpoints/{threadID}/latest", s.handleGetLatestOpaqueCheckpoint)
	mux.HandleFunc("GET /internal/checkpoints/{threadID}/{checkpointID}", s.handleGetOpaqueCheckpoint)
	mux.HandleFunc("DELETE /internal/checkpoints/{threadID}/{checkpointID}", s.handleDeleteOpaqueCheckpoint)
	mux.HandleFunc("GET /internal/checkpoints/{threadID}", s.handleListOpaqueCheckpoints)

	// Webhook dead-letters (event hooks / webhook delivery) -- inspection,
	// not replay. A failed-after-all-retries delivery is persisted (see
	// internal/hooks.WebhookSink) instead of only logged and lost; this is
	// how an operator finds out about it.
	mux.HandleFunc("GET /internal/webhooks/dead-letters", s.handleListWebhookDeadLetters)

	// Agent-to-Agent (A2A) delegation -- see a2a.go's package doc
	// comment for the full design.
	mux.HandleFunc("POST /internal/a2a/runs", s.handleA2ACreateRun)

	// MCP-server support -- exposes Runkite's own agents AS MCP tools, the
	// reverse of the Connectors feature's MCP-client direction; see
	// mcpserver.go's own package doc comment. Client-facing (normal API
	// key/JWT auth applies, not the runner-token /internal/* auth), and
	// mounted without a method prefix since the Streamable HTTP transport
	// uses POST for RPCs, GET for its optional SSE stream, and DELETE to
	// close a session.
	mux.Handle("/mcp", s.mcpHTTPHandler())

	// Admin API. Gated on "admin" permission specifically, enforced in
	// auth.Middleware for the whole /admin-api/ prefix -- see this file's
	// package doc comment for scope.
	if s.adminSessions != nil {
		mux.HandleFunc("POST /admin-api/session", s.adminSessions.Create)
		mux.HandleFunc("GET /admin-api/session", s.adminSessions.Status)
		mux.HandleFunc("DELETE /admin-api/session", s.adminSessions.Delete)
	}
	mux.HandleFunc("GET /admin-api/overview", s.handleAdminOverview)
	mux.HandleFunc("GET /admin-api/agents", s.handleAdminListAgents)
	mux.HandleFunc("GET /admin-api/agents/{agentID}", s.handleAdminGetAgent)
	mux.HandleFunc("GET /admin-api/registry", s.handleAdminListRegistryEntries)
	mux.HandleFunc("GET /admin-api/registry/{name}", s.handleAdminGetRegistryEntry)
	mux.HandleFunc("GET /admin-api/registry/{name}/versions", s.handleAdminListRegistryEntryVersions)
	mux.HandleFunc("GET /admin-api/threads", s.handleAdminListThreads)
	mux.HandleFunc("GET /admin-api/threads/{threadID}", s.handleAdminGetThread)
	mux.HandleFunc("GET /admin-api/threads/{threadID}/runs", s.handleAdminListThreadRuns)
	mux.HandleFunc("GET /admin-api/runs", s.handleAdminListRuns)
	mux.HandleFunc("GET /admin-api/runs/{runID}", s.handleAdminGetRun)
	mux.HandleFunc("GET /admin-api/runs/{runID}/stream", s.handleAdminStreamRun)
	mux.HandleFunc("GET /admin-api/audit-events", s.handleAdminListAuditEvents)
	mux.HandleFunc("GET /admin-api/policy-grants", s.handleAdminListPolicyGrants)
	mux.HandleFunc("POST /admin-api/policy-grants", s.handleAdminCreatePolicyGrant)
	mux.HandleFunc("GET /admin-api/policy-grants/{id}", s.handleAdminGetPolicyGrant)
	mux.HandleFunc("PUT /admin-api/policy-grants/{id}", s.handleAdminUpdatePolicyGrant)
	mux.HandleFunc("DELETE /admin-api/policy-grants/{id}", s.handleAdminDeletePolicyGrant)
	mux.HandleFunc("GET /admin-api/mandatory-hitl", s.handleAdminListMandatoryHITL)
	mux.HandleFunc("POST /admin-api/mandatory-hitl", s.handleAdminCreateMandatoryHITL)
	mux.HandleFunc("GET /admin-api/mandatory-hitl/{id}", s.handleAdminGetMandatoryHITL)
	mux.HandleFunc("PUT /admin-api/mandatory-hitl/{id}", s.handleAdminUpdateMandatoryHITL)
	mux.HandleFunc("DELETE /admin-api/mandatory-hitl/{id}", s.handleAdminDeleteMandatoryHITL)
	mux.HandleFunc("GET /admin-api/pending-actions", s.handleAdminListPendingActions)
	mux.HandleFunc("GET /admin-api/pending-actions/{id}", s.handleAdminGetPendingAction)
	mux.HandleFunc("POST /admin-api/pending-actions/{id}/approve", s.handleAdminApprovePendingAction)
	mux.HandleFunc("POST /admin-api/pending-actions/{id}/deny", s.handleAdminDenyPendingAction)
	mux.HandleFunc("GET /admin-api/kill-switches", s.handleAdminListKillSwitches)
	mux.HandleFunc("POST /admin-api/kill-switches", s.handleAdminCreateKillSwitch)
	mux.HandleFunc("GET /admin-api/kill-switches/{id}", s.handleAdminGetKillSwitch)
	mux.HandleFunc("DELETE /admin-api/kill-switches/{id}", s.handleAdminDeleteKillSwitch)
	mux.HandleFunc("GET /admin-api/break-glass", s.handleAdminListBreakGlass)
	mux.HandleFunc("POST /admin-api/break-glass", s.handleAdminCreateBreakGlass)
	mux.HandleFunc("GET /admin-api/break-glass/{id}", s.handleAdminGetBreakGlass)
	mux.HandleFunc("DELETE /admin-api/break-glass/{id}", s.handleAdminDeleteBreakGlass)
	mux.HandleFunc("GET /admin-api/connectors", withSystemContext(s.handleListConnectors))
	mux.HandleFunc("GET /admin-api/connectors/{name}", withSystemContext(s.handleGetConnector))
	mux.HandleFunc("GET /admin-api/cron", withSystemContext(s.handleListCronSchedules))
	mux.HandleFunc("GET /admin-api/webhooks/dead-letters", withSystemContext(s.handleListWebhookDeadLetters))
	// Write actions -- the admin UI otherwise has none of its own;
	// cancel/delete reuse the exact client-facing handlers, scoped to every
	// tenant the same way every other /admin-api/* route already is;
	// redeliver is genuinely new (client-facing API has no equivalent).
	mux.HandleFunc("POST /admin-api/runs/{runID}/cancel", withSystemContext(s.handleCancelRun))
	mux.HandleFunc("DELETE /admin-api/threads/{threadID}", withSystemContext(s.handleDeleteThread))
	mux.HandleFunc("POST /admin-api/webhooks/dead-letters/{id}/redeliver", withSystemContext(s.handleRedeliverWebhook))

	// Admin UI -- the built React dashboard, embedded into the binary
	// (see internal/adminui). Public at the HTTP layer (see
	// auth.Middleware's /admin/ bypass); the dashboard's own login screen
	// is what actually gates access, via /admin-api/* requiring "admin".
	mux.Handle("GET /admin/", http.StripPrefix("/admin", adminui.Handler()))

	// Custom routes are dispatched in the wrapper below (not registered on
	// the mux) so the mount prefix can be configured after Handler() is
	// built — same dynamic-field pattern as hooks/rateLimit/connectors.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mount := s.customMount
		if mount == "" {
			mount = customroutes.DefaultMount
		}
		if s.customProxy != nil && customroutes.Matches(mount, r.URL.Path) {
			s.customProxy.ServeHTTP(w, r)
			return
		}
		// Unconfigured default mount still 404s with a clear message when
		// someone hits /custom/* with no custom_routes config.
		if s.customProxy == nil && customroutes.Matches(customroutes.DefaultMount, r.URL.Path) {
			writeError(w, http.StatusNotFound, "custom routes not configured")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// GET /internal/webhooks/dead-letters?limit=N
func (s *Server) handleListWebhookDeadLetters(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	dls, err := s.store.ListWebhookDeadLetters(r.Context(), limit)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if dls == nil {
		dls = []*models.WebhookDeadLetter{}
	}
	writeJSON(w, http.StatusOK, dls)
}

// StatusCallback returns a function the bridge server calls on ReportStatus.
func (s *Server) StatusCallback() func(runID, status, errorMsg string) {
	return func(runID, statusStr, errorMsg string) {
		status := models.RunStatus(statusStr)

		// ReportStatus arrives from the runner over gRPC keyed only by
		// runID -- no authenticated HTTP request to derive a tenant
		// context from. A regular tenant-filtered GetRun would never
		// find the run (it doesn't know which tenant to filter by yet),
		// so this first lookup uses a system context, then every
		// subsequent operation in this callback re-derives a REGULAR
		// (non-system) context scoped to the run's own tenant -- not
		// system bypass -- so the writes below (status, thread reset,
		// checkpoint, cache) are correctly tagged with that tenant
		// rather than silently landing in "default".
		run, err := s.store.GetRun(tenant.SystemContext(context.Background()), runID)
		if err != nil {
			slog.Error("status callback: failed to get run", "run_id", runID, "error", err)
			return
		}
		ctx := tenant.WithContext(context.Background(), run.TenantID)

		// Persist checkpoint + thread.values BEFORE advertising a terminal
		// run status. Clients that poll create+GET /runs until success then
		// immediately GET /threads/{id} must not observe success with empty
		// values -- that race was a real CI flake in the LangChain adapter
		// e2e (status flipped first; values write landed a moment later).
		// Still unconditional for every run (not only wait/stream handlers):
		// fire-and-forget create + poll is a first-class Agent Protocol path.
		var cachedVals map[string]interface{}
		if events, replayErr := s.broker.Replay(ctx, runID, 0); replayErr == nil {
			for i := len(events) - 1; i >= 0; i-- {
				if events[i].Method == "values" {
					var vals map[string]interface{}
					if json.Unmarshal(events[i].Data, &vals) == nil && len(vals) > 0 {
						cachedVals = vals
						s.saveRunCheckpoint(ctx, run.ThreadID, vals)
						if _, err := s.store.UpdateThread(ctx, run.ThreadID, &models.ThreadPatch{Values: vals}); err != nil {
							slog.Error("status callback: failed to update thread values", "thread_id", run.ThreadID, "error", err)
						}
					}
					break
				}
			}
		} else {
			slog.Error("status callback: failed to replay events for checkpoint", "run_id", runID, "error", replayErr)
		}

		if !tryStatusTransition("update_run_status", run.ThreadID, runID, func() error {
			return s.store.UpdateRunStatus(ctx, runID, status, nil, errorMsg)
		}) {
			return
		}

		if status == models.RunStatusSuccess && cachedVals != nil {
			s.maybeCacheRunResult(ctx, run, cachedVals)
		}

		// Record run completion metrics
		metrics.ActiveRuns.Dec()

		metrics.RunsTotal.WithLabelValues(run.AgentID, string(status)).Inc()
		metrics.RunDuration.WithLabelValues(run.AgentID).Observe(time.Since(run.CreatedAt).Seconds())
		// Only release the thread if no OTHER in-flight run owns it.
		// Atomic ReleaseThreadIfNoOtherActive closes the old check-then-act
		// race where SearchRuns + SetThreadStatus could idle a thread a
		// newer run had already claimed via TryClaimThread.
		threadStatus := models.ThreadStatusIdle
		if status == models.RunStatusInterrupted {
			threadStatus = models.ThreadStatusInterrupted
		}
		var released bool
		ok := tryStatusTransition("release_thread", run.ThreadID, runID, func() error {
			var err error
			released, err = s.store.ReleaseThreadIfNoOtherActive(ctx, run.ThreadID, runID, threadStatus)
			return err
		})
		if ok && !released {
			slog.Info("status callback: skipping thread release; another run is in-flight or thread already released",
				"thread_id", run.ThreadID, "completed_run_id", runID)
		}

		// Close the event broker so SSE/wait consumers know the run is done
		_ = s.broker.Close(runID)

		s.finishRun(runID, run.ThreadID, run.AgentID, run.TenantID, status, errorMsg)
	}
}

// finishRun performs terminal bookkeeping shared by StatusCallback
// (runner reported its own final status) and cancelRunCore / run-timeout
// (control plane forced a terminal status). Both are legitimate ways for
// a run to finish, and both can observe it independently (a cancelled
// run's runner may still ReportStatus afterwards).
//
// OTel: end the local span if createRun happened in this process
// (runSpans is process-local; a miss is normal under multi-replica LB).
//
// Webhooks: dispatch the terminal hook exactly once across replicas via
// TryClaimTerminalHook (shared DB). Do not gate on runSpans -- that
// dropped ~2/3 of run_complete deliveries in a 3-replica soak when
// ReportStatus hit a different replica than create. finishedRuns skips
// a redundant claim when cancel+status race on the same process. The
// claim write is skipped entirely when no sinks are registered
// (HasSinks) -- zero cost when webhooks are unconfigured. A claim-store
// error fail-opens (still dispatches) so a DB blip cannot silence every
// terminal webhook.
func (s *Server) finishRun(runID, threadID, agentID, tenantID string, status models.RunStatus, errorMsg string) {
	if v, ok := s.runSpans.LoadAndDelete(runID); ok {
		span := v.(trace.Span)
		span.SetAttributes(attribute.String("run.status", string(status)))
		// timeout is a forced control-plane failure (hung agent), same
		// observability class as error -- not a successful completion.
		if status == models.RunStatusError || status == models.RunStatusTimeout {
			span.SetStatus(codes.Error, errorMsg)
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}

	if _, already := s.finishedRuns.LoadOrStore(runID, struct{}{}); already {
		return
	}

	if !s.hooks.HasSinks() {
		return
	}

	won, err := s.store.TryClaimTerminalHook(context.Background(), runID)
	if err != nil {
		slog.Error("terminal webhook claim failed; dispatching anyway", "run_id", runID, "error", err)
	} else if !won {
		return
	}

	hookType := hooks.RunComplete
	switch status {
	case models.RunStatusError, models.RunStatusTimeout:
		hookType = hooks.Error
	case models.RunStatusInterrupted:
		hookType = hooks.Interrupt
	}
	s.hooks.Dispatch(hooks.Event{
		Type: hookType, RunID: runID, ThreadID: threadID, AgentID: agentID, TenantID: tenantID,
		Data:      map[string]interface{}{"status": string(status), "error": errorMsg},
		Timestamp: time.Now().UTC(),
	})
}

// maybeCacheRunResult saves a run's final values to the LLM response cache
// -- a configurable per-agent TTL with the cache key derived from a hash of
// the input -- if and only if createRun stashed a cache key in the run's
// metadata (set only when the agent has caching configured -- see
// createRun). Uses that
// stored key verbatim rather than recomputing it from run.Input/run.Config:
// those come back from the DB post-JSONB-round-trip (Postgres reformats
// JSON on write), which would silently produce a different hash than the
// one computed from the original raw request bytes at lookup time -- a
// real bug found via live testing, not a hypothetical.
func (s *Server) maybeCacheRunResult(ctx context.Context, run *models.Run, vals map[string]interface{}) {
	cacheKey, ok := run.Metadata["cache_key"].(string)
	if !ok || cacheKey == "" {
		return
	}
	agent, err := s.store.GetAgent(ctx, run.AgentID)
	if err != nil || agent == nil {
		return
	}
	ttlSeconds := cacheTTLSeconds(agent.Metadata)
	if ttlSeconds <= 0 {
		return
	}
	now := time.Now().UTC()
	err = s.store.SaveCachedRunResult(ctx, &models.CachedRunResult{
		CacheKey:  cacheKey,
		AgentID:   run.AgentID,
		Output:    vals,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Duration(ttlSeconds) * time.Second),
	})
	if err != nil {
		slog.Error("failed to save run result to cache", "run_id", run.RunID, "agent_id", run.AgentID, "error", err)
	}
}

// saveRunCheckpoint persists a checkpoint with the given values for a thread.
// Called from StatusCallback on every terminal run.
func (s *Server) saveRunCheckpoint(ctx context.Context, threadID string, values map[string]interface{}) {
	// Get the latest checkpoint to use as parent
	var parentCP *models.ThreadCheckpoint
	latest, err := s.store.GetLatestCheckpoint(ctx, threadID)
	if err == nil {
		cp := latest.Checkpoint
		parentCP = &cp
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	cpID := uuid.New().String()
	ts := &models.ThreadState{
		Values: values,
		Next:   []string{},
		Checkpoint: models.ThreadCheckpoint{
			ThreadID:     threadID,
			CheckpointNS: "",
			CheckpointID: cpID,
		},
		Metadata:         map[string]interface{}{},
		CreatedAt:        &now,
		ParentCheckpoint: parentCP,
		Tasks:            []interface{}{},
		Interrupts:       []interface{}{},
	}

	if err := s.store.SaveCheckpoint(ctx, threadID, ts); err != nil {
		slog.Error("failed to save run checkpoint", "thread_id", threadID, "error", err)
	}
}

// --- Connector handlers ---

// GET /internal/connectors — list all registered connectors (names + types, no secrets)
func (s *Server) handleListConnectors(w http.ResponseWriter, r *http.Request) {
	if s.connectors == nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}
	type connectorInfo struct {
		Name           string `json:"name"`
		Type           string `json:"type"`
		MCP            string `json:"mcp,omitempty"`
		CircuitBreaker string `json:"circuit_breaker_state,omitempty"`
	}
	names := s.connectors.List()
	result := make([]connectorInfo, 0, len(names))
	for _, name := range names {
		c, _ := s.connectors.Get(name)
		info := connectorInfo{Name: name, Type: c.Config.Auth.Type, CircuitBreaker: s.connectors.BreakerState(name)}
		// Same fix, same reason as handleGetConnector -- the raw
		// downstream MCP URL was leaking through this endpoint too
		// (found on review). Missed the
		// first time because only the single-connector GET had a test
		// asserting on the MCP field; the list endpoint's own test only
		// checked name/type. Shows the proxy path instead.
		if c.Config.MCP != nil {
			info.MCP = "/internal/connectors/" + name + "/mcp"
		}
		result = append(result, info)
	}
	writeJSON(w, http.StatusOK, result)
}

// GET /internal/connectors/{name} — get connector info (config sans secrets)
func (s *Server) handleGetConnector(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.connectors == nil {
		writeError(w, http.StatusNotFound, "connector not found: "+name)
		return
	}
	c, err := s.connectors.Get(name)
	if err != nil {
		writeError(w, http.StatusNotFound, "connector not found: "+name)
		return
	}
	// Return safe info — no secrets
	type safeConfig struct {
		Name           string            `json:"name"`
		Type           string            `json:"type"`
		MCP            string            `json:"mcp,omitempty"`
		Errors         map[string]string `json:"errors,omitempty"`
		Tools          interface{}       `json:"tools,omitempty"`
		CircuitBreaker string            `json:"circuit_breaker_state,omitempty"`
	}
	resp := safeConfig{
		Name:           name,
		Type:           c.Config.Auth.Type,
		Errors:         c.Config.Errors,
		Tools:          c.Config.Tools,
		CircuitBreaker: s.connectors.BreakerState(name),
	}
	// The raw downstream MCP URL is deliberately NOT exposed here --
	// this endpoint is reachable with just a runner token, same as
	// GetSession (see registry.go's GetSession doc comment), so leaking
	// it here would let a misbehaving or compromised agent discover and
	// connect to the real server directly, bypassing the proxy's tool
	// allow/deny enforcement the same way a leaked raw credential would.
	// Shows the proxy path instead -- the actually-correct place to
	// connect to, and consistent with what GetSession itself hands out.
	if c.Config.MCP != nil {
		resp.MCP = "/internal/connectors/" + name + "/mcp"
	}
	writeJSON(w, http.StatusOK, resp)
}

// GET /internal/cron — list configured cron schedules, for operators to
// confirm what actually loaded from langgraph.json without querying the
// database directly (same motivation as GET /internal/connectors). System
// context: /internal/* is runner-token/operator-authenticated, not
// client-tenant-authenticated, so there's no single caller tenant to
// scope to here -- this intentionally shows every tenant's schedules.
func (s *Server) handleListCronSchedules(w http.ResponseWriter, r *http.Request) {
	schedules, err := s.store.ListCronSchedules(tenant.SystemContext(r.Context()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, schedules)
}

// POST /internal/connectors/{name}/session — get a pre-authenticated session
func (s *Server) handleGetConnectorSession(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.connectors == nil {
		writeError(w, http.StatusNotFound, "connector not found: "+name)
		return
	}

	if dec, deny := s.checkConnectorPolicy(r.Context(), policy.StageConnectorSession, name, ""); deny {
		s.emitToolAuthEvent(r.Context(), policy.StageConnectorSession, name, "", dec, "")
		writeJSON(w, http.StatusForbidden, policyDenyJSON(dec))
		return
	}

	var body struct {
		UserContext map[string]interface{} `json:"user_context"`
	}
	if err := readJSON(r, &body); err != nil {
		// Allow empty body
		body.UserContext = nil
	}

	sess, err := s.connectors.GetSession(r.Context(), name, body.UserContext)
	if err != nil {
		if errors.Is(err, connector.ErrNotFound) {
			writeError(w, http.StatusNotFound, "connector not found: "+name)
			return
		}
		var circuitOpen *connector.ErrCircuitOpen
		if errors.As(err, &circuitOpen) {
			// 503 + Retry-After, not 502/500: this is a deliberate,
			// fast-fail rejection (the breaker didn't even attempt the
			// network call), not an actual request failure -- distinct
			// enough to warrant its own status rather than folding into
			// the generic BadGateway case below.
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		slog.Error("connector session failed", "connector", name, "error", err)
		// Check the connector's error taxonomy for a user-facing message
		msg := "failed to create connector session"
		if c, cErr := s.connectors.Get(name); cErr == nil && len(c.Config.Errors) > 0 {
			errStr := err.Error()
			for code, userMsg := range c.Config.Errors {
				if strings.Contains(errStr, code) {
					msg = userMsg
					break
				}
			}
		}
		writeError(w, http.StatusBadGateway, msg)
		return
	}

	// MCP connectors get a short-lived capability token bound to this
	// run. Pre-warm (createRun) calls Registry.GetSession directly and
	// does not mint — runners mint at use time via this handler.
	if sess.MCP != nil {
		if s.connectorSessions == nil {
			writeError(w, http.StatusServiceUnavailable, "connector session store not configured")
			return
		}
		binding := auth.RunBindingFromContext(r.Context())
		if binding == nil {
			writeError(w, http.StatusUnauthorized, "run binding required for MCP connector session")
			return
		}
		cap, err := s.connectorSessions.Create(name, binding.RunID, binding.Generation, binding.TenantID, binding.AgentID)
		if err != nil {
			slog.Error("connector session mint failed", "connector", name, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to mint connector session token")
			return
		}
		sess.SessionToken = cap.Token
		sess.ExpiresAt = cap.Expires.UTC().Format(time.RFC3339)
	}

	writeJSON(w, http.StatusOK, sess)
}

// POST /internal/connectors/{name}/mcp — proxy one JSON-RPC request to a
// connector's downstream MCP server, enforcing its tool allow/deny list
// (see internal/connector/mcpproxy.go's doc comment for the full
// rationale: this is what makes that filter a real gate instead of an
// advisory hint the agent's own MCP client could just ignore). The
// request body is the raw JSON-RPC message, forwarded (or rejected)
// as-is -- not wrapped the way /session's body is, since altering the
// JSON-RPC envelope would break any standard MCP client sending it.
// User context for oauth2_token_exchange connectors (which need a fresh
// per-call token, not a cached one -- see getToken's own doc comment)
// travels via a header instead of the body for the same reason.
func (s *Server) handleProxyMCPRequest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.connectors == nil {
		writeError(w, http.StatusNotFound, "connector not found: "+name)
		return
	}
	if _, err := s.connectors.Get(name); err != nil {
		writeError(w, http.StatusNotFound, "connector not found: "+name)
		return
	}
	if err := s.requireConnectorSession(w, r, name); err != nil {
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var userCtx map[string]interface{}
	if raw := r.Header.Get("X-Runkite-User-Context"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &userCtx)
	}

	// Policy on tools/call only (other MCP methods stay transparent after
	// the connector's own static tool filter inside ProxyMCPRequest).
	// One-shot capability (Admin-approved pending) is checked before Decide
	// so a cached or re-pending webhook cannot block the approved retry.
	if method, tool := extractToolsCallName(body); method == "tools/call" {
		if !s.tryConsumePendingCapability(r.Context(), name, tool) {
			if dec, deny := s.checkConnectorPolicy(r.Context(), policy.StageToolCall, name, tool); deny {
				actionID := ""
				if dec.Effect == policy.EffectPending {
					id, err := s.persistPendingAction(r.Context(), name, tool, dec)
					if err != nil {
						slog.Warn("policy: persist pending action failed", "error", err, "connector", name, "tool", tool)
						dec.Effect = policy.EffectDeny
						if dec.Reason == "" {
							dec.Reason = "pending approval unavailable"
						}
						if dec.ReasonCode == "" {
							dec.ReasonCode = policy.ReasonPolicyPending
						}
					} else {
						actionID = id
						if dec.ReasonCode == "" {
							dec.ReasonCode = policy.ReasonPolicyPending
						}
					}
				}
				s.emitToolAuthEvent(r.Context(), policy.StageToolCall, name, tool, dec, actionID)
				msg := dec.Reason
				if msg == "" {
					if dec.Effect == policy.EffectPending {
						msg = "awaiting policy approval"
					} else {
						msg = "denied by policy"
					}
				}
				denied, err := connector.DeniedRPCResult(extractJSONRPCID(body), msg, mcpPolicyDenyData(dec, actionID))
				if err != nil {
					writeError(w, http.StatusInternalServerError, "failed to build policy deny response")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(denied.StatusCode)
				_, _ = w.Write(denied.Body)
				return
			}
		}
	}

	result, err := s.connectors.ProxyMCPRequest(r.Context(), name, userCtx, body)
	if err != nil {
		if errors.Is(err, connector.ErrNotFound) {
			writeError(w, http.StatusNotFound, "connector not found: "+name)
			return
		}
		var circuitOpen *connector.ErrCircuitOpen
		if errors.As(err, &circuitOpen) {
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		slog.Error("mcp proxy request failed", "connector", name, "error", err)
		msg := "failed to proxy mcp request"
		if c, cErr := s.connectors.Get(name); cErr == nil && len(c.Config.Errors) > 0 {
			errStr := err.Error()
			for code, userMsg := range c.Config.Errors {
				if strings.Contains(errStr, code) {
					msg = userMsg
					break
				}
			}
		}
		writeError(w, http.StatusBadGateway, msg)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(result.StatusCode)
	w.Write(result.Body)
}

// requireConnectorSession checks the short-lived MCP capability token.
// Returns a non-nil error after writing the HTTP response on failure.
func (s *Server) requireConnectorSession(w http.ResponseWriter, r *http.Request, connectorName string) error {
	if s.connectorSessions == nil {
		writeError(w, http.StatusServiceUnavailable, "connector session store not configured")
		return errConnectorSession
	}
	token := strings.TrimSpace(r.Header.Get(connector.HeaderConnectorSession))
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing "+connector.HeaderConnectorSession)
		return errConnectorSession
	}
	sess := s.connectorSessions.Get(token)
	if sess == nil {
		writeError(w, http.StatusUnauthorized, "invalid or expired connector session token")
		return errConnectorSession
	}
	binding := auth.RunBindingFromContext(r.Context())
	if binding == nil {
		writeError(w, http.StatusUnauthorized, "run binding required")
		return errConnectorSession
	}
	if sess.Connector != connectorName || sess.RunID != binding.RunID || sess.Generation != binding.Generation {
		writeError(w, http.StatusForbidden, "connector session token does not match this run")
		return errConnectorSession
	}
	return nil
}

// errConnectorSession marks that requireConnectorSession already wrote the response.
var errConnectorSession = errors.New("connector session denied")

// --- Shared helpers ---

// handleHealth predates the /livez + /readyz split below and is kept
// exactly as-is (unconditional 200, no dependency checks) for backward
// compatibility with anything already pointed at it -- notably every
// existing Docker Compose file's own healthcheck before this change.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleLivez answers "is this process alive at all" -- deliberately as
// cheap as handleHealth (no dependency checks): a liveness probe's job
// is to catch a genuinely wedged process (deadlock, infinite loop) so an
// orchestrator can restart it, and checking a downstream DB/queue here
// would make a transient dependency outage look like THIS process is
// broken, triggering a pointless restart-crash-loop that fixes nothing
// (restarting the control plane doesn't bring Postgres back). That
// distinction is exactly what /readyz below exists for instead.
func (s *Server) handleLivez(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// pinger is implemented by transport.EventBroker/CancelBroker only
// optionally (the interfaces themselves don't require it -- see
// redis.Broker.Ping's doc comment for why only Redis's implementations
// need their own check).
type pinger interface {
	Ping(ctx context.Context) error
}

// handleReadyz answers "can this replica actually serve a request right
// now" by round-tripping every backend dependency it has -- the
// distinction a load balancer needs to stop routing traffic to a
// replica whose Postgres/Redis/Kafka has become unreachable, even
// though the process itself (per /livez) is still perfectly alive.
// Bounded to 3s total so one hung dependency fails this probe fast
// instead of hanging until the caller's own probe timeout.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := map[string]string{}
	ready := true

	if err := s.store.Ping(ctx); err != nil {
		checks["store"] = err.Error()
		ready = false
	} else {
		checks["store"] = "ok"
	}

	if err := s.queue.Ping(ctx); err != nil {
		checks["queue"] = err.Error()
		ready = false
	} else {
		checks["queue"] = "ok"
	}

	// Broker/cancel bus: only checked if this backend combination
	// actually gives them a connection distinct from the queue's own
	// (see pinger's doc comment) -- e.g. in-process and NATS have
	// nothing extra to check here, since Ping above already covers
	// their one shared connection.
	if p, ok := s.broker.(pinger); ok {
		if err := p.Ping(ctx); err != nil {
			checks["event_broker"] = err.Error()
			ready = false
		} else {
			checks["event_broker"] = "ok"
		}
	}
	if p, ok := s.cancel.(pinger); ok {
		if err := p.Ping(ctx); err != nil {
			checks["cancel_broker"] = err.Error()
			ready = false
		} else {
			checks["cancel_broker"] = "ok"
		}
	}

	status := http.StatusOK
	statusText := "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		statusText = "not ready"
	}
	writeJSON(w, status, map[string]interface{}{"status": statusText, "checks": checks})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, models.ErrorResponse{Message: msg})
}

// maxJSONBodyBytes caps request bodies decoded by readJSON. Without a
// bound, a single POST /runs/search (or any JSON handler) can pin
// unbounded memory in the decoder -- the opposite of the scale goal.
const maxJSONBodyBytes = 1 << 20 // 1 MiB

// maxSearchLimit caps Limit on search endpoints. Stores historically
// treated Limit<=0 as "no limit" / a huge default; a client sending
// {"limit":1000000} would scan and marshal the world.
const maxSearchLimit = 100

func clampSearchLimit(limit, defaultLimit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	if limit > maxSearchLimit {
		return maxSearchLimit
	}
	return limit
}

// readJSON decodes exactly one JSON value from the request body and
// rejects anything left over afterward (a second JSON value, or
// arbitrary trailing bytes) -- a single Decode call on its own silently
// accepts and ignores trailing data, which could otherwise hide a
// client bug (or smuggle a second, unparsed payload past whatever
// logging/validation only looks at the first decoded value).
func readJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxJSONBodyBytes))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("request body must contain exactly one JSON value, found trailing data")
	}
	return nil
}

// ErrA2ADepthExceeded means creating this run would exceed the
// configured Agent-to-Agent delegation chain limit (see A2AEntry.MaxDepth
// in internal/config/loader.go) -- a client-actionable 400, not a server
// error: the caller's delegation chain is too deep, nothing is broken.
type ErrA2ADepthExceeded struct {
	Depth    int
	MaxDepth int
}

func (e *ErrA2ADepthExceeded) Error() string {
	return fmt.Sprintf("a2a delegation depth %d exceeds max_depth %d", e.Depth, e.MaxDepth)
}

// ErrA2ABreadthExceeded means the parent already has max_breadth direct
// children -- client-actionable 400 (fan-out cap), not a server fault.
type ErrA2ABreadthExceeded struct {
	ParentRunID string
	Breadth     int
	MaxBreadth  int
}

func (e *ErrA2ABreadthExceeded) Error() string {
	return fmt.Sprintf("a2a parent %s already has %d direct children (max_breadth %d)", e.ParentRunID, e.Breadth, e.MaxBreadth)
}

func handleStoreError(w http.ResponseWriter, err error) {
	var notFound *state.ErrNotFound
	var conflict *state.ErrConflict
	var rateLimited *ratelimit.ErrRateLimited
	var depthExceeded *ErrA2ADepthExceeded
	var breadthExceeded *ErrA2ABreadthExceeded
	var admissionExceeded *state.ErrAdmissionLimitExceeded
	switch {
	case errors.As(err, &notFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.As(err, &conflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.As(err, &rateLimited):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.As(err, &admissionExceeded):
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfterAdmission(admissionExceeded, time.Now())))
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.As(err, &depthExceeded):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.As(err, &breadthExceeded):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		var denied *hooks.ErrDenied
		if errors.As(err, &denied) {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		slog.Error("store error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
