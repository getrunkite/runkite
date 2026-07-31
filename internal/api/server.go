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

	"github.com/sharanharsoor/runkite/internal/adminui"
	"github.com/sharanharsoor/runkite/internal/connector"
	"github.com/sharanharsoor/runkite/internal/hooks"
	"github.com/sharanharsoor/runkite/internal/metrics"
	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/ratelimit"
	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/tenant"
	"github.com/sharanharsoor/runkite/internal/transport"
	"github.com/sharanharsoor/runkite/internal/vectorstore"
)

// Server is the HTTP API server for the Agent Protocol.
type Server struct {
	store       state.Store
	queue       transport.JobQueue
	broker      transport.EventBroker
	cancel      transport.CancelBroker
	connectors  *connector.Registry     // nil if no connectors configured
	rateLimit   *ratelimit.Limiter      // nil-safe: nil behaves as disabled
	hooks       *hooks.Dispatcher       // nil-safe: nil Dispatch/HasSinks are no-ops
	customProxy http.Handler            // nil if no custom_routes configured; mounted at /custom/
	vectors     vectorstore.VectorStore // nil if no vector_store configured; /vectors/* 501s
	a2aMaxDepth int                     // 0 means "use the default" -- see SetA2AMaxDepth
	aliases     *AliasResolver          // nil-safe: nil Resolve is a pass-through

	// runSpans holds the in-flight OTel span for each run, from createRun
	// until StatusCallback closes it out. ponytail: if the control plane
	// restarts mid-run, that run's span is never End()'d and is dropped
	// with the process -- a missing trace segment, not corrupted data.
	// Deliberately still process-local (unlike the job queue's in-flight
	// tracking, moved into Redis itself -- see internal/transport/redis's
	// Queue doc comment -- after being found to cause actual duplicate/
	// lost job execution): losing a trace segment on crash is a much
	// lower-severity gap than losing or duplicating a run, so this one is
	// an accepted trade-off, not an oversight. No cleanup goroutine
	// needed: every run that reaches a terminal status (including via the
	// reclaim/redelivery path) removes its own entry.
	runSpans sync.Map // run_id -> trace.Span
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

// SetAliasResolver attaches A/B deployment routing (see alias.go).
// Called after NewServer when an "agent_aliases" config section is
// present; a never-set resolver means every agent_id passes through
// unchanged, same as before this feature existed.
func (s *Server) SetAliasResolver(r *AliasResolver) {
	s.aliases = r
}

// SetHookDispatcher attaches an event-hook dispatcher to the server.
// Called after NewServer when webhooks (or other hook sinks) are configured.
func (s *Server) SetHookDispatcher(d *hooks.Dispatcher) {
	s.hooks = d
}

// SetCustomRoutesProxy attaches a reverse proxy to be mounted at /custom/*,
// the platform's user-defined-routes feature. Called after NewServer when
// custom_routes is configured; nil (the default) means /custom/* 404s.
func (s *Server) SetCustomRoutesProxy(proxy http.Handler) {
	s.customProxy = proxy
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

	// Health
	mux.HandleFunc("GET /health", s.handleHealth)

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

	// Custom routes -- user-defined HTTP endpoints mounted alongside the
	// Agent Protocol API. In-runner mode
	// (the Python runner SDK hosts the user's ASGI app) and sidecar mode
	// (a separately-run, language-agnostic process) are the exact same
	// mechanism from here: a reverse proxy to a configured URL. Mounted
	// inside the same client auth as the rest of the API by default (see
	// cmd/serve.go's middleware chain) -- the user's app can layer its own
	// auth on top if it needs something different.
	//
	// Registered unconditionally (unlike a plain nil-check at Handler()
	// build time) because SetCustomRoutesProxy is always called *after*
	// Handler() has already been built and handed to the HTTP server (see
	// cmd/serve.go and every test's newTestEnv) -- net/http.ServeMux has no
	// way to add a route after the fact, so the proxy field must be read
	// fresh on every request instead of baked into a one-time routing
	// decision, the same way s.hooks/s.rateLimit/s.connectors already are.
	mux.HandleFunc("/custom/", s.handleCustomRoute)

	return mux
}

func (s *Server) handleCustomRoute(w http.ResponseWriter, r *http.Request) {
	if s.customProxy == nil {
		writeError(w, http.StatusNotFound, "custom routes not configured")
		return
	}
	s.customProxy.ServeHTTP(w, r)
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

		if err := s.store.UpdateRunStatus(ctx, runID, status, nil, errorMsg); err != nil {
			slog.Error("status callback: failed to update run status", "run_id", runID, "error", err)
			return
		}

		// Record run completion metrics
		metrics.ActiveRuns.Dec()

		metrics.RunsTotal.WithLabelValues(run.AgentID, string(status)).Inc()
		metrics.RunDuration.WithLabelValues(run.AgentID).Observe(time.Since(run.CreatedAt).Seconds())
		// Only release the thread if no OTHER in-flight run owns it.
		// /wait may already have set idle and a subsequent create claimed
		// busy for a newer run; a late ReportStatus for THIS (older) run
		// must not clobber that -- otherwise two runs can execute on the
		// same thread (the new one thinks it holds the claim while we
		// silently flip the thread back to idle).
		if !threadHasOtherActiveRun(ctx, s.store, run.ThreadID, runID) {
			threadStatus := models.ThreadStatusIdle
			if status == models.RunStatusInterrupted {
				threadStatus = models.ThreadStatusInterrupted
			}
			if err := s.store.SetThreadStatus(ctx, run.ThreadID, threadStatus); err != nil {
				slog.Error("status callback: failed to reset thread status", "thread_id", run.ThreadID, "error", err)
			}
		} else {
			slog.Info("status callback: skipping thread release; another run is in-flight",
				"thread_id", run.ThreadID, "completed_run_id", runID)
		}

		// Persist a checkpoint from the run's last "values" event and update
		// thread.values. This must happen here -- unconditionally, for every
		// run -- rather than only inside the HTTP wait/stream handlers, so
		// that checkpoint history and GET /threads/{id}/state work
		// regardless of how the client observed the run (fire-and-forget
		// create + poll is a first-class Agent Protocol pattern, not just
		// create-and-wait/create-and-stream).
		if events, replayErr := s.broker.Replay(ctx, runID, 0); replayErr == nil {
			for i := len(events) - 1; i >= 0; i-- {
				if events[i].Method == "values" {
					var vals map[string]interface{}
					if json.Unmarshal(events[i].Data, &vals) == nil && len(vals) > 0 {
						s.saveRunCheckpoint(ctx, run.ThreadID, vals)
						if _, err := s.store.UpdateThread(ctx, run.ThreadID, &models.ThreadPatch{Values: vals}); err != nil {
							slog.Error("status callback: failed to update thread values", "thread_id", run.ThreadID, "error", err)
						}
						if status == models.RunStatusSuccess {
							s.maybeCacheRunResult(ctx, run, vals)
						}
					}
					break
				}
			}
		} else {
			slog.Error("status callback: failed to replay events for checkpoint", "run_id", runID, "error", replayErr)
		}

		// Close the event broker so SSE/wait consumers know the run is done
		_ = s.broker.Close(runID)

		s.finishRun(runID, run.ThreadID, run.AgentID, status, errorMsg)
	}
}

// threadHasOtherActiveRun reports whether threadID has a pending/running
// run other than excludeRunID. Used by StatusCallback so a late
// ReportStatus for a completed run cannot release a thread that a newer
// run has already claimed (see /wait's early SetThreadStatus + fast
// follow-up create).
//
// ponytail: this check-then-act (SearchRuns, then a separate
// SetThreadStatus write) is not atomic -- structurally the same class of
// race TryClaimThread's own doc comment calls out ("checking status
// first and writing busy second... is a TOCTOU race, confirmed
// empirically"), just with a far narrower window: a third create-run
// request would need to land in the few-millisecond gap between this
// query and that write. Closing it fully would need a single atomic
// conditional store operation (UPDATE ... WHERE status='busy' AND NOT
// EXISTS (other active run), mirroring TryClaimThread's own atomic
// UPDATE) implemented across all four backends plus conformance
// coverage -- real effort for a window this narrow doesn't currently
// justify. Upgrade path if this ever needs closing: add that as a new
// state.Store method instead of two separate calls.
func threadHasOtherActiveRun(ctx context.Context, store state.Store, threadID, excludeRunID string) bool {
	// Filter by status -- an unfiltered newest-N search hides older
	// pending/running runs behind a flood of cache-hit success rows on a
	// hot thread (real bug: StatusCallback then idled a busy thread).
	for _, st := range []models.RunStatus{models.RunStatusPending, models.RunStatusRunning} {
		st := st
		runs, err := store.SearchRuns(ctx, &models.RunSearchRequest{
			ThreadID: threadID,
			Status:   &st,
			Limit:    5,
		})
		if err != nil {
			// Fail closed: skip the release rather than risk clobbering a
			// newer claim when we can't tell.
			return true
		}
		for _, r := range runs {
			if r.RunID != excludeRunID {
				return true
			}
		}
	}
	return false
}

// finishRun performs the once-per-run terminal bookkeeping shared by
// StatusCallback (runner reported its own final status) and cancelRunCore
// (control plane forced a cancellation) -- both are legitimate ways for a
// run to reach a terminal state, and both can observe it independently
// (a cancelled run's runner may still report its own final status
// afterwards). s.runSpans.LoadAndDelete is the single idempotency guard
// for the whole function: whichever caller arrives first does the work
// (ending the OTel span + dispatching exactly one on_run_complete/
// on_error/on_interrupt hook); the second arrival finds nothing and no-ops.
// Without this, a cancelled-then-later-self-reported run would fire its
// terminal webhook twice.
func (s *Server) finishRun(runID, threadID, agentID string, status models.RunStatus, errorMsg string) {
	v, ok := s.runSpans.LoadAndDelete(runID)
	if !ok {
		return
	}
	span := v.(trace.Span)
	span.SetAttributes(attribute.String("run.status", string(status)))
	if status == models.RunStatusError {
		span.SetStatus(codes.Error, errorMsg)
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()

	hookType := hooks.RunComplete
	switch status {
	case models.RunStatusError:
		hookType = hooks.Error
	case models.RunStatusInterrupted:
		hookType = hooks.Interrupt
	}
	s.hooks.Dispatch(hooks.Event{
		Type: hookType, RunID: runID, ThreadID: threadID, AgentID: agentID,
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

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var userCtx map[string]interface{}
	if raw := r.Header.Get("X-Runkite-User-Context"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &userCtx)
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

// --- Shared helpers ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

func handleStoreError(w http.ResponseWriter, err error) {
	var notFound *state.ErrNotFound
	var conflict *state.ErrConflict
	var rateLimited *ratelimit.ErrRateLimited
	var depthExceeded *ErrA2ADepthExceeded
	switch {
	case errors.As(err, &notFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.As(err, &conflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.As(err, &rateLimited):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.As(err, &depthExceeded):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		slog.Error("store error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
