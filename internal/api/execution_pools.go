package api

import (
	"errors"

	"github.com/getrunkite/runkite/internal/execution"
	"github.com/getrunkite/runkite/internal/models"
)

var errExecutionPoolDenied = errors.New("execution pool does not allow this assignment")

// SetExecutionPools installs immutable operator configuration before serving.
func (s *Server) SetExecutionPools(pools *execution.Catalog) { s.executionPools = pools }

func runnerKindForAgent(agent *models.Agent) string {
	if agent != nil {
		if kind, ok := agent.Metadata["runner_kind"].(string); ok && kind != "" {
			return kind
		}
	}
	return "python-langgraph"
}
