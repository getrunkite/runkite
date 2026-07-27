// Package api: Agent-to-Agent (A2A) delegation (master plan:
// "Agent-to-agent (A2A): agent calls agent via the same Agent Protocol
// API -- native sub-agent delegation. Requires real semantics for auth
// context propagation between agents, recursion limits, and cost
// attribution.")
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
//  1. Auth context propagation: on_behalf_of carries the ORIGINAL
//     caller's identity/permissions/tenant forward into the sub-run,
//     via the exact transport.UserContext shape a runner already
//     receives in its own RunAssignment.User -- the sub-agent executes
//     within the same permission boundary as the run that delegated to
//     it, never with more access than the original caller had. Tenant
//     is derived from the PARENT run's own tenant_id (looked up
//     server-side), not trusted from the request body, so a buggy or
//     compromised runner can't escalate into a different tenant by
//     forging on_behalf_of's claims.
//  2. Recursion limits: parent_run_id is required, and createRunCtx
//     (see runs.go) enforces a2a.max_depth against the parent's own
//     depth, failing a chain that's gone too deep (accidental cycle or
//     runaway delegation) with 400, not a resource leak.
//  3. Cost attribution: every delegated run's root_run_id points at the
//     top of its delegation tree, so "every run this original request
//     ultimately caused" is one query (WHERE root_run_id = ?) rather
//     than walking parent pointers.
//
// Runner-authenticated (mounted under /internal/*, not client-facing) --
// this is infrastructure a runner's own SDK helper calls on an agent's
// behalf, not something an end-user client should ever call directly.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/sharanharsoor/runkite/internal/auth"
	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/tenant"
	"github.com/sharanharsoor/runkite/internal/transport"
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
