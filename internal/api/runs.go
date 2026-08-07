package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/metrics"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/ratelimit"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/tracing"
	"github.com/getrunkite/runkite/internal/transport"
)

// --- Shared run creation logic ---

func (s *Server) createRun(r *http.Request, threadID string, req *models.RunCreate) (*models.Run, *transport.RunAssignment, error) {
	return s.createRunCtx(r.Context(), threadID, req)
}

// findRunForRetry implements client-retriable run creation: a client that
// supplies its own run_id can safely retry a create-run request (e.g. after
// a timeout or dropped connection) without risking a duplicate run, because
// every client-facing create-run handler calls this FIRST, before doing
// anything else, and if it finds a run, delegates to the existing-run
// wait/stream/get path instead of creating a new one.
//
// Deliberately NOT implemented by reusing createRunCtx's own nil-assignment
// "cache hit" convention (tryServeCachedRun) -- that convention always means
// "definitively complete, safe to render run.Output as a synthetic success,"
// which is true for an LLM-cache hit but NOT true here: the run this finds
// might still be pending/running (the original request is still being
// processed, or the client is retrying while it's mid-flight), and reusing
// the cache-hit renderer would fabricate a fake "success" for a run that
// hasn't actually finished. Delegating to waitForExistingRun/streamExistingRun
// instead is correct for any status, because those already branch on
// isTerminalStatus rather than assuming completion.
//
// The narrower race below this pre-check -- a genuinely concurrent retry
// that arrives after this check but before the original request finishes
// claiming the thread or inserting the run row -- is handled separately
// inside createRunCtx via errRunRetryRace, for the exact same "might not be
// done yet" reason (see errRunRetryRace's own doc comment). That path checks
// the same fingerprint (via fingerprintMismatch, below) for the identical
// reason this one does.
//
// Also validates a fingerprint against the found run -- a client-supplied
// run_id is only a safe retry if paired with the SAME agent/thread/input
// every time. A run_id reused with a DIFFERENT one of those is either a
// client bug (e.g. accidentally reusing an ID across unrelated requests) or
// a genuine run_id collision -- either way, silently returning the
// ORIGINAL request's run for a caller that thinks it's creating something
// different would be a real correctness surprise, not a helpful retry.
// Returns a *state.ErrConflict (409, the same conflict type this project
// already uses for a stale optimistic-concurrency version on PATCH
// /threads) instead, which the caller should route through
// handleStoreError exactly like any other store error.
func (s *Server) findRunForRetry(ctx context.Context, runID string, req *models.RunCreate, effectiveThreadID string) (*models.Run, error) {
	if runID == "" {
		return nil, nil
	}
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return nil, nil
	}
	if reason := s.fingerprintMismatch(run, req, effectiveThreadID); reason != "" {
		return nil, &state.ErrConflict{Resource: "run", ID: runID, Reason: reason}
	}
	return run, nil
}

// fingerprintMismatch returns a non-empty conflict reason if run doesn't
// match the request retrying with the same run_id, or "" if it's a safe
// retry (including when the request simply didn't specify a given field --
// only fields the client actually set are compared, so an older/thinner
// retry client isn't penalized for omitting something the original request
// happened to include).
//
// AgentID comparison must NOT call AliasResolver.Resolve a second time to
// get a "real" agent_id to compare against -- Resolve picks weighted-random
// among an alias's targets on every call (see its own doc comment), so a
// retry resolving to a DIFFERENT target than the original request did
// would wrongly look like a mismatch, turning a legitimate retry into a
// false conflict. Instead: if the request's agent name is a currently
// configured alias at all (checked via Resolve's own boolean return,
// discarding the random pick), compare against the alias NAME the
// original run recorded it was requested through (run.Metadata's own
// "requested_alias", set once at creation and never re-rolled) rather
// than re-resolving. A non-alias agent_id still compares directly against
// run.AgentID as before.
//
// Input is compared by decoded JSON structure (unmarshal + reflect.DeepEqual),
// not raw bytes -- byte-for-byte comparison would false-positive on a
// harmless whitespace/key-order difference between two JSON encoders (e.g.
// a client re-serializing the same logical payload before retrying).
func (s *Server) fingerprintMismatch(run *models.Run, req *models.RunCreate, effectiveThreadID string) string {
	agentID := req.AgentID
	if agentID == "" {
		agentID = req.AssistantID
	}
	if agentID != "" {
		if _, wasAlias := s.aliases.Resolve(agentID); wasAlias {
			storedAlias, _ := run.Metadata["requested_alias"].(string)
			if storedAlias != agentID {
				return "run_id reused with a different agent"
			}
		} else if run.AgentID != agentID {
			return "run_id reused with a different agent"
		}
	}
	if effectiveThreadID != "" && run.ThreadID != effectiveThreadID {
		return "run_id reused with a different thread"
	}
	if len(req.Input) > 0 && !jsonDeepEqual(run.Input, req.Input) {
		return "run_id reused with different input"
	}
	return ""
}

// jsonDeepEqual reports whether a and b decode to the same JSON structure,
// ignoring whitespace/key-order differences a raw byte comparison would
// treat as different. Malformed JSON on either side compares unequal
// (conservative: treat "can't tell" as "don't silently allow it through"),
// not equal.
func jsonDeepEqual(a, b json.RawMessage) bool {
	var av, bv interface{}
	if json.Unmarshal(a, &av) != nil {
		return false
	}
	if json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// errRunRetryRace signals that a concurrent request already claimed the
// thread or created the run for a client-supplied run_id this request was
// also trying to create -- the narrow race window between findRunForRetry's
// pre-check (above) and this request reaching TryClaimThread/CreateRun
// inside createRunCtx. Deliberately a DISTINCT type from the (run, nil, nil)
// "cache hit" convention used elsewhere in createRunCtx: a cache hit is
// always definitively complete and safe to render as a synthetic success,
// but the run found here might still be pending/running, so callers must
// dispatch it through the SAME status-aware retry path (waitForExistingRun /
// streamExistingRun / a plain write) that findRunForRetry's callers already
// use, not assume completion. Only ever constructed by createRunCtx and only
// ever reachable when the caller supplied a client run_id (see
// dispatchIfRunRetryRace's call sites) -- every other creation path
// (WebSocket commands, A2A delegation, cron) never sets a client run_id, so
// this race is structurally impossible for them.
type errRunRetryRace struct {
	run *models.Run
}

func (e *errRunRetryRace) Error() string {
	return fmt.Sprintf("run %s already exists from a concurrent request", e.run.RunID)
}

// dispatchIfRunRetryRace checks whether err is errRunRetryRace and, if so,
// dispatches the found run through respond (typically a closure wrapping
// writeJSON, streamExistingRun, or waitForExistingRun -- matching whatever
// this specific handler already does for findRunForRetry's own pre-check)
// instead of the generic error path. Returns true if it handled the
// response, in which case the caller must not write anything else.
//
// Prefer handleCreateRunError (below) over calling this directly -- see
// its own doc comment for why.
func dispatchIfRunRetryRace(err error, respond func(run *models.Run)) bool {
	var raceErr *errRunRetryRace
	if errors.As(err, &raceErr) {
		respond(raceErr.run)
		return true
	}
	return false
}

// handleCreateRunError is the ONE call every client-facing create-run
// handler makes on a createRunCtx error -- folds dispatchIfRunRetryRace
// and handleStoreError into a single call, unconditionally writing a
// response either way, so there's nothing left for a caller to get wrong
// by forgetting a step.
//
// This exists because the original
// two-separate-calls pattern (dispatchIfRunRetryRace, THEN
// handleStoreError only if that returned false) wasn't enforced by the
// type system -- a future create-run handler that copy-pasted just the
// handleStoreError half would compile fine and still work most of the
// time (errRunRetryRace isn't one of handleStoreError's own recognized
// types, so it falls through to a generic 500 -- a safe failure mode,
// not a silent wrong success, but a real missed dispatch nonetheless).
// Collapsing both steps into this one call removes the chance to get it
// wrong for every future caller, not just today's six, without needing a
// design review, a lint rule, or trusting anyone to remember a two-step
// convention.
func (s *Server) handleCreateRunError(w http.ResponseWriter, err error, respond func(run *models.Run)) {
	if dispatchIfRunRetryRace(err, respond) {
		return
	}
	handleStoreError(w, err)
}

// createRunCtx is the context-based core of run creation, shared by every
// HTTP call site (via createRun, above) and the cron scheduler (which has
// no *http.Request to derive a context from -- see internal's cron loop in
// cmd/serve.go).
//
// Named returns so a failed path after TryClaimThread can release the
// thread claim via defer -- without that, an A2A parent-lookup failure or
// CreateRun error left the thread stuck busy forever (no future run on
// that thread could claim it).
func (s *Server) createRunCtx(ctx context.Context, threadID string, req *models.RunCreate) (outRun *models.Run, outAssign *transport.RunAssignment, err error) {
	// Empty / whitespace-only checkpoint_ref is treated as absent (resume
	// from the thread's latest checkpoint). A non-empty value is forwarded
	// on RunAssignment so LangGraph runners can time-travel via
	// configurable.checkpoint_id -- see build_run_config / buildRunConfig.
	if req.CheckpointRef != nil && strings.TrimSpace(*req.CheckpointRef) == "" {
		req.CheckpointRef = nil
	}

	now := time.Now().UTC()
	// Client-supplied run_id (retry idempotency, see RunID's own doc
	// comment) -- the common, empty case still gets a fresh server-side
	// ID exactly as before. The actual "is this a retry of an existing
	// run" check happens at the HTTP handler level, BEFORE this function
	// is even called (see findRunForRetry) -- by the time execution
	// reaches here, this is either a genuinely new run or the client
	// reused a run_id from an earlier request that's since ended up
	// running concurrently with this one (the two fallback re-checks
	// below handle that narrow race; see their own comments).
	runID := req.RunID
	if runID == "" {
		runID = uuid.New().String()
	}

	// SDK compat: accept assistant_id as alias for agent_id
	if req.AgentID == "" && req.AssistantID != "" {
		req.AgentID = req.AssistantID
	}

	// A/B deployment routing (see alias.go). Resolved BEFORE rate
	// limiting/agent lookup so everything downstream -- per-agent rate
	// limits, runner_kind lookup, the actual dispatched assignment --
	// consistently sees the REAL target agent, not the alias name (a
	// rate limit configured for a specific deployment target should
	// apply to that target regardless of which alias routed to it).
	// requestedAlias is empty for a normal, non-aliased agent_id.
	requestedAlias := ""
	if resolved, wasAlias := s.aliases.Resolve(req.AgentID); wasAlias {
		requestedAlias = req.AgentID
		req.AgentID = resolved
	}

	// Fail fast on an unknown agent_id, before claiming/creating anything.
	// Without this, a typo'd agent_id still got a 200 with a pending run
	// that only ever failed later, asynchronously, once a runner tried
	// (and failed) to load a graph that was never registered -- burning a
	// thread claim, a queue slot, and runner capacity for an error the
	// caller could have gotten synchronously instead. GetAgent is already
	// tenant-scoped (see its own implementation), and runs after alias
	// resolution above so this checks the REAL target, not the alias
	// name. Applies to every run, including resumes -- a resume still
	// needs a real, registered agent to dispatch against.
	if _, agentErr := s.store.GetAgent(ctx, req.AgentID); agentErr != nil {
		var notFound *state.ErrNotFound
		if errors.As(agentErr, &notFound) {
			return nil, nil, agentErr
		}
		// A non-not-found error here (DB down, etc.) is a real
		// infrastructure failure, not proof the agent doesn't exist --
		// don't reject the run on that basis, let the rest of this
		// function's own error handling surface the real problem if it
		// recurs.
	}

	// Per-agent rate limiting -- one dimension of the broader per-user,
	// per-agent, per-tenant limiting scheme. Enforced here rather than in
	// generic HTTP middleware because agent_id is parsed from the request
	// body, not the URL -- this is the single choke point every
	// run-creation path (REST, WebSocket, streaming commands) already goes through, so there's
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

	// Sync pre-flight gates (before_run). Must run BEFORE "ensure thread
	// exists" as well as before cache/claim/CreateRun: otherwise a deny
	// with if_not_exists=create (the default) would still auto-create an
	// idle thread row for a brand-new thread_id, accumulating empty
	// threads under a deny-everything gate. ThreadID/AgentID/input are
	// already known from the request, so the gate loses no information.
	// Observational run_start webhooks still fire later via Dispatch
	// after the run row exists. Fail-closed on timeout/error (see
	// hooks.CheckBeforeRun). 30s is a hard ceiling for the whole gate
	// chain; each HTTP gate also has its own shorter client timeout.
	{
		preflightCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		data := map[string]interface{}{}
		if len(req.Input) > 0 {
			var input any
			if json.Unmarshal(req.Input, &input) == nil {
				data["input"] = input
			}
		}
		if pfErr := s.hooks.CheckBeforeRun(preflightCtx, hooks.Event{
			Type:      hooks.BeforeRun,
			RunID:     runID,
			ThreadID:  threadID,
			AgentID:   req.AgentID,
			TenantID:  tenant.FromContext(ctx),
			Data:      data,
			Timestamp: now,
		}); pfErr != nil {
			cancel()
			return nil, nil, pfErr
		}
		cancel()
	}

	// Ensure thread exists (create if requested)
	_, err = s.store.GetThread(ctx, threadID)
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

	// LLM response caching, with a configurable per-agent TTL and a cache
	// key derived from a hash of the input. A resume is inherently
	// continuing a specific prior execution, never a fresh cacheable
	// computation, so it's excluded regardless of the agent's cache config. A hit
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
	claimed, claimErr := s.store.TryClaimThread(ctx, threadID)
	if claimErr != nil {
		return nil, nil, claimErr
	}
	if !claimed {
		// Narrow race fallback: findRunForRetry's own pre-check (at the
		// HTTP handler level) runs before this call, so it can miss a
		// genuinely concurrent retry -- the ORIGINAL request for this
		// same run_id might claim the thread and finish CreateRun in the
		// gap between that pre-check and this line. If req.RunID is set
		// and a run with that ID exists NOW, this thread being busy is
		// actually the retry's own earlier attempt still owning it (or
		// having just finished) -- return that run via errRunRetryRace
		// instead of a spurious conflict, so the caller dispatches it
		// through the same status-aware path as any other retry (the
		// run found here might still be pending, not necessarily done).
		// If it's still not there, this is either a genuinely different
		// reason the thread is busy, or a true concurrent double-send
		// racing inside a sub-millisecond window -- a 409 is the
		// correct, honest answer for that case, not worth
		// polling/retrying to close.
		if req.RunID != "" {
			if existing, getErr := s.store.GetRun(ctx, req.RunID); getErr == nil {
				// Same fingerprint check findRunForRetry's own
				// pre-check applies -- this fallback exists BECAUSE
				// that pre-check can miss a genuinely concurrent
				// retry, but "concurrent retry of the SAME request"
				// and "unrelated request that happens to reuse this
				// run_id" are still different things, and only the
				// former should be silently dispatched as the winner.
				if reason := s.fingerprintMismatch(existing, req, threadID); reason != "" {
					return nil, nil, &state.ErrConflict{Resource: "run", ID: req.RunID, Reason: reason}
				}
				return nil, nil, &errRunRetryRace{run: existing}
			}
		}
		// Reason must say "busy", not the default "already exists" --
		// TryClaimThread failing means another run owns the thread, not
		// that the thread_id is a duplicate create.
		return nil, nil, &state.ErrConflict{Resource: "thread", ID: threadID, Reason: "is busy"}
	}
	// Release the claim on any subsequent error before CreateRun succeeds
	// (A2A parent missing, depth exceeded, CreateRun failure). After
	// CreateRun the pending run owns the claim and callers that fail
	// enqueue must use rollbackCreatedRun instead.
	created := false
	defer func() {
		if err != nil && !created {
			tryStatusTransition("set_thread_status", threadID, "", func() error {
				return s.store.SetThreadStatus(ctx, threadID, models.ThreadStatusIdle)
			})
		}
	}()

	// Agent-to-Agent (A2A) delegation bookkeeping: only set when this run
	// is being created via POST /internal/a2a/runs (req.ParentRunID is
	// never populated from a client-facing request body -- see
	// RunCreate.ParentRunID's doc comment). A top-level run has Depth 0
	// and no parent/root; a delegated run inherits and increments from
	// its parent, enforced against a2aMaxDepthOrDefault() here so a
	// cyclic or runaway delegation chain fails fast at creation time
	// rather than consuming resources indefinitely.
	var rootRunID *string
	depth := 0
	if req.ParentRunID != nil {
		var parent *models.Run
		parent, err = s.store.GetRun(ctx, *req.ParentRunID)
		if err != nil {
			return nil, nil, fmt.Errorf("a2a: look up parent run %s: %w", *req.ParentRunID, err)
		}
		depth = parent.Depth + 1
		if depth > s.a2aMaxDepthOrDefault() {
			err = &ErrA2ADepthExceeded{Depth: depth, MaxDepth: s.a2aMaxDepthOrDefault()}
			return nil, nil, err
		}
		if parent.RootRunID != nil {
			rootRunID = parent.RootRunID
		} else {
			rootRunID = &parent.RunID
		}
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
		ParentRunID: req.ParentRunID,
		RootRunID:   rootRunID,
		Depth:       depth,
	}
	if requestedAlias != "" {
		// Cost/observability attribution for A/B routing: agent_id on
		// this run is already the REAL resolved target (see above) --
		// this is the only record of which alias the client actually
		// asked for, needed to answer "what fraction of alias X's
		// traffic went to which target" after the fact.
		run.Metadata["requested_alias"] = requestedAlias
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

	if err = s.store.CreateRun(ctx, run); err != nil {
		// run_id is the primary key, so this fails with a unique-
		// constraint violation if a concurrent request already inserted
		// the same client-supplied run_id first. Only reachable when
		// that run belongs to a DIFFERENT thread_id than this request's
		// own threadID -- TryClaimThread's own atomicity already rules
		// out two requests for the SAME thread_id both reaching this
		// line, so if the run_id already existed under THIS thread_id,
		// this request would never have gotten this far in the first
		// place. That means fingerprintMismatch's thread-id check below
		// will always find a mismatch here, by construction, which is
		// the correct outcome, not a bug in the check: "same run_id,
		// different thread_id" is exactly the client mistake / run_id
		// collision the fingerprint exists to catch -- a clear
		// state.ErrConflict is more useful to the caller than a raw DB
		// error, and keeps err non-nil here so the defer above releases
		// THIS request's own thread claim (the winning run's own thread
		// has no run on it and would otherwise stay stuck "busy"
		// forever with nothing left to ever idle it).
		if req.RunID != "" {
			if existing, getErr := s.store.GetRun(ctx, req.RunID); getErr == nil {
				reason := s.fingerprintMismatch(existing, req, threadID)
				if reason == "" {
					// Structurally shouldn't happen (see above) --
					// fall back to the generic race dispatch rather
					// than a reason-less conflict, just in case.
					err = &errRunRetryRace{run: existing}
					return nil, nil, err
				}
				err = &state.ErrConflict{Resource: "run", ID: req.RunID, Reason: reason}
				return nil, nil, err
			}
		}
		return nil, nil, err
	}
	created = true // pending run owns the claim; do not idle the thread on later returns

	metrics.RunsTotal.WithLabelValues(req.AgentID, "created").Inc()
	metrics.ActiveRuns.Inc()

	// OTel run span, part of the OTel observability fan-out. No-op with
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
		TenantID:       tenant.FromContext(ctx),
		// Generation starts at 1 for every fresh dispatch (see its own
		// doc comment on RunAssignment for the full fencing rationale)
		// -- ReclaimStale is the only thing that ever increments it
		// from here.
		Generation: 1,
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
		prewarmCtx := withPolicyAgent(ctx, req.AgentID, runID)
		for _, name := range assignment.ConnectorNeeds {
			if dec, deny := s.checkConnectorPolicy(prewarmCtx, policy.StageConnectorSession, name, ""); deny {
				slog.Warn("connector pre-warm skipped by policy", "connector", name, "run_id", runID, "reason_code", dec.ReasonCode)
				continue
			}
			sess, err := s.connectors.GetSession(prewarmCtx, name, nil)
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

	// Event hooks (on_run_start). Fired here rather than after
	// the caller's separate enqueue call so every one of createRun's 8 call
	// sites (REST, WebSocket, streaming commands) gets it automatically
	// instead of needing to remember to fire it themselves; the tradeoff is
	// a run_start hook fires even in the rare case the subsequent enqueue
	// fails, which is preferable to the alternative of a hook that's easy
	// to silently forget at a new call site.
	s.hooks.Dispatch(hooks.Event{
		Type: hooks.RunStart, RunID: runID, ThreadID: threadID, AgentID: req.AgentID,
		TenantID: tenant.FromContext(ctx), Timestamp: now,
	})

	// on_tool_call needs to observe the run's live event stream, which
	// createRun's caller doesn't otherwise do (it only cares about the
	// terminal outcome). Skipped entirely when nothing is listening for
	// hooks, so this costs nothing in the common (hooks-disabled) case.
	if s.hooks.HasSinks() {
		go s.watchRunEventsForToolCallHook(runID, threadID, req.AgentID, tenant.FromContext(ctx))
	}

	return run, assignment, nil
}

// rollbackCreatedRun cleans up after createRunCtx succeeded but a later
// enqueue (or equivalent) failed: the run is stuck pending, the thread
// is stuck busy, and ActiveRuns was already incremented. Without this,
// that thread can never accept another run.
func (s *Server) rollbackCreatedRun(ctx context.Context, run *models.Run) {
	if run == nil {
		return
	}
	tryStatusTransition("update_run_status", run.ThreadID, run.RunID, func() error {
		return s.store.UpdateRunStatus(ctx, run.RunID, models.RunStatusError, nil, "enqueue failed")
	})
	metrics.ActiveRuns.Dec()
	tryStatusTransition("release_thread", run.ThreadID, run.RunID, func() error {
		_, err := s.store.ReleaseThreadIfNoOtherActive(ctx, run.ThreadID, run.RunID, models.ThreadStatusIdle)
		return err
	})
	// Close after any Subscribe that may have already happened (stream/wait
	// paths subscribe before enqueue) so tailers and broker state do not leak.
	_ = s.broker.Close(run.RunID)
	tid := run.TenantID
	if tid == "" {
		tid = tenant.FromContext(ctx)
	}
	s.finishRun(run.RunID, run.ThreadID, run.AgentID, tid, models.RunStatusError, "enqueue failed")
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
		s.rollbackCreatedRun(ctx, run)
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
func (s *Server) watchRunEventsForToolCallHook(runID, threadID, agentID, tenantID string) {
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
				Type: hooks.ToolCall, RunID: runID, ThreadID: threadID, AgentID: agentID, TenantID: tenantID,
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

	// Cache hits run before TryClaimThread. If the thread is already busy
	// with a real run, mutating values/checkpoint here races that run and
	// leaves two "success" narratives for one thread. Refuse the hit and
	// let the caller take the normal claim path (which will 409).
	if th, thErr := s.store.GetThread(ctx, threadID); thErr == nil && th != nil {
		if th.Status == models.ThreadStatusBusy {
			return nil, false, &state.ErrConflict{Resource: "thread", ID: threadID, Reason: "is busy"}
		}
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

	tid := tenant.FromContext(ctx)
	s.hooks.Dispatch(hooks.Event{Type: hooks.RunStart, RunID: runID, ThreadID: threadID, AgentID: req.AgentID, TenantID: tid, Timestamp: now})
	s.hooks.Dispatch(hooks.Event{
		Type: hooks.RunComplete, RunID: runID, ThreadID: threadID, AgentID: req.AgentID, TenantID: tid,
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

	// Fingerprint check against req.ThreadID (empty when the client
	// didn't specify one), NOT the fresh UUID generated below -- an
	// omitted thread_id means "any thread is fine," so a found retry
	// shouldn't be flagged as a mismatch just because this call
	// happened to generate a different fallback UUID than the original
	// request's own call did.
	existing, retryErr := s.findRunForRetry(r.Context(), req.RunID, &req, req.ThreadID)
	if retryErr != nil {
		handleStoreError(w, retryErr)
		return
	}
	if existing != nil {
		writeJSON(w, http.StatusOK, existing)
		return
	}

	threadID := req.ThreadID
	if threadID == "" {
		threadID = uuid.New().String()
	}

	run, assignment, err := s.createRun(r, threadID, &req)
	if err != nil {
		s.handleCreateRunError(w, err, func(existing *models.Run) { writeJSON(w, http.StatusOK, existing) })
		return
	}

	if err := s.enqueue(r.Context(), assignment); err != nil {
		s.rollbackCreatedRun(r.Context(), run)
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
	existing, retryErr := s.findRunForRetry(r.Context(), req.RunID, &req, req.ThreadID)
	if retryErr != nil {
		handleStoreError(w, retryErr)
		return
	}
	if existing != nil {
		s.streamExistingRun(w, r, existing.RunID)
		return
	}

	threadID := req.ThreadID
	if threadID == "" {
		threadID = uuid.New().String()
	}

	run, assignment, err := s.createRun(r, threadID, &req)
	if err != nil {
		s.handleCreateRunError(w, err, func(existing *models.Run) { s.streamExistingRun(w, r, existing.RunID) })
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
	existing, retryErr := s.findRunForRetry(r.Context(), req.RunID, &req, req.ThreadID)
	if retryErr != nil {
		handleStoreError(w, retryErr)
		return
	}
	if existing != nil {
		s.waitForExistingRun(w, r, existing.RunID)
		return
	}

	threadID := req.ThreadID
	if threadID == "" {
		threadID = uuid.New().String()
	}

	run, assignment, err := s.createRun(r, threadID, &req)
	if err != nil {
		s.handleCreateRunError(w, err, func(existing *models.Run) { s.waitForExistingRun(w, r, existing.RunID) })
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
	req.Limit = clampSearchLimit(req.Limit, 10)

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
	existing, retryErr := s.findRunForRetry(r.Context(), req.RunID, &req, threadID)
	if retryErr != nil {
		handleStoreError(w, retryErr)
		return
	}
	if existing != nil {
		writeJSON(w, http.StatusOK, existing)
		return
	}

	run, assignment, err := s.createRun(r, threadID, &req)
	if err != nil {
		s.handleCreateRunError(w, err, func(existing *models.Run) { writeJSON(w, http.StatusOK, existing) })
		return
	}

	if err := s.enqueue(r.Context(), assignment); err != nil {
		s.rollbackCreatedRun(r.Context(), run)
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
	existing, retryErr := s.findRunForRetry(r.Context(), req.RunID, &req, threadID)
	if retryErr != nil {
		handleStoreError(w, retryErr)
		return
	}
	if existing != nil {
		s.streamExistingRun(w, r, existing.RunID)
		return
	}

	run, assignment, err := s.createRun(r, threadID, &req)
	if err != nil {
		s.handleCreateRunError(w, err, func(existing *models.Run) { s.streamExistingRun(w, r, existing.RunID) })
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
	existing, retryErr := s.findRunForRetry(r.Context(), req.RunID, &req, threadID)
	if retryErr != nil {
		handleStoreError(w, retryErr)
		return
	}
	if existing != nil {
		s.waitForExistingRun(w, r, existing.RunID)
		return
	}

	run, assignment, err := s.createRun(r, threadID, &req)
	if err != nil {
		s.handleCreateRunError(w, err, func(existing *models.Run) { s.waitForExistingRun(w, r, existing.RunID) })
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
		s.rollbackCreatedRun(r.Context(), run)
		writeError(w, http.StatusInternalServerError, "failed to subscribe to events")
		return
	}

	if err := s.enqueue(r.Context(), assignment); err != nil {
		s.rollbackCreatedRun(r.Context(), run)
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
	resp, err := s.waitForRunResult(r.Context(), run, assignment)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, *resp)
}

// waitForRunResult is waitForRun's HTTP-independent core, extracted so
// the internal A2A endpoint (POST /internal/a2a/runs, which needs the
// same "enqueue and block for the terminal result" behavior but returns
// a Go value to its own caller instead of writing an HTTP response
// directly) doesn't duplicate this logic and risk it drifting from the
// client-facing /wait behavior.
func (s *Server) waitForRunResult(ctx context.Context, run *models.Run, assignment *transport.RunAssignment) (*models.RunWaitResponse, error) {
	// A cache hit (see tryServeCachedRun) already completed synchronously
	// with no broker events published for it -- respond directly instead
	// of subscribing and waiting for events that will never arrive.
	if assignment == nil {
		resp := models.RunWaitResponse{Run: run}
		var vals map[string]interface{}
		json.Unmarshal(run.Output, &vals)
		resp.Values = vals
		return &resp, nil
	}

	// Subscribe BEFORE enqueuing
	eventCh, err := s.broker.Subscribe(ctx, run.RunID)
	if err != nil {
		s.rollbackCreatedRun(ctx, run)
		return nil, fmt.Errorf("failed to subscribe: %w", err)
	}

	if err := s.enqueue(ctx, assignment); err != nil {
		s.rollbackCreatedRun(ctx, run)
		return nil, fmt.Errorf("failed to enqueue: %w", err)
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
	finalRun, _ := s.store.GetRun(ctx, run.RunID)
	if finalRun == nil {
		finalRun = run
	}
	if terminalStatus != "" && !isTerminalStatus(finalRun.Status) {
		tryStatusTransition("update_run_status", finalRun.ThreadID, run.RunID, func() error {
			return s.store.UpdateRunStatus(ctx, run.RunID, models.RunStatus(terminalStatus), nil, "")
		})
		// See waitForExistingRun's identical fix for the full rationale:
		// the thread's own status must be reset too, not just the run's,
		// or a fast wait-then-immediately-create-the-next-run on the
		// same thread can get a spurious 409 (TryClaimThread sees a
		// still-"busy" thread until StatusCallback's later write).
		threadStatus := models.ThreadStatusIdle
		if terminalStatus == string(models.RunStatusInterrupted) {
			threadStatus = models.ThreadStatusInterrupted
		}
		tryStatusTransition("set_thread_status", finalRun.ThreadID, run.RunID, func() error {
			return s.store.SetThreadStatus(ctx, finalRun.ThreadID, threadStatus)
		})
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
	return &resp, nil
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
		tryStatusTransition("update_run_status", finalRun.ThreadID, runID, func() error {
			return s.store.UpdateRunStatus(r.Context(), runID, models.RunStatus(terminalStatus), nil, "")
		})
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
		tryStatusTransition("set_thread_status", finalRun.ThreadID, runID, func() error {
			return s.store.SetThreadStatus(r.Context(), finalRun.ThreadID, threadStatus)
		})
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

// cancelRun implements the Agent Protocol spec's two query params on
// POST /runs/{run_id}/cancel (and the thread-scoped equivalent):
//
//   - wait (bool, default false): whether the HTTP response itself waits
//     for the run's post-cancel grace window (see cancelRunSingle) before
//     returning, instead of backgrounding it. The run's status is set to
//     "interrupted" synchronously either way -- wait only changes when
//     the response is sent, not what gets persisted or when.
//   - action (interrupt|rollback, default interrupt): rollback additionally
//     deletes the run record after cancelling it (see cancelRunCore's own
//     doc comment for the one honest limitation: this does NOT delete any
//     checkpoints, unlike the spec's literal wording).
func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request, runID string) {
	wait := r.URL.Query().Get("wait") == "true"
	rollback := r.URL.Query().Get("action") == "rollback"

	updated, err := s.cancelRunCore(r.Context(), runID, wait, rollback)
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
// REST DELETE/cancel handler and the WebSocket "run.cancel" command (which
// always passes wait=false, rollback=false -- today's exact prior behavior,
// unchanged; wiring the WS command's own body to these same options is a
// reasonable follow-up, not done here).
//
// rollback=true deletes the run row (state.Store.DeleteRun) after
// cancelling it -- but NOT any checkpoints, despite the Agent Protocol
// spec's literal "action=rollback ... will cancel the run and delete the
// run and associated checkpoints afterwards" wording. Checkpoints in this
// schema are keyed by thread_id, not run_id (SaveCheckpoint's own
// signature) -- a thread accumulates checkpoints across every run ever
// executed on it, with no per-run attribution to select just this run's
// slice for deletion. Worse, in direct mode (the common, recommended
// checkpoint deployment -- see the Checkpoint Dual Mode docs), checkpoints
// live entirely in LangGraph's own Postgres tables, invisible to this
// state.Store's checkpoint table at all -- deleting rows here would be a
// silent no-op for exactly the deployments most likely to have real
// checkpoints to delete. Implementing this precisely would need a schema
// change (tagging every checkpoint with the run_id that created it)
// across all 4 backends, disproportionate to this fix; documenting the
// gap honestly here is the deliberate choice over a half-correct delete.
func (s *Server) cancelRunCore(ctx context.Context, runID string, wait, rollback bool) (*models.Run, error) {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}

	updated, err := s.cancelRunSingle(ctx, run, wait)
	if err != nil {
		return nil, err
	}

	// A2A cancel cascade: cancelling a run must also cancel anything it
	// delegated to, directly or transitively -- otherwise a cancelled
	// parent leaves orphaned child runs still
	// executing with no way for the caller to have stopped them). Best-
	// effort: a lookup/cancel failure here must never fail the parent's
	// own cancel, which already succeeded above.
	s.cascadeCancelDescendants(ctx, run)

	if rollback {
		// A DeleteRun failure here must surface as an error response
		// (same as plain DELETE /runs/{id}, see deleteRunGuarded), not
		// a silent 204 -- returning nil, nil despite the delete failing
		// would tell the client the run is gone when a GET can still
		// find it (as "interrupted", since the cancel half above already
		// succeeded independently and is not undone by this failure).
		if delErr := s.store.DeleteRun(ctx, runID); delErr != nil {
			return nil, delErr
		}
		return nil, nil
	}

	return updated, nil
}

// cancelRunSingle performs the side effects of cancelling exactly ONE
// run (queue cancel, pub/sub cancel signal, status update, thread idle,
// terminal bookkeeping) without touching any other run in its
// delegation tree -- factored out of cancelRunCore so
// cascadeCancelDescendants can apply the same side effects to each
// descendant without re-triggering a redundant tree lookup per node
// (cancelRunCore itself already looked up the whole tree once).
//
// wait controls whether the post-cancel grace window (giving the runner
// a few seconds to emit any final straggler events before the broker is
// closed) runs synchronously -- delaying whatever eventually calls this
// -- or in the background, as it always did before the wait query param
// existed. Callers cancelling A2A descendants (cascadeCancelDescendants)
// always pass false: a deep delegation tree cancelling sequentially with
// each hop blocking for the same grace window would make the parent's
// own cancel latency scale with tree depth, for no benefit -- only the
// single run the caller actually asked to cancel needs its own response
// to reflect that wait.
func (s *Server) cancelRunSingle(ctx context.Context, run *models.Run, wait bool) (*models.Run, error) {
	runID := run.RunID

	if isTerminalStatus(run.Status) {
		return run, nil
	}

	tryStatusTransition("queue_cancel", run.ThreadID, runID, func() error {
		return s.queue.Cancel(ctx, runID)
	})
	tryStatusTransition("publish_cancel", run.ThreadID, runID, func() error {
		return s.cancel.PublishCancel(ctx, runID)
	})
	tryStatusTransition("update_run_status", run.ThreadID, runID, func() error {
		return s.store.UpdateRunStatus(ctx, runID, models.RunStatusInterrupted, nil, "")
	})
	tryStatusTransition("set_thread_status", run.ThreadID, runID, func() error {
		return s.store.SetThreadStatus(ctx, run.ThreadID, models.ThreadStatusInterrupted)
	})

	// Finish the run's terminal bookkeeping (OTel span + on_interrupt hook)
	// here defensively -- cancel is a terminal outcome even if the runner's
	// own StatusCallback never arrives (runner already dead, etc). Safe if
	// StatusCallback ALSO fires later: finishRun's LoadAndDelete guard
	// means only whichever call arrives first does anything.
	s.finishRun(runID, run.ThreadID, run.AgentID, run.TenantID, models.RunStatusInterrupted, "")

	// Give runner time to emit its own terminal event, then close broker.
	closeBroker := func() {
		time.Sleep(5 * time.Second)
		_ = s.broker.Close(runID)
	}
	if wait {
		closeBroker()
	} else {
		go closeBroker()
	}

	slog.Info("run cancelled", "run_id", runID, "wait", wait)

	updated, _ := s.store.GetRun(ctx, runID)
	if updated != nil {
		return updated, nil
	}
	return nil, nil
}

// maxA2ACascadeRuns caps the single SearchRuns(RootRunID=...) query
// cascadeCancelDescendants issues -- a safety ceiling against a
// pathological delegation tree, not a realistic limit (a2a.max_depth
// already bounds tree depth; this bounds the total node count fetched
// in one query regardless of shape).
const maxA2ACascadeRuns = 5000

// cascadeCancelDescendants cancels every run delegated (directly or
// transitively) from run, via a single SearchRuns(root_run_id=...)
// query for the whole delegation tree followed by an in-memory
// breadth-first walk starting at run itself -- one query regardless of
// tree depth, instead of one query per level for a deep chain.
//
// Uses a system context: the entire tree always shares one tenant in
// practice (handleA2ACreateRun forces a delegated run's tenant to match
// its parent's, never the caller's), so this isn't crossing a tenant
// boundary -- it just avoids re-deriving tenant scoping for a lookup
// that's keyed by root_run_id/parent_run_id, not caller identity.
func (s *Server) cascadeCancelDescendants(ctx context.Context, run *models.Run) {
	rootID := run.RunID
	if run.RootRunID != nil {
		rootID = *run.RootRunID
	}

	sysCtx := tenant.SystemContext(ctx)
	treeRuns, err := s.store.SearchRuns(sysCtx, &models.RunSearchRequest{RootRunID: rootID, Limit: maxA2ACascadeRuns})
	if err != nil {
		slog.Warn("a2a cancel cascade: failed to look up delegation tree", "run_id", run.RunID, "error", err)
		return
	}

	childrenOf := make(map[string][]*models.Run, len(treeRuns))
	for _, r := range treeRuns {
		if r.ParentRunID != nil {
			childrenOf[*r.ParentRunID] = append(childrenOf[*r.ParentRunID], r)
		}
	}

	queue := childrenOf[run.RunID]
	for len(queue) > 0 {
		child := queue[0]
		queue = queue[1:]
		if _, err := s.cancelRunSingle(sysCtx, child, false); err != nil {
			slog.Warn("a2a cancel cascade: failed to cancel descendant run", "root_run_id", rootID, "parent_run_id", run.RunID, "child_run_id", child.RunID, "error", err)
		}
		queue = append(queue, childrenOf[child.RunID]...)
	}
}

func isTerminalStatus(status models.RunStatus) bool {
	return status == models.RunStatusSuccess ||
		status == models.RunStatusError ||
		status == models.RunStatusInterrupted ||
		status == models.RunStatusTimeout
}
