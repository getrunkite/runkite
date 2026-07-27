package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/sharanharsoor/runkite/internal/auth"
	"github.com/sharanharsoor/runkite/internal/hooks"
	"github.com/sharanharsoor/runkite/internal/metrics"
	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/ratelimit"
	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/tenant"
	"github.com/sharanharsoor/runkite/internal/tracing"
	"github.com/sharanharsoor/runkite/internal/transport"
)

// --- Shared run creation logic ---

func (s *Server) createRun(r *http.Request, threadID string, req *models.RunCreate) (*models.Run, *transport.RunAssignment, error) {
	return s.createRunCtx(r.Context(), threadID, req)
}

// createRunCtx is the context-based core of run creation, shared by every
// HTTP call site (via createRun, above) and the cron scheduler (which has
// no *http.Request to derive a context from -- see internal's cron loop in
// cmd/serve.go).
func (s *Server) createRunCtx(ctx context.Context, threadID string, req *models.RunCreate) (*models.Run, *transport.RunAssignment, error) {
	now := time.Now().UTC()
	runID := uuid.New().String()

	// SDK compat: accept assistant_id as alias for agent_id
	if req.AgentID == "" && req.AssistantID != "" {
		req.AgentID = req.AssistantID
	}

	// Per-agent rate limiting (master plan: "Rate limiting: per-user,
	// per-agent, per-tenant"). Enforced here rather than in generic HTTP
	// middleware because agent_id is parsed from the request body, not the
	// URL -- this is the single choke point every run-creation path (REST,
	// WebSocket, streaming commands) already goes through, so there's
	// nowhere for a caller to bypass it. Global/per-user dimensions are a
	// separate, cheaper HTTP middleware layer (ratelimit.Middleware) that
	// doesn't need body access.
	if !s.rateLimit.AllowAgent(req.AgentID) {
		return nil, nil, &ratelimit.ErrRateLimited{Scope: "agent"}
	}

	// SDK compat: accept "command":{"resume":...} as alias for resume_command
	if req.ResumeCommand == nil && req.Command != nil {
		var cmd map[string]json.RawMessage
		if json.Unmarshal(req.Command, &cmd) == nil {
			if resume, ok := cmd["resume"]; ok {
				// Wrap in the shape the runner expects: {"response": <value>}
				wrapped, _ := json.Marshal(map[string]json.RawMessage{"response": resume})
				req.ResumeCommand = wrapped
			}
		}
	}

	// Ensure thread exists (create if requested)
	_, err := s.store.GetThread(ctx, threadID)
	if err != nil {
		ifNotExists := req.IfNotExists
		if ifNotExists == "" {
			ifNotExists = "create"
		}
		if ifNotExists == "create" {
			thread := &models.Thread{
				ThreadID:  threadID,
				Status:    models.ThreadStatusIdle,
				Metadata:  map[string]interface{}{},
				CreatedAt: now,
				UpdatedAt: now,
			}
			if createErr := s.store.CreateThread(ctx, thread); createErr != nil {
				// Thread might have been created concurrently, try to get it
				if _, getErr := s.store.GetThread(ctx, threadID); getErr != nil {
					return nil, nil, fmt.Errorf("create thread: %w", createErr)
				}
			}
		} else {
			return nil, nil, err
		}
	}

	// LLM response caching (master plan: "configurable TTL, cache key
	// derivation from input hash"). A resume is inherently continuing a
	// specific prior execution, never a fresh cacheable computation, so
	// it's excluded regardless of the agent's cache config. A hit
	// short-circuits entirely: no thread claim, no RunAssignment, no
	// runner dispatch -- callers (all 8 call sites) check for a nil
	// assignment and skip enqueue. Placed after thread creation above --
	// a cache-hit run record still needs a real thread row to satisfy the
	// runs.thread_id foreign key.
	if req.ResumeCommand == nil {
		if run, hit, err := s.tryServeCachedRun(ctx, runID, threadID, req, now); hit || err != nil {
			return run, nil, err
		}
	}

	// TS-009: atomically claim the thread (idle/interrupted/etc -> busy) in one
	// conditional UPDATE. Two concurrent requests can never both succeed here --
	// checking status first and writing busy second (two separate calls) is a
	// TOCTOU race under real concurrency, confirmed empirically before this fix.
	claimed, err := s.store.TryClaimThread(ctx, threadID)
	if err != nil {
		return nil, nil, err
	}
	if !claimed {
		return nil, nil, &state.ErrConflict{Resource: "thread", ID: threadID}
	}

	run := &models.Run{
		RunID:       runID,
		ThreadID:    threadID,
		AgentID:     req.AgentID,
		AssistantID: req.AgentID, // SDK compat: always mirror
		Status:      models.RunStatusPending,
		Metadata:    map[string]interface{}{},
		Input:       req.Input,
		Config:      req.Config,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// LLM response caching: compute the cache key HERE, from the raw
	// request bytes (req.Input/req.Config), and stash it in the run's own
	// metadata for StatusCallback to reuse verbatim later -- a real bug
	// found via live end-to-end testing against Postgres: input/config are
	// stored as JSONB, which reformats on write (strips whitespace),
	// so recomputing the key from run.Input/run.Config fetched back out of
	// the DB (as StatusCallback originally did) produced a DIFFERENT hash
	// than the one computed from the original raw bytes at lookup time --
	// caching silently never hit on Postgres. SQLite (plain TEXT columns,
	// no reformatting) masked this in earlier testing.
	if req.ResumeCommand == nil {
		if agent, agentErr := s.store.GetAgent(ctx, req.AgentID); agentErr == nil && agent != nil {
			if cacheTTLSeconds(agent.Metadata) > 0 {
				run.Metadata["cache_key"] = computeCacheKey(tenant.FromContext(ctx), req.AgentID, req.Input, req.Config)
			}
		}
	}

	if err := s.store.CreateRun(ctx, run); err != nil {
		return nil, nil, err
	}

	metrics.RunsTotal.WithLabelValues(req.AgentID, "created").Inc()
	metrics.ActiveRuns.Inc()

	// OTel run span (master plan: "OTel observability fan-out"). No-op with
	// zero overhead unless OTEL_EXPORTER_OTLP_ENDPOINT is set (see
	// internal/tracing). Kept open in s.runSpans until StatusCallback closes
	// it on terminal status -- a run's lifetime spans this request and an
	// async completion, not a single function call.
	spanCtx, span := tracing.Tracer().Start(ctx, "run",
		trace.WithAttributes(
			attribute.String("run.id", runID),
			attribute.String("thread.id", threadID),
			attribute.String("graph.id", req.AgentID),
		),
	)
	s.runSpans.Store(runID, span)

	// runner_kind comes from the agent's own declared config (langgraph.json's
	// top-level "runner_kind", stashed in agent metadata at bootstrap --
	// see bootstrapAgents in cmd/serve.go) so a run is routed to whichever
	// runner implementation actually loaded that graph -- Python,
	// TypeScript, or otherwise. Defaults to "python-langgraph" if the
	// agent lookup fails or predates this field, matching the config
	// loader's own default and preserving every existing deployment's
	// behavior unchanged.
	runnerKind := "python-langgraph"
	agentForAssignment, agentLookupErr := s.store.GetAgent(ctx, req.AgentID)
	if agentLookupErr == nil && agentForAssignment != nil {
		if rk, ok := agentForAssignment.Metadata["runner_kind"].(string); ok && rk != "" {
			runnerKind = rk
		}
	}

	// Build RunAssignment for the runner
	assignment := &transport.RunAssignment{
		RunID:          runID,
		ThreadID:       threadID,
		RunnerKind:     runnerKind,
		GraphID:        req.AgentID,
		Input:          req.Input,
		Config:         req.Config,
		CheckpointRef:  req.CheckpointRef,
		ResumeCommand:  req.ResumeCommand,
		StreamModes:    []string{"values", "updates"},
		ConnectorNeeds: []string{},
		User:           userContextFromAuth(ctx),
		TraceContext: &transport.TraceContext{
			// Real W3C traceparent from the span above (empty string if
			// tracing is disabled) -- a runner that does its own OTel
			// instrumentation (e.g. LangChain's callback handler) can parent
			// its spans under this run's span in whatever backend is
			// configured, instead of an orphaned trace. Previously this was
			// a hand-rolled fake ID that looked like a traceparent but
			// wasn't backed by any real span -- fixed as part of wiring up
			// actual OTel support.
			Traceparent:   tracing.Traceparent(spanCtx),
			CorrelationID: runID,
		},
	}

	// Parse stream_mode from request
	if req.StreamMode != nil {
		var modes []string
		// stream_mode can be a string or array
		var singleMode string
		if json.Unmarshal(req.StreamMode, &singleMode) == nil {
			modes = []string{singleMode}
		} else {
			json.Unmarshal(req.StreamMode, &modes)
		}
		if len(modes) > 0 {
			assignment.StreamModes = modes
		}
	}

	// connector_needs comes from the agent's own declared config (langgraph.json
	// "connector_needs" section), not the client request -- the agent author
	// knows its own dependencies statically, the run-creation caller doesn't.
	// Reuses the same agent fetched above for runner_kind -- no need to
	// look it up twice.
	if agentForAssignment != nil {
		if needs := extractConnectorNeeds(agentForAssignment.Metadata); len(needs) > 0 {
			assignment.ConnectorNeeds = needs
		}
	}

	// Pre-warm connector sessions if registry is available and connector_needs is non-empty.
	// This is a hint, not a hard gate — don't block run creation on failure.
	if s.connectors != nil && len(assignment.ConnectorNeeds) > 0 {
		sessions := make(map[string]interface{})
		for _, name := range assignment.ConnectorNeeds {
			sess, err := s.connectors.GetSession(ctx, name, nil)
			if err != nil {
				slog.Warn("connector pre-warm failed", "connector", name, "run_id", runID, "error", err)
				continue
			}
			sessions[name] = sess
		}
		if len(sessions) > 0 {
			ctxData, _ := json.Marshal(sessions)
			assignment.Context = ctxData
		}
	}

	// Event hooks (master plan: on_run_start). Fired here rather than after
	// the caller's separate enqueue call so every one of createRun's 8 call
	// sites (REST, WebSocket, streaming commands) gets it automatically
	// instead of needing to remember to fire it themselves; the tradeoff is
	// a run_start hook fires even in the rare case the subsequent enqueue
	// fails, which is preferable to the alternative of a hook that's easy
	// to silently forget at a new call site.
	s.hooks.Dispatch(hooks.Event{
		Type: hooks.RunStart, RunID: runID, ThreadID: threadID, AgentID: req.AgentID,
		Timestamp: now,
	})

	// on_tool_call needs to observe the run's live event stream, which
	// createRun's caller doesn't otherwise do (it only cares about the
	// terminal outcome). Skipped entirely when nothing is listening for
	// hooks, so this costs nothing in the common (hooks-disabled) case.
	if s.hooks.HasSinks() {
		go s.watchRunEventsForToolCallHook(runID, threadID, req.AgentID)
	}

	return run, assignment, nil
}

// DispatchScheduledRun creates and enqueues a run with no HTTP request
// involved at all -- for the cron scheduler (see cmd/cron.go) or any other
// non-HTTP caller. Uses a fixed thread per schedule name ("cron:<name>")
// so a schedule's run history is browsable as one continuous thread the
// same way any other Agent Protocol thread is, rather than a fresh
// disconnected thread per fire.
func (s *Server) DispatchScheduledRun(ctx context.Context, scheduleName, agentID string, input, config json.RawMessage) (*models.Run, error) {
	threadID := "cron:" + scheduleName
	run, assignment, err := s.createRunCtx(ctx, threadID, &models.RunCreate{
		AgentID: agentID, Input: input, Config: config,
	})
	if err != nil {
		return nil, err
	}
	if err := s.enqueue(ctx, assignment); err != nil {
		return nil, err
	}
	return run, nil
}

// watchRunEventsForToolCallHook tails a run's event stream purely to fire
// on_tool_call hooks, reusing the same Subscribe+Replay-free tailing the
// broker already provides everywhere else (SSE/WS streaming). The control
// plane doesn't parse LangGraph/LangChain message shapes to detect tool
// calls itself -- staying framework-agnostic -- it just watches for a
// RunEvent whose method is literally "tool_call", which a framework-aware
// runner can choose to emit alongside its normal values/updates events.
func (s *Server) watchRunEventsForToolCallHook(runID, threadID, agentID string) {
	ch, err := s.broker.Subscribe(context.Background(), runID)
	if err != nil {
		return
	}
	// Ceiling so an orphaned run (never reaches StatusCallback / cancel,
	// so broker.Close never fires) cannot leave this goroutine blocked
	// forever. Normal runs close the channel via broker.Close well before
	// this — the timer is a leak guard, not a soft timeout on tool hooks.
	timer := time.NewTimer(2 * time.Hour)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-ch:
			if !ok {
				return
			}
			if event.Method != "tool_call" {
				continue
			}
			var data map[string]interface{}
			json.Unmarshal(event.Data, &data)
			s.hooks.Dispatch(hooks.Event{
				Type: hooks.ToolCall, RunID: runID, ThreadID: threadID, AgentID: agentID,
				Data: data, Timestamp: time.Now().UTC(),
			})
		case <-timer.C:
			return
		}
	}
}

// tryServeCachedRun checks for a cached result for this exact (agent,
// input, config) combination and, on a hit, synthesizes a completed run
// record directly -- no queue, no runner, no wait. hit=false (with a nil
// run and nil error) means "proceed with a normal run" -- the overwhelmingly
// common case (agent has no cache config, or a real cache miss).
func (s *Server) tryServeCachedRun(ctx context.Context, runID, threadID string, req *models.RunCreate, now time.Time) (run *models.Run, hit bool, err error) {
	agent, agentErr := s.store.GetAgent(ctx, req.AgentID)
	if agentErr != nil || agent == nil {
		return nil, false, nil
	}
	ttlSeconds := cacheTTLSeconds(agent.Metadata)
	if ttlSeconds <= 0 {
		return nil, false, nil
	}

	cacheKey := computeCacheKey(tenant.FromContext(ctx), req.AgentID, req.Input, req.Config)
	cached, cacheErr := s.store.GetCachedRunResult(ctx, cacheKey)
	if cacheErr != nil {
		return nil, false, nil // miss (including "not found or expired") -- caller proceeds normally
	}

	outputJSON, _ := json.Marshal(cached.Output)
	run = &models.Run{
		RunID: runID, ThreadID: threadID, AgentID: req.AgentID, AssistantID: req.AgentID,
		Status:    models.RunStatusSuccess,
		Metadata:  map[string]interface{}{"cache_hit": true},
		Input:     req.Input,
		Config:    req.Config,
		Output:    outputJSON,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.CreateRun(ctx, run); err != nil {
		return nil, false, err
	}
	// CreateRun's INSERT has no output column (output is normally only
	// known after the run finishes) -- a cache hit already has it at
	// creation time, so persist it in a separate, explicit update.
	if err := s.store.UpdateRunStatus(ctx, runID, models.RunStatusSuccess, outputJSON, ""); err != nil {
		slog.Error("failed to persist cached run output", "run_id", runID, "error", err)
	}
	metrics.RunsTotal.WithLabelValues(req.AgentID, "cache_hit").Inc()

	// Keep thread state consistent with what a normal completed run would
	// produce -- a client reading GET /threads/{id}/state right after a
	// cache hit must see the (cached) values, same as any other run.
	s.saveRunCheckpoint(ctx, threadID, cached.Output)
	_, _ = s.store.UpdateThread(ctx, threadID, &models.ThreadPatch{Values: cached.Output})

	s.hooks.Dispatch(hooks.Event{Type: hooks.RunStart, RunID: runID, ThreadID: threadID, AgentID: req.AgentID, Timestamp: now})
	s.hooks.Dispatch(hooks.Event{
		Type: hooks.RunComplete, RunID: runID, ThreadID: threadID, AgentID: req.AgentID,
		Data: map[string]interface{}{"status": "success", "cache_hit": true}, Timestamp: time.Now().UTC(),
	})

	// No events are ever published for this run (there's no runner
	// dispatch), so any later s.broker.Subscribe(runID) call -- e.g. the
	// WebSocket/streaming-commands run.start path, which always tails a
	// run's events after starting it -- would otherwise block forever on
	// a channel nothing ever closes. Closing it now (Subscribe on an
	// already-closed run returns an immediately-closed channel on both the
	// in-process and Redis brokers) makes that tailer exit immediately
	// instead of leaking a goroutine per cache-hit run.
	_ = s.broker.Close(runID)

	return run, true, nil
}

// cacheTTLSeconds reads the "cache_ttl_seconds" hint from agent metadata
// (set at bootstrap from langgraph.json's "llm_cache" section -- see
// bootstrapAgents in cmd/serve.go). Returns 0 (caching disabled) if absent.
func cacheTTLSeconds(metadata map[string]interface{}) int {
	raw, ok := metadata["cache_ttl_seconds"]
	if !ok {
		return 0
	}
	switch v := raw.(type) {
	case int:
		return v
	case float64: // JSON round-trip through the state store decodes numbers as float64
		return int(v)
	default:
		return 0
	}
}

// computeCacheKey hashes (agent_id, input, config) into a stable cache key.
// ponytail: hashes the raw request bytes as-is rather than canonicalizing
// (parse+re-marshal with sorted keys) first, so two semantically-identical
// inputs serialized with different key order/whitespace produce different
// keys -- an extra cache miss, never a wrong cache hit, so it's safe, just
// not maximally effective. Upgrade path: canonicalize via
// json.Unmarshal+json.Marshal (Go sorts map keys) before hashing, if cache
// hit rate in practice turns out to matter more than this function's
// current simplicity.
// computeCacheKey hashes (tenant, agent, input, config). Tenant is part of
// the key on purpose: run_cache's row identity must not collide across
// tenants for the same agent/input (a real bug when the hash omitted
// tenant_id — ON CONFLICT(cache_key) let tenant B overwrite tenant A's
// cached output while leaving tenant_id=A, so A later served B's result).
// userContextFromAuth converts the caller's auth.AuthResult (if any) into
// a transport.UserContext for the outgoing RunAssignment. Framework-
// specific runners (e.g. the Python LangGraph adapter's factory graphs)
// reconstruct their own runtime.user from this -- the control plane never
// imports LangGraph types. Returns nil when there's no authenticated
// identity (no auth provider configured, or the request context carries
// none) -- a nil User is the "anonymous" case, not an error.
func userContextFromAuth(ctx context.Context) *transport.UserContext {
	result := auth.FromContext(ctx)
	if result == nil || result.Identity == "" {
		return nil
	}
	return &transport.UserContext{
		Identity:        result.Identity,
		DisplayName:     result.DisplayName,
		IsAuthenticated: true,
		Permissions:     result.Permissions,
		Extra:           result.Extra,
	}
}

func computeCacheKey(tenantID, agentID string, input, config json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(tenantID))
	h.Write([]byte{0})
	h.Write([]byte(agentID))
	h.Write([]byte{0})
	h.Write(input)
	h.Write([]byte{0})
	h.Write(config)
	return hex.EncodeToString(h.Sum(nil))
}

// extractConnectorNeeds reads the "connector_needs" list from agent metadata.
// Handles both []string (set in-process) and []interface{} (the shape it
// comes back as after a JSON round-trip through the state store).
func extractConnectorNeeds(metadata map[string]interface{}) []string {
	raw, ok := metadata["connector_needs"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []interface{}:
		needs := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				needs = append(needs, s)
			}
		}
		return needs
	default:
		return nil
	}
}

// --- Background Runs ---

// POST /runs
func (s *Server) handleCreateBackgroundRun(w http.ResponseWriter, r *http.Request) {
	var req models.RunCreate
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	threadID := req.ThreadID
	if threadID == "" {
		threadID = uuid.New().String()
	}

	run, assignment, err := s.createRun(r, threadID, &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	if err := s.enqueue(r.Context(), assignment); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to enqueue run")
		return
	}

	slog.Info("background run created", "run_id", run.RunID, "thread_id", threadID)
	writeJSON(w, http.StatusOK, run)
}

// POST /runs/stream
func (s *Server) handleCreateAndStreamRun(w http.ResponseWriter, r *http.Request) {
	var req models.RunCreate
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	threadID := req.ThreadID
	if threadID == "" {
		threadID = uuid.New().String()
	}

	run, assignment, err := s.createRun(r, threadID, &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	s.streamRun(w, r, run, assignment)
}

// POST /runs/wait
func (s *Server) handleCreateAndWaitRun(w http.ResponseWriter, r *http.Request) {
	var req models.RunCreate
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	threadID := req.ThreadID
	if threadID == "" {
		threadID = uuid.New().String()
	}

	run, assignment, err := s.createRun(r, threadID, &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	s.waitForRun(w, r, run, assignment)
}

// GET /runs/{runID}
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")

	run, err := s.store.GetRun(r.Context(), runID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// DELETE /runs/{runID}
func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	s.deleteRunGuarded(w, r, runID)
}

// deleteRunGuarded checks that a run is terminal before deleting (AP-035).
func (s *Server) deleteRunGuarded(w http.ResponseWriter, r *http.Request, runID string) {
	run, err := s.store.GetRun(r.Context(), runID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if !isTerminalStatus(run.Status) {
		writeError(w, http.StatusUnprocessableEntity, "cannot delete active run; cancel it first")
		return
	}
	if err := s.store.DeleteRun(r.Context(), runID); err != nil {
		handleStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /runs/search
func (s *Server) handleSearchRuns(w http.ResponseWriter, r *http.Request) {
	var req models.RunSearchRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}

	runs, err := s.store.SearchRuns(r.Context(), &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if runs == nil {
		runs = []*models.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// POST /runs/{runID}/cancel
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	s.cancelRun(w, r, runID)
}

// GET /runs/{runID}/wait
func (s *Server) handleWaitRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	s.waitForExistingRun(w, r, runID)
}

// GET /runs/{runID}/stream
func (s *Server) handleStreamRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	s.streamExistingRun(w, r, runID)
}

// --- Thread Runs ---

// POST /threads/{threadID}/runs
func (s *Server) handleCreateThreadRun(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")

	var req models.RunCreate
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	run, assignment, err := s.createRun(r, threadID, &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	if err := s.enqueue(r.Context(), assignment); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to enqueue run")
		return
	}

	slog.Info("thread run created", "run_id", run.RunID, "thread_id", threadID)
	writeJSON(w, http.StatusOK, run)
}

// POST /threads/{threadID}/runs/stream
func (s *Server) handleCreateAndStreamThreadRun(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")

	var req models.RunCreate
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	run, assignment, err := s.createRun(r, threadID, &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	s.streamRun(w, r, run, assignment)
}

// POST /threads/{threadID}/runs/wait
func (s *Server) handleCreateAndWaitThreadRun(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")

	var req models.RunCreate
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	run, assignment, err := s.createRun(r, threadID, &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	s.waitForRun(w, r, run, assignment)
}

// GET /threads/{threadID}/runs
func (s *Server) handleListThreadRuns(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")

	runs, err := s.store.SearchRuns(r.Context(), &models.RunSearchRequest{ThreadID: threadID, Limit: 100})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if runs == nil {
		runs = []*models.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// GET /threads/{threadID}/runs/{runID}
func (s *Server) handleGetThreadRun(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")
	runID := r.PathValue("runID")

	run, err := s.store.GetRun(r.Context(), runID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if run.ThreadID != threadID {
		writeError(w, http.StatusNotFound, "run not found on this thread")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// GET /threads/{threadID}/runs/{runID}/stream
func (s *Server) handleStreamThreadRun(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")
	runID := r.PathValue("runID")
	if !s.verifyRunThread(w, r, runID, threadID) {
		return
	}
	s.streamExistingRun(w, r, runID)
}

// GET /threads/{threadID}/runs/{runID}/wait
func (s *Server) handleWaitThreadRun(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")
	runID := r.PathValue("runID")
	if !s.verifyRunThread(w, r, runID, threadID) {
		return
	}
	s.waitForExistingRun(w, r, runID)
}

// POST /threads/{threadID}/runs/{runID}/cancel
func (s *Server) handleCancelThreadRun(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")
	runID := r.PathValue("runID")
	if !s.verifyRunThread(w, r, runID, threadID) {
		return
	}
	s.cancelRun(w, r, runID)
}

// DELETE /threads/{threadID}/runs/{runID}
func (s *Server) handleDeleteThreadRun(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")
	runID := r.PathValue("runID")
	if !s.verifyRunThread(w, r, runID, threadID) {
		return
	}
	s.deleteRunGuarded(w, r, runID)
}

// verifyRunThread checks that a run belongs to the given thread. Returns false
// (and writes the error response) when the run doesn't exist or is on a
// different thread.
func (s *Server) verifyRunThread(w http.ResponseWriter, r *http.Request, runID, threadID string) bool {
	run, err := s.store.GetRun(r.Context(), runID)
	if err != nil {
		handleStoreError(w, err)
		return false
	}
	if run.ThreadID != threadID {
		writeError(w, http.StatusNotFound, "run not found on this thread")
		return false
	}
	return true
}

// GET /internal/runs/{runID}/status
func (s *Server) handleGetRunStatus(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")

	run, err := s.store.GetRun(r.Context(), runID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": string(run.Status)})
}

// --- Shared streaming/waiting logic ---

func (s *Server) streamRun(w http.ResponseWriter, r *http.Request, run *models.Run, assignment *transport.RunAssignment) {
	// A cache hit (see tryServeCachedRun) never publishes any broker
	// events -- it completed synchronously before this function was even
	// called. Subscribing and waiting here would hang forever waiting for
	// events that will never arrive, so it gets its own tiny, direct SSE
	// response instead of the subscribe/enqueue/tail path below.
	if assignment == nil {
		streamCacheHitRun(w, run)
		return
	}

	// Subscribe BEFORE enqueuing (no race)
	eventCh, err := s.broker.Subscribe(r.Context(), run.RunID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to subscribe to events")
		return
	}

	if err := s.enqueue(r.Context(), assignment); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to enqueue run")
		return
	}

	slog.Info("run created and streaming", "run_id", run.RunID)
	metrics.ActiveSSEConnections.Inc()
	defer metrics.ActiveSSEConnections.Dec()

	// SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	// Metadata event
	fmt.Fprintf(w, "event: metadata\ndata: {\"run_id\":%q}\n\n", run.RunID)
	flusher.Flush()

	// Stream events — send just the payload (event.Data), not the full RunEvent
	// envelope, to match the LangGraph platform SSE format the SDK expects.
	// Checkpoint/thread-values persistence happens centrally in StatusCallback,
	// not here, so it fires regardless of how the client observes the run.
	for event := range eventCh {
		fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", event.EventID, event.Method, string(event.Data))
		flusher.Flush()
		if event.IsTerminal() {
			break
		}
	}
}

func (s *Server) streamExistingRun(w http.ResponseWriter, r *http.Request, runID string) {
	metrics.ActiveSSEConnections.Inc()
	defer metrics.ActiveSSEConnections.Dec()

	run, err := s.store.GetRun(r.Context(), runID)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	// If already terminal, replay full history (which includes the end event).
	// Only append a synthetic end if replay had no terminal event (e.g. broker
	// buffer was already evicted).
	if isTerminalStatus(run.Status) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		hasTerminal := false
		if past, replayErr := s.broker.Replay(r.Context(), runID, 0); replayErr == nil {
			for _, ev := range past {
				fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.EventID, ev.Method, string(ev.Data))
				if ev.IsTerminal() {
					hasTerminal = true
				}
			}
		}
		if !hasTerminal {
			fmt.Fprintf(w, "event: end\ndata: {\"status\":%q}\n\n", run.Status)
		}
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	// Subscribe FIRST so no live events are missed, then replay history.
	eventCh, err := s.broker.Subscribe(r.Context(), runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to subscribe")
		return
	}

	// Replay all past events to find the high-water mark
	var maxReplayedSeq int64
	pastEvents, _ := s.broker.Replay(r.Context(), runID, 0)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	// Send replayed (historical) events
	for _, ev := range pastEvents {
		fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.EventID, ev.Method, string(ev.Data))
		if ev.Seq > maxReplayedSeq {
			maxReplayedSeq = ev.Seq
		}
		if ev.IsTerminal() {
			flusher.Flush()
			return
		}
	}
	flusher.Flush()

	// Tail live events, skipping any already sent via replay (dedup on Seq)
	for event := range eventCh {
		if event.Seq <= maxReplayedSeq {
			continue
		}
		fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", event.EventID, event.Method, string(event.Data))
		flusher.Flush()
		if event.IsTerminal() {
			break
		}
	}
}

// streamCacheHitRun sends the minimal SSE sequence a real streamed run
// would end with (a "values" event carrying the cached output, then a
// terminal "end" event) for a run that completed synchronously via the
// LLM response cache -- no runner, no broker events to tail.
func streamCacheHitRun(w http.ResponseWriter, run *models.Run) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	fmt.Fprintf(w, "id: %s_cache\nevent: values\ndata: %s\n\n", run.RunID, string(run.Output))
	flusher.Flush()
	fmt.Fprintf(w, "id: %s_end\nevent: end\ndata: {\"status\":\"success\"}\n\n", run.RunID)
	flusher.Flush()
}

func (s *Server) waitForRun(w http.ResponseWriter, r *http.Request, run *models.Run, assignment *transport.RunAssignment) {
	// A cache hit (see tryServeCachedRun) already completed synchronously
	// with no broker events published for it -- respond directly instead
	// of subscribing and waiting for events that will never arrive.
	if assignment == nil {
		resp := models.RunWaitResponse{Run: run}
		var vals map[string]interface{}
		json.Unmarshal(run.Output, &vals)
		resp.Values = vals
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Subscribe BEFORE enqueuing
	eventCh, err := s.broker.Subscribe(r.Context(), run.RunID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to subscribe")
		return
	}

	if err := s.enqueue(r.Context(), assignment); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to enqueue")
		return
	}

	// Wait for terminal event
	var lastValues json.RawMessage
	var terminalStatus string
	for event := range eventCh {
		if event.Method == "values" {
			lastValues = event.Data
		}
		if event.IsTerminal() {
			// Extract status from the end event data (e.g. {"status":"success"})
			var endData map[string]interface{}
			if json.Unmarshal(event.Data, &endData) == nil {
				if s, ok := endData["status"].(string); ok {
					terminalStatus = s
				}
			}
			break
		}
	}

	// Note: checkpoint/thread-values persistence happens centrally in
	// StatusCallback (fired by the runner's ReportStatus RPC), not here, so
	// it's consistent regardless of how the client observes the run.

	// Get final run state — ReportStatus may not have arrived yet, so
	// overlay the status from the terminal event if the store is still
	// stale, AND persist it here too (real bug found via load testing,
	// see waitForExistingRun's identical fix for the full rationale --
	// without this, a plain GET immediately after this response could
	// still see "pending" even though this response says "success").
	finalRun, _ := s.store.GetRun(r.Context(), run.RunID)
	if finalRun == nil {
		finalRun = run
	}
	if terminalStatus != "" && !isTerminalStatus(finalRun.Status) {
		if err := s.store.UpdateRunStatus(r.Context(), run.RunID, models.RunStatus(terminalStatus), nil, ""); err != nil {
			slog.Error("wait: failed to persist status derived from event stream", "run_id", run.RunID, "error", err)
		}
		// See waitForExistingRun's identical fix for the full rationale:
		// the thread's own status must be reset too, not just the run's,
		// or a fast wait-then-immediately-create-the-next-run on the
		// same thread can get a spurious 409 (TryClaimThread sees a
		// still-"busy" thread until StatusCallback's later write).
		threadStatus := models.ThreadStatusIdle
		if terminalStatus == string(models.RunStatusInterrupted) {
			threadStatus = models.ThreadStatusInterrupted
		}
		if err := s.store.SetThreadStatus(r.Context(), finalRun.ThreadID, threadStatus); err != nil {
			slog.Error("wait: failed to reset thread status derived from event stream", "thread_id", finalRun.ThreadID, "error", err)
		}
		finalRun.Status = models.RunStatus(terminalStatus)
	}

	resp := models.RunWaitResponse{
		Run: finalRun,
	}
	if lastValues != nil {
		var vals map[string]interface{}
		json.Unmarshal(lastValues, &vals)
		resp.Values = vals
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) waitForExistingRun(w http.ResponseWriter, r *http.Request, runID string) {
	run, err := s.store.GetRun(r.Context(), runID)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	if isTerminalStatus(run.Status) {
		// Replay to find the last values event for a complete response
		resp := models.RunWaitResponse{Run: run}
		if past, replayErr := s.broker.Replay(r.Context(), runID, 0); replayErr == nil {
			for i := len(past) - 1; i >= 0; i-- {
				if past[i].Method == "values" {
					var vals map[string]interface{}
					if json.Unmarshal(past[i].Data, &vals) == nil {
						resp.Values = vals
					}
					break
				}
			}
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Subscribe FIRST, then replay to catch any events already published
	eventCh, err := s.broker.Subscribe(r.Context(), runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to subscribe")
		return
	}

	var lastValues json.RawMessage
	var terminalStatus string
	var maxReplayedSeq int64

	// Replay past events to catch values already emitted
	if past, replayErr := s.broker.Replay(r.Context(), runID, 0); replayErr == nil {
		for _, ev := range past {
			if ev.Seq > maxReplayedSeq {
				maxReplayedSeq = ev.Seq
			}
			if ev.Method == "values" {
				lastValues = ev.Data
			}
			if ev.IsTerminal() {
				var endData map[string]interface{}
				if json.Unmarshal(ev.Data, &endData) == nil {
					if s, ok := endData["status"].(string); ok {
						terminalStatus = s
					}
				}
			}
		}
	}

	// If replay already contained a terminal event, no need to wait on live channel
	if terminalStatus == "" {
		for event := range eventCh {
			if event.Seq <= maxReplayedSeq {
				continue
			}
			if event.Method == "values" {
				lastValues = event.Data
			}
			if event.IsTerminal() {
				var endData map[string]interface{}
				if json.Unmarshal(event.Data, &endData) == nil {
					if s, ok := endData["status"].(string); ok {
						terminalStatus = s
					}
				}
				break
			}
		}
	}

	finalRun, _ := s.store.GetRun(r.Context(), runID)
	if finalRun == nil {
		finalRun = run
	}
	if terminalStatus != "" && !isTerminalStatus(finalRun.Status) {
		// Real bug found via load testing: this used to only patch
		// finalRun.Status in memory for THIS response, never writing it
		// back -- so /wait correctly reported "success" (derived from
		// observing the terminal event) while a plain GET immediately
		// afterward still saw "pending", because the runner's separate
		// ReportStatus RPC (which is what StatusCallback in server.go
		// normally persists this from) hadn't arrived yet. Persisting
		// it here closes that window; StatusCallback's own later write
		// of the same status is a harmless, idempotent no-op overwrite.
		// output stays nil here (matching StatusCallback's own
		// UpdateRunStatus call) -- that field is populated elsewhere,
		// not derived from a "values" event.
		if err := s.store.UpdateRunStatus(r.Context(), runID, models.RunStatus(terminalStatus), nil, ""); err != nil {
			slog.Error("wait: failed to persist status derived from event stream", "run_id", runID, "error", err)
		}
		// Real gap found in review: persisting the run's status (above)
		// isn't enough on its own -- TryClaimThread (createRunCtx) still
		// checks the THREAD's own status, which otherwise stays "busy"
		// until StatusCallback's later SetThreadStatus call. A fast
		// client doing wait-then-immediately-create-the-next-run on the
		// same thread could get a spurious 409 in that gap. Resetting it
		// here too closes that window the same way the run-status fix
		// did; StatusCallback's later write is again a harmless no-op.
		// Deliberately NOT replicating StatusCallback's other side
		// effects here (checkpoint save, thread.values update, cache
		// population) -- those are more involved and still solely
		// StatusCallback's job, same as before this fix.
		threadStatus := models.ThreadStatusIdle
		if terminalStatus == string(models.RunStatusInterrupted) {
			threadStatus = models.ThreadStatusInterrupted
		}
		if err := s.store.SetThreadStatus(r.Context(), finalRun.ThreadID, threadStatus); err != nil {
			slog.Error("wait: failed to reset thread status derived from event stream", "thread_id", finalRun.ThreadID, "error", err)
		}
		finalRun.Status = models.RunStatus(terminalStatus)
	}

	resp := models.RunWaitResponse{Run: finalRun}
	if lastValues != nil {
		var vals map[string]interface{}
		json.Unmarshal(lastValues, &vals)
		resp.Values = vals
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request, runID string) {
	updated, err := s.cancelRunCore(r.Context(), runID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if updated != nil {
		writeJSON(w, http.StatusOK, updated)
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

// cancelRunCore is the HTTP-independent cancel implementation, shared by the
// REST DELETE/cancel handler and the WebSocket "run.cancel" command.
func (s *Server) cancelRunCore(ctx context.Context, runID string) (*models.Run, error) {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}

	if isTerminalStatus(run.Status) {
		return run, nil
	}

	_ = s.queue.Cancel(ctx, runID)
	_ = s.cancel.PublishCancel(ctx, runID)
	_ = s.store.UpdateRunStatus(ctx, runID, models.RunStatusInterrupted, nil, "")
	_ = s.store.SetThreadStatus(ctx, run.ThreadID, models.ThreadStatusInterrupted)

	// Finish the run's terminal bookkeeping (OTel span + on_interrupt hook)
	// here defensively -- cancel is a terminal outcome even if the runner's
	// own StatusCallback never arrives (runner already dead, etc). Safe if
	// StatusCallback ALSO fires later: finishRun's LoadAndDelete guard
	// means only whichever call arrives first does anything.
	s.finishRun(runID, run.ThreadID, run.AgentID, models.RunStatusInterrupted, "")

	// Give runner time to emit its own terminal event, then close broker
	go func() {
		time.Sleep(5 * time.Second)
		_ = s.broker.Close(runID)
	}()

	slog.Info("run cancelled", "run_id", runID)

	updated, _ := s.store.GetRun(ctx, runID)
	if updated != nil {
		return updated, nil
	}
	return nil, nil
}

func isTerminalStatus(status models.RunStatus) bool {
	return status == models.RunStatusSuccess ||
		status == models.RunStatusError ||
		status == models.RunStatusInterrupted ||
		status == models.RunStatusTimeout
}
