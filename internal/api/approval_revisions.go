package api

import (
	"context"
	"net/http"

	"github.com/getrunkite/runkite/internal/models"
)

func (s *Server) checkPendingToolRevision(ctx context.Context, a *models.PendingAction) error {
	if s.connectors == nil {
		return &pendingDecisionError{http.StatusConflict, "connector unavailable; fresh review is required"}
	}
	bound, _, err := s.connectors.BindMCPRequestTarget(ctx, a.Connector)
	if err != nil {
		return &pendingDecisionError{http.StatusConflict, "connector unavailable; fresh review is required"}
	}
	var user map[string]interface{}
	if s.queue != nil {
		live, e := s.queue.LookupInflight(ctx, a.RunID)
		if e != nil {
			return &pendingDecisionError{http.StatusServiceUnavailable, "assignment lookup unavailable"}
		}
		if live != nil && live.Generation == a.Generation && live.TenantID == a.TenantID && live.GraphID == a.AgentID && live.User != nil {
			user = live.User.Extra
		}
	}
	_, revision, err := s.connectors.BindMCPToolRevision(bound, a.Connector, a.Tool, user)
	if err != nil || revision != a.ToolRevision {
		return &pendingDecisionError{http.StatusConflict, "tool definition changed or is unavailable; fresh review is required"}
	}
	return nil
}
