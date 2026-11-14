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
// Delegation is bound to the currently in-flight parent assignment. Its
// tenant and initiating user are server-owned. on_behalf_of can only narrow
// explicit permissions while retaining that user's identity and claims.
// Recursion limits, tree cost attribution and cancellation reuse the normal
// run lifecycle.
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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/google/uuid"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

// A2ARunRequest is the body for POST /internal/a2a/runs.
type A2ARunRequest struct {
	AgentID     string          `json:"agent_id"`
	ThreadID    string          `json:"thread_id,omitempty"` // auto-generated if empty
	Input       json.RawMessage `json:"input,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`
	ParentRunID string          `json:"parent_run_id"` // required
	OnBehalfOf  json.RawMessage `json:"on_behalf_of,omitempty"`
	Wait        bool            `json:"wait,omitempty"`
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

	binding := auth.RunBindingFromContext(r.Context())
	if binding == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "parent run binding required", "reason_code": auth.ReasonRunBindingRequired})
		return
	}
	if req.ParentRunID != binding.RunID {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "parent_run_id does not match run binding", "reason_code": "a2a_parent_mismatch"})
		return
	}
	// Read again after the body. A parent can be reclaimed while a slow
	// request is being read. This check does not claim an atomic transaction
	// across the queue and child creation.
	current, err := s.queue.LookupInflight(r.Context(), binding.RunID)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "failed to resolve parent assignment", "reason_code": auth.ReasonRunBindingLookupFailed})
		return
	}
	if current == nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "parent run is not in-flight", "reason_code": auth.ReasonRunNotInflight})
		return
	}
	if current.Generation != binding.Generation {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "parent generation mismatch", "reason_code": auth.ReasonRunGenerationMismatch})
		return
	}
	tenantID := current.TenantID
	if tenantID == "" {
		tenantID = tenant.DefaultTenant
	}
	if tenantID != binding.TenantID || current.GraphID != binding.AgentID || current.ThreadID != binding.ThreadID {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "parent assignment changed", "reason_code": "a2a_parent_mismatch"})
		return
	}
	ctx := tenant.WithContext(r.Context(), tenantID)
	parent, err := s.store.GetRun(ctx, binding.RunID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if parent.ThreadID != current.ThreadID || parent.AgentID != current.GraphID {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "parent assignment does not match stored run", "reason_code": "a2a_parent_mismatch"})
		return
	}
	if parent.Status != models.RunStatusRunning && parent.Status != models.RunStatusPending {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "parent run is terminal", "reason_code": auth.ReasonRunNotInflight})
		return
	}
	principal, err := a2aDelegatedPrincipal(current.User, parent, req.OnBehalfOf)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": err.Error(), "reason_code": "a2a_authority_denied"})
		return
	}
	ctx = auth.WithContext(ctx, principal)

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

	if err := s.checkA2AParentCurrent(ctx, parentRunID); err != nil {
		// Use canonical completion so an overlapping parent cancellation
		// cascade cannot finalize or decrement this child a second time.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_, cleanupErr := s.cancelRunCore(cleanupCtx, run.RunID, false, false)
		cancel()
		if cleanupErr != nil {
			handleStoreError(w, cleanupErr)
			return
		}
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

// checkA2AParentCurrent closes slow preflight and connector setup windows.
// Queue validation and durable insertion remain separate operations; this is
// not an atomic cross-store delegation transaction.
func (s *Server) checkA2AParentCurrent(ctx context.Context, parentID string) error {
	binding := auth.RunBindingFromContext(ctx)
	if binding == nil || binding.RunID != parentID {
		return &hooks.ErrDenied{Reason: "parent run binding changed"}
	}
	current, err := s.queue.LookupInflight(ctx, parentID)
	if err != nil {
		return fmt.Errorf("resolve parent assignment: %w", err)
	}
	if current == nil || current.Generation != binding.Generation || current.ThreadID != binding.ThreadID || current.GraphID != binding.AgentID {
		return &hooks.ErrDenied{Reason: "parent assignment is no longer current"}
	}
	tenantID := current.TenantID
	if tenantID == "" {
		tenantID = tenant.DefaultTenant
	}
	if tenantID != binding.TenantID || tenantID != tenant.FromContext(ctx) {
		return &hooks.ErrDenied{Reason: "parent assignment tenant changed"}
	}
	parent, err := s.store.GetRun(ctx, parentID)
	if err != nil {
		return err
	}
	if parent.Status != models.RunStatusRunning && parent.Status != models.RunStatusPending {
		return &hooks.ErrDenied{Reason: "parent run is no longer active"}
	}
	return nil
}

// a2aDelegatedPrincipal retains server-owned identity and attributes. The
// optional body only selects a literal subset of the parent's explicit grants.
func a2aDelegatedPrincipal(user *transport.UserContext, parent *models.Run, raw json.RawMessage) (*auth.AuthResult, error) {
	var principal *auth.AuthResult
	verified := false
	if user != nil && user.Identity != "" {
		principal = &auth.AuthResult{Identity: user.Identity, Permissions: append([]string(nil), user.Permissions...), TenantID: parent.TenantID, DisplayName: user.DisplayName, Extra: user.Extra}
	}
	if frozen, present := parent.Metadata["run_manifest"]; present {
		encoded, err := json.Marshal(frozen)
		var manifest models.RunManifest
		if err != nil || json.Unmarshal(encoded, &manifest) != nil {
			return nil, fmt.Errorf("parent manifest is invalid")
		}
		if manifest.Principal != nil {
			if principal == nil || principal.Identity != manifest.Principal.Identity || !samePermissionSet(principal.Permissions, manifest.Principal.Permissions) {
				return nil, fmt.Errorf("parent identity does not match frozen manifest")
			}
			verified = manifest.Principal.VerifiedIdentity
		} else if principal != nil {
			return nil, fmt.Errorf("parent manifest has no delegated identity")
		}
	}
	if !verified {
		auth.MarkIdentityUnverified(principal)
	}
	if len(raw) == 0 {
		return principal, nil
	}
	var fields map[string]json.RawMessage
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, fmt.Errorf("on_behalf_of must be an object")
	}
	if principal == nil {
		return nil, fmt.Errorf("anonymous parent cannot supply on_behalf_of")
	}
	for name, value := range fields {
		switch name {
		case "identity", "display_name":
			var supplied string
			want := principal.Identity
			if name == "display_name" {
				want = principal.DisplayName
			}
			if json.Unmarshal(value, &supplied) != nil || supplied != want {
				return nil, fmt.Errorf("on_behalf_of cannot replace parent identity or claims")
			}
		case "is_authenticated":
			var supplied bool
			if json.Unmarshal(value, &supplied) != nil || supplied != user.IsAuthenticated {
				return nil, fmt.Errorf("on_behalf_of cannot replace parent identity or claims")
			}
		case "permissions":
			var supplied []string
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &supplied) != nil || (len(supplied) == 0 && len(user.Permissions) != 0) {
				// Empty grants mean unrestricted in the existing auth contract.
				// They can preserve an unrestricted parent, never widen a
				// restricted parent. Omit the field to inherit either form.
				return nil, fmt.Errorf("on_behalf_of permissions must preserve or narrow parent permissions; omit to inherit")
			}
			for _, permission := range supplied {
				if permission == "" || (len(user.Permissions) != 0 && !containsPermission(user.Permissions, permission)) {
					return nil, fmt.Errorf("on_behalf_of cannot expand parent permissions")
				}
			}
			principal.Permissions = supplied
		case "extra":
			var supplied map[string]interface{}
			if json.Unmarshal(value, &supplied) != nil || supplied == nil {
				return nil, fmt.Errorf("on_behalf_of claims are invalid")
			}
			for key, claim := range supplied {
				if !sameA2AClaim(principal.Extra, key, claim) {
					return nil, fmt.Errorf("on_behalf_of cannot replace parent identity or claims")
				}
			}
		default:
			var claim interface{}
			if json.Unmarshal(value, &claim) != nil || !sameA2AClaim(principal.Extra, name, claim) {
				return nil, fmt.Errorf("on_behalf_of cannot replace parent identity or claims")
			}
		}
	}
	return principal, nil
}

func sameA2AClaim(extra map[string]interface{}, name string, value interface{}) bool {
	original, exists := extra[name]
	if !exists {
		return false
	}
	// Normalize JSON number/container representation from trusted in-memory
	// fixtures and decoded queue assignments before comparing claims.
	raw, err := json.Marshal(original)
	if err != nil {
		return false
	}
	var normalized interface{}
	return json.Unmarshal(raw, &normalized) == nil && reflect.DeepEqual(normalized, value)
}

func containsPermission(permissions []string, want string) bool {
	for _, permission := range permissions {
		if permission == want {
			return true
		}
	}
	return false
}

func samePermissionSet(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	for _, permission := range a {
		if !containsPermission(b, permission) {
			return false
		}
	}
	for _, permission := range b {
		if !containsPermission(a, permission) {
			return false
		}
	}
	return true
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
	// persistTerminalUsage) instead of silently looking free. The runner
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
