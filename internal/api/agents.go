package api

import (
	"net/http"
	"strconv"

	"github.com/sharanharsoor/runkite/internal/models"
)

// POST /agents/search
func (s *Server) handleSearchAgents(w http.ResponseWriter, r *http.Request) {
	var req models.AgentSearchRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}

	agents, err := s.store.SearchAgents(r.Context(), &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if agents == nil {
		agents = []*models.Agent{}
	}
	writeJSON(w, http.StatusOK, agents)
}

// GET /agents/{agentID}
func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")

	agent, err := s.store.GetAgent(r.Context(), agentID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

// GET /agents/{agentID}/schemas
func (s *Server) handleGetAgentSchemas(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")

	schema, err := s.store.GetAgentSchema(r.Context(), agentID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, schema)
}

// GET /agents/{agentID}/versions -- full agent versioning (master plan:
// "version history browsing"). Newest first, matching the store's own
// ordering contract.
func (s *Server) handleListAgentVersions(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")

	versions, err := s.store.ListAgentVersions(r.Context(), agentID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, versions)
}

// POST /agents/{agentID}/versions/{version}/rollback -- full agent
// versioning (master plan: "rollback to arbitrary past versions").
// Re-applies an old version's snapshot via the normal UpsertAgent path,
// which itself creates a NEW version whose content matches the old one
// -- see models.AgentVersion's doc comment for why this keeps history
// strictly linear/append-only rather than rewriting or deleting
// anything. The response is the resulting (new) Agent, whose Version
// will be current+1, not the target version number.
func (s *Server) handleRollbackAgent(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")
	versionStr := r.PathValue("version")
	version, err := strconv.Atoi(versionStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "version must be an integer")
		return
	}

	snapshot, err := s.store.GetAgentVersion(r.Context(), agentID, version)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	if err := s.store.UpsertAgent(r.Context(), &models.Agent{
		AgentID:      agentID,
		Name:         snapshot.Name,
		Description:  snapshot.Description,
		Metadata:     snapshot.Metadata,
		Capabilities: snapshot.Capabilities,
	}); err != nil {
		handleStoreError(w, err)
		return
	}

	updated, err := s.store.GetAgent(r.Context(), agentID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// --------------------------------------------------------------------------
// LangGraph SDK compatibility: /assistants/* routes
//
// The LangGraph Python SDK (langgraph-sdk) uses /assistants/* instead of
// /agents/* and expects a different response shape: assistant_id instead of
// agent_id, plus graph_id, config, version fields.
// --------------------------------------------------------------------------

// assistantView is the SDK-expected response shape for an "assistant".
type assistantView struct {
	AssistantID  string                 `json:"assistant_id"`
	GraphID      string                 `json:"graph_id"`
	Config       map[string]interface{} `json:"config"`
	Metadata     map[string]interface{} `json:"metadata"`
	Name         string                 `json:"name"`
	Description  string                 `json:"description,omitempty"`
	Version      int                    `json:"version"`
	CreatedAt    string                 `json:"created_at,omitempty"`
	UpdatedAt    string                 `json:"updated_at,omitempty"`
}

func agentToAssistantView(a *models.Agent) assistantView {
	return assistantView{
		AssistantID: a.AgentID,
		GraphID:     a.AgentID,
		Config:      map[string]interface{}{},
		Metadata:    a.Metadata,
		Name:        a.Name,
		Description: a.Description,
		// Real (not hardcoded) bug found in passing while touching this
		// file for full agent versioning: this always reported "1" for
		// every agent regardless of its actual version, silently hiding
		// version bumps from any client using the SDK-compat /assistants
		// surface instead of /agents.
		Version: a.Version,
	}
}

// graphSchemaView is the SDK-expected response for GET /assistants/{id}/schemas.
type graphSchemaView struct {
	GraphID      string                 `json:"graph_id"`
	InputSchema  map[string]interface{} `json:"input_schema"`
	OutputSchema map[string]interface{} `json:"output_schema"`
	StateSchema  map[string]interface{} `json:"state_schema,omitempty"`
	ConfigSchema map[string]interface{} `json:"config_schema,omitempty"`
}

// POST /assistants/search
func (s *Server) handleSearchAssistants(w http.ResponseWriter, r *http.Request) {
	var req models.AgentSearchRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}

	agents, err := s.store.SearchAgents(r.Context(), &req)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	views := make([]assistantView, 0, len(agents))
	for _, a := range agents {
		views = append(views, agentToAssistantView(a))
	}
	writeJSON(w, http.StatusOK, views)
}

// GET /assistants/{agentID}
func (s *Server) handleGetAssistant(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")

	agent, err := s.store.GetAgent(r.Context(), agentID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentToAssistantView(agent))
}

// GET /assistants/{agentID}/schemas
func (s *Server) handleGetAssistantSchemas(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")

	schema, err := s.store.GetAgentSchema(r.Context(), agentID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, graphSchemaView{
		GraphID:      schema.AgentID,
		InputSchema:  schema.InputSchema,
		OutputSchema: schema.OutputSchema,
		StateSchema:  schema.StateSchema,
		ConfigSchema: schema.ConfigSchema,
	})
}
