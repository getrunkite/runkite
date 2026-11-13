package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

const headerConnectorActionID = "X-Runkite-Connector-Action-Id"

type recordedConnectorAction struct {
	store   state.ProtectedActionStore
	row     *models.ConnectorAction
	receipt *state.ConnectorActionDispatch
}

func (s *Server) handleRecordedConnectorAction(w http.ResponseWriter, r *http.Request, name string, userCtx map[string]interface{}, rpc *connector.MCPRequest) {
	in := s.policyInput(r.Context(), policy.StageToolCall, name, rpc.Tool)
	var err error
	in.Args, in.ArgsDigest, in.ArgsMeta, err = policy.BindArgsStrict(rpc.Arguments)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid or ambiguous tool arguments")
		return
	}
	if rpc.Tool == "" {
		writeError(w, http.StatusBadRequest, "tools/call requires a tool name")
		return
	}
	binding := auth.RunBindingFromContext(r.Context())
	store, ok := s.store.(state.ProtectedActionStore)
	if binding == nil || !ok || !s.policy.AuditRequired() {
		writeConnectorActionUnavailable(w, false)
		return
	}
	id := uuid.NewString()
	w.Header().Set(headerConnectorActionID, id)
	in.ConnectorActionID = id
	transportMode := "json"
	if cfg, err := s.connectors.Get(name); err == nil && cfg.Config.MCP != nil && cfg.Config.MCP.Transport != "" {
		transportMode = cfg.Config.MCP.Transport
	}
	row := &models.ConnectorAction{
		ID: id, TenantID: in.TenantID, RunID: in.RunID, ThreadID: binding.ThreadID,
		Generation: in.Generation, AgentID: in.AgentID, Principal: in.Principal,
		Connector: name, Tool: rpc.Tool, Transport: transportMode, ArgsDigest: in.ArgsDigest,
	}
	if span := trace.SpanFromContext(r.Context()).SpanContext(); span.IsValid() {
		row.TraceID = span.TraceID().String()
	}
	row, err = store.CreateConnectorAction(r.Context(), row, uuid.NewString())
	if err != nil {
		writeConnectorActionUnavailable(w, false)
		return
	}
	action := &recordedConnectorAction{store: store, row: row}
	var evaluation connectorPolicyEvaluation
	dec, deny := s.checkConnectorPolicyEvaluation(r.Context(), in, &evaluation)
	source := "policy"
	if evaluation.BreakGlassID != "" {
		source = "break_glass"
	}
	var approval *models.PendingAction
	if !dec.AuditFailed && source != "break_glass" && (!deny || dec.Effect == policy.EffectPending) {
		approval, err = store.PeekApprovedConnectorAction(r.Context(), id)
		if err != nil {
			writeConnectorActionUnavailable(w, false)
			return
		}
		if approval != nil {
			source = "approved_capability"
		}
	}
	pendingID := ""
	if approval != nil {
		pendingID = approval.ID
	}
	if deny && approval == nil && dec.Effect == policy.EffectPending {
		pendingID, err = s.persistPendingAction(r.Context(), in, name, rpc.Tool, dec, nil)
		if err != nil {
			writeConnectorActionUnavailable(w, false)
			return
		}
	}
	recordedSource := source
	if recordedSource == "approved_capability" {
		// The gate decision remains policy evidence. Prepare records approved
		// authority only when its one-shot capability is actually consumed.
		recordedSource = "policy"
	}
	action.row, err = store.RecordConnectorActionDecision(r.Context(), id, row.Version, models.ConnectorActionDecision{
		Effect: evaluation.Original.Effect, ReasonCode: evaluation.Original.ReasonCode, RuleID: evaluation.Original.RuleID,
		AuthorizationSource: recordedSource, PendingActionID: pendingID, BreakGlassID: evaluation.BreakGlassID,
	}, uuid.NewString())
	if err != nil {
		writeConnectorActionUnavailable(w, false)
		return
	}
	if deny && approval == nil {
		s.emitToolAuthEvent(r.Context(), in, dec, pendingID)
		message := dec.Reason
		if message == "" {
			message = "denied by policy"
			if dec.Effect == policy.EffectPending {
				message = "awaiting policy approval"
			}
		}
		result, err := connector.DeniedRPCResult(rpc.ID, message, mcpPolicyDenyData(dec, pendingID))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to build policy deny response")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(result.StatusCode)
		_, _ = w.Write(result.Body)
		return
	}
	observed := connector.NewMCPActionTrace(func(ctx context.Context) error {
		// Recheck the shared assignment after potentially slow preparation.
		// SQL Prepare fences the stored attempt separately. Neither check can
		// atomically commit an external service's effects.
		if s.queue == nil {
			return connector.ErrMCPDispatchBlocked
		}
		live, err := s.queue.LookupInflight(ctx, in.RunID)
		if err != nil || live == nil {
			return connector.ErrMCPDispatchBlocked
		}
		liveTenant := live.TenantID
		if liveTenant == "" {
			liveTenant = tenant.DefaultTenant
		}
		livePrincipal := ""
		if live.User != nil {
			livePrincipal = live.User.Identity
		}
		if live.Generation != in.Generation || liveTenant != in.TenantID || live.GraphID != in.AgentID || live.ThreadID != binding.ThreadID || livePrincipal != in.Principal {
			return connector.ErrMCPDispatchBlocked
		}
		receipt, err := store.PrepareConnectorActionDispatch(ctx, id, action.row.Version, pendingID, source, time.Now().UTC().Add(45*time.Second), uuid.NewString())
		if err != nil || receipt == nil || !receipt.Applied {
			return connector.ErrMCPDispatchBlocked
		}
		action.receipt = receipt
		return nil
	})
	ctx := connector.WithMCPActionTrace(r.Context(), observed)
	result, forwardErr := s.connectors.ProxyMCPRequest(ctx, name, userCtx, rpc.Body)
	ob := observed.Snapshot()
	if err := action.finish(r.Context(), ob); err != nil {
		slog.Warn("connector action observation persistence failed", "connector_action_id", id)
		writeConnectorActionUnavailable(w, ob.DispatchStarted)
		return
	}
	if forwardErr != nil {
		if errors.Is(forwardErr, connector.ErrMCPDispatchBlocked) {
			writeConnectorActionUnavailable(w, false)
			return
		}
		reason := "connector_preparation_failed"
		message := "connector preparation failed; protected tool was not dispatched"
		if ob.DispatchStarted {
			reason = "connector_outcome_unknown"
			message = "connector operation outcome is unknown; the tool may have executed"
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"message": message, "reason_code": reason})
		return
	}
	if result.StatusCode == http.StatusUnauthorized {
		writeJSON(w, http.StatusBadGateway, map[string]string{"message": "upstream connector authentication failed; the operation may have executed", "reason_code": "connector_upstream_unauthorized"})
		return
	}
	result.Body = s.maybeShrinkMCP(r.Context(), result.StatusCode, result.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(result.StatusCode)
	_, _ = w.Write(result.Body)
}

func (a *recordedConnectorAction) finish(ctx context.Context, ob connector.MCPActionObservation) error {
	observation := models.ConnectorActionObservation{
		State: models.ConnectorActionNotDispatched, ReasonCode: ob.ReasonCode, ResponseKind: ob.ResponseKind,
		HTTPStatus: ob.HTTPStatus, RPCErrorCode: ob.RPCErrorCode, ResponseBytes: ob.ResponseBytes,
	}
	version, dispatchID := a.row.Version, ""
	if a.receipt != nil {
		version, dispatchID = a.receipt.Version, a.receipt.DispatchID
		observation.State = models.ConnectorActionOutcomeUnknown
		if ob.ResponseObserved {
			observation.State = models.ConnectorActionResponseObserved
		}
	}
	// A disconnected caller must not erase an observed response. The same
	// mutation ID can retry SQL persistence, never the external tool.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	mutationID := uuid.NewString()
	for range 2 {
		row, err := a.store.FinishConnectorAction(finishCtx, a.row.ID, version, dispatchID, observation, mutationID)
		if err == nil {
			a.row = row
			return nil
		}
		if finishCtx.Err() != nil {
			return finishCtx.Err()
		}
	}
	return errors.New("connector action observation unavailable")
}

func writeConnectorActionUnavailable(w http.ResponseWriter, mayHaveExecuted bool) {
	message := "connector action evidence unavailable; protected tool was not dispatched"
	if mayHaveExecuted {
		message = "connector action evidence incomplete; the tool may have executed"
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": message, "reason_code": "connector_action_evidence_unavailable"})
}
