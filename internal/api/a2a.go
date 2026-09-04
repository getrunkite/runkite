// Package api: Agent-to-Agent (A2A) delegation -- native sub-agent
// delegation where an agent calls another agent via the same Agent
// Protocol API, with real semantics for auth context propagation between
// agents, recursion limits, and cost attribution.
//
// The mechanism is deliberately NOT a new client-facing endpoint --
// it's the exact same POST /threads/{id}/runs + wait-for-result path
// any client uses, just reachable from inside a runner's own process
// via one new /internal/* route instead of a public one. A running
// agent's own code (e.g. a LangGraph node) calls this to invoke another
// agent as a sub-task, wait for its result, and continue -- "agent
// calls agent via the same Agent Protocol API" in the most literal
// sense: createRunCtx and waitForRunResult are the identical functions
// every client-facing run-creation path already uses.
//
// Three things this endpoint adds on top of that shared path:
//
//  1. Auth context propagation: on_behalf_of forwards identity/
//     permissions into the sub-run (propagation, not enforcement --
//     the body is trusted from the runner; runs don't persist the
//     original caller's auth to compare against). Tenant is derived
//     from the PARENT run's own tenant_id (looked up server-side),
//     never from the request body. Parent lookup uses SystemContext
//     (runner-trusted /internal/*), so a compromised runner that
//     knows another tenant's run UUID could attach a child there --
//     same trust boundary as the rest of /internal/*, stated plainly.
//  2. Recursion limits: parent_run_id is required, and createRunCtx
//     (see runs.go) enforces a2a.max_depth against the parent's own
//     depth, failing a chain that's gone too deep (accidental cycle or
//     runaway delegation) with 400, not a resource leak.
//  3. Cost attribution: every delegated run's root_run_id points at the
//     top of its delegation tree (persisted + indexed). RunSearchRequest
//     exposes this as a client-facing filter (root_run_id) so any run
//     in a tree can be used to find every other run in it with one
//     query, and GET /runs/{runID}/cost (below) aggregates best-effort
//     token/cost usage across the whole tree from it.
//
// Runner-authenticated (mounted under /internal/*, not client-facing) --
// this is infrastructure a runner's own SDK helper calls on an agent's
// behalf, not something an end-user client should ever call directly.
//
// Cancelling a run cascades to everything it delegated, directly or
// transitively (see runs.go's cancelRunCore/cascadeCancelDescendants) --
// a cancelled parent can't leave orphaned children still executing.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

// A2ARunRequest is the body for POST /internal/a2a/runs.
type A2ARunRequest struct {
	AgentID     string                 `json:"agent_id"`
	ThreadID    string                 `json:"thread_id,omitempty"` // auto-generated if empty
	Input       json.RawMessage        `json:"input,omitempty"`
	Config      json.RawMessage        `json:"config,omitempty"`
	ParentRunID string                 `json:"parent_run_id"` // required
	OnBehalfOf  *transport.UserContext `json:"on_behalf_of,omitempty"`
	Wait        bool                   `json:"wait,omitempty"`
}

// POST /internal/a2a/runs
func (s *Server) handleA2ACreateRun(w http.ResponseWriter, r *http.Request) {
	var req A2ARunRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.AgentID == "" {
		writeError(w, http.StatusBadRequest, "agent_id is required")
		return
	}
	if req.ParentRunID == "" {
		writeError(w, http.StatusBadRequest, "parent_run_id is required")
		return
	}

	// System context to look up the parent regardless of which tenant
	// it's in -- this endpoint has no client auth to derive a tenant
	// from (it's runner-authenticated, not user-authenticated). The
	// sub-run's actual tenant scoping is set below from the parent's
	// own tenant_id, not trusted from anywhere in the request body.
	parent, err := s.store.GetRun(tenant.SystemContext(r.Context()), req.ParentRunID)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	ctx := tenant.WithContext(r.Context(), parent.TenantID)
	if req.OnBehalfOf != nil {
		ctx = auth.WithContext(ctx, &auth.AuthResult{
			Identity:    req.OnBehalfOf.Identity,
			Permissions: req.OnBehalfOf.Permissions,
			TenantID:    parent.TenantID, // never trust on_behalf_of's own tenant claim
			DisplayName: req.OnBehalfOf.DisplayName,
			Extra:       req.OnBehalfOf.Extra,
		})
	}

	threadID := req.ThreadID
	if threadID == "" {
		threadID = "a2a-" + uuid.New().String()
	}
	parentRunID := req.ParentRunID

	run, assignment, err := s.createRunCtx(ctx, threadID, &models.RunCreate{
		AgentID:     req.AgentID,
		Input:       req.Input,
		Config:      req.Config,
		ParentRunID: &parentRunID,
	})
	if err != nil {
		handleStoreError(w, err)
		return
	}

	if !req.Wait {
		if err := s.enqueue(ctx, assignment); err != nil {
			s.rollbackCreatedRun(ctx, run)
			writeError(w, http.StatusInternalServerError, "failed to enqueue")
			return
		}
		writeJSON(w, http.StatusOK, run)
		return
	}

	resp, err := s.waitForRunResult(ctx, run, assignment)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, *resp)
}

// --------------------------------------------------------------------------
// Cost aggregation -- cost attribution via root_run_id, a rollup on top
// of it, not just the raw field.
// --------------------------------------------------------------------------

// RunUsage is LLM token/cost usage for one run. Every field is best-
// effort: nothing in the Runner Protocol requires a runner to report
// usage today (deliberately, to ship this without a protocol version
// bump -- see RunCostSummary's doc comment), so a run that never
// reported any of this simply contributes all zeros to a rollup.
type RunUsage struct {
	PromptTokens     int64   `json:"prompt_tokens,omitempty"`
	CompletionTokens int64   `json:"completion_tokens,omitempty"`
	TotalTokens      int64   `json:"total_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
	// Unmetered is set by a runner when it found an AI-shaped reply (a
	// real model turn happened) but could not extract any token/cost data
	// from it at all -- distinct from zero usage meaning "no LLM call
	// happened". Surfaces as a usage_unmetered audit alert (see
	// ingestTerminalUsage) instead of silently looking free. The runner
	// side (python/runkite_runner/usage.py's usage_payload /
	// usage_or_unmetered) is where this gets set; it exists specifically
	// for a provider/framework integration whose usage-reporting shape
	// this codebase has never seen.
	Unmetered bool `json:"unmetered,omitempty"`
}

// RunCostDetail is one run's contribution to a RunCostSummary.
type RunCostDetail struct {
	RunID   string   `json:"run_id"`
	AgentID string   `json:"agent_id"`
	Depth   int      `json:"depth"`
	Usage   RunUsage `json:"usage"`
}

// RunCostSummary is the response for GET /runs/{runID}/cost: a rollup
// of usage across an entire A2A delegation tree, keyed by whichever
// run's ID the caller happened to have on hand -- any run in the tree
// resolves to the same root and the same aggregate.
//
// Deliberately convention-based, not a new required field on Run or a
// Runner Protocol change: it reads whatever a run's own Output JSON
// already contains. If a runner's output happens to include a
// top-level "usage" object -- the same shape most LLM APIs already
// return (common LLM API-style prompt_tokens/completion_tokens/
// total_tokens), plus an optional cost_usd -- it's picked up and summed
// across the tree. This ships real value against what agents already
// emit today; a future Runner Protocol version could make usage
// reporting authoritative instead of best-effort without changing this
// endpoint's shape, only extractRunUsage's source.
type RunCostSummary struct {
	RootRunID string          `json:"root_run_id"`
	RunCount  int             `json:"run_count"`
	Usage     RunUsage        `json:"usage"`
	Runs      []RunCostDetail `json:"runs"`
}

// GET /runs/{runID}/cost
func (s *Server) handleGetRunCost(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	run, err := s.store.GetRun(r.Context(), runID)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	rootID := run.RunID
	if run.RootRunID != nil {
		rootID = *run.RootRunID
	}

	// SearchRuns(root_run_id=...) excludes the root itself by design
	// (a root's own RootRunID is nil -- see models.Run's doc comment),
	// so it's fetched separately here unless the run being queried
	// already IS the tree's own root.
	allRuns := make([]*models.Run, 0, 1)
	if rootID == run.RunID {
		allRuns = append(allRuns, run)
	} else if rootRun, err := s.store.GetRun(r.Context(), rootID); err == nil {
		allRuns = append(allRuns, rootRun)
	}
	descendants, err := s.store.SearchRuns(r.Context(), &models.RunSearchRequest{RootRunID: rootID, Limit: maxA2ACascadeRuns})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	allRuns = append(allRuns, descendants...)

	summary := RunCostSummary{RootRunID: rootID, RunCount: len(allRuns), Runs: make([]RunCostDetail, 0, len(allRuns))}
	for _, rn := range allRuns {
		usage := extractRunUsage(rn.Output)
		summary.Usage.PromptTokens += usage.PromptTokens
		summary.Usage.CompletionTokens += usage.CompletionTokens
		summary.Usage.TotalTokens += usage.TotalTokens
		summary.Usage.CostUSD += usage.CostUSD
		summary.Runs = append(summary.Runs, RunCostDetail{RunID: rn.RunID, AgentID: rn.AgentID, Depth: rn.Depth, Usage: usage})
	}

	writeJSON(w, http.StatusOK, summary)
}

// extractRunUsage reads a run's Output JSON for a conventional
// top-level "usage" object, tolerating the key (or any field within
// it) being entirely absent -- see RunCostSummary's doc comment for
// why this is a best-effort convention, not a contract enforced
// anywhere in the Runner Protocol. If a runner reports prompt/
// completion tokens but not their sum, total_tokens is filled in as a
// fallback rather than left at zero.
func extractRunUsage(output json.RawMessage) RunUsage {
	if len(output) == 0 {
		return RunUsage{}
	}
	var parsed struct {
		Usage RunUsage `json:"usage"`
	}
	if err := json.Unmarshal(output, &parsed); err != nil {
		return RunUsage{}
	}
	u := parsed.Usage
	if u.TotalTokens == 0 && (u.PromptTokens > 0 || u.CompletionTokens > 0) {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	return u
}
