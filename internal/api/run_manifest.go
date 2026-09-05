package api

import (
	"encoding/json"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/transport"
)

const runManifestSchemaVersion = 1

type runManifestInput struct {
	capturedAt       time.Time
	tenantID         string
	agentID          string
	requestedAlias   string
	agent            *models.Agent
	policyFailClosed bool
	user             *transport.UserContext
	parentRunID      *string
	depth            int
}

func buildRunManifest(in runManifestInput) models.RunManifest {
	runnerKind := "python-langgraph"
	var connectorNeeds []string
	var allowedTools *[]string
	agentVersion := 0
	if in.agent != nil {
		agentVersion = in.agent.Version
		if rk, ok := in.agent.Metadata["runner_kind"].(string); ok && rk != "" {
			runnerKind = rk
		}
		if needs := extractConnectorNeeds(in.agent.Metadata); len(needs) > 0 {
			connectorNeeds = needs
		}
		allowedTools = extractAllowedTools(in.agent.Metadata)
	}

	var principal *models.RunManifestPrincipal
	if in.user != nil && in.user.Identity != "" {
		principal = &models.RunManifestPrincipal{
			Identity:    in.user.Identity,
			Permissions: append([]string(nil), in.user.Permissions...),
		}
	}

	return models.RunManifest{
		SchemaVersion:    runManifestSchemaVersion,
		CapturedAt:       in.capturedAt.UTC(),
		TenantID:         in.tenantID,
		AgentID:          in.agentID,
		RequestedAlias:   in.requestedAlias,
		AgentVersion:     agentVersion,
		RunnerKind:       runnerKind,
		ConnectorNeeds:   connectorNeeds,
		AllowedTools:     allowedTools,
		PolicyFailClosed: in.policyFailClosed,
		Principal:        principal,
		ParentRunID:      in.parentRunID,
		Depth:            in.depth,
	}
}

func runManifestToMetadata(m models.RunManifest) map[string]interface{} {
	raw, err := json.Marshal(m)
	if err != nil {
		return map[string]interface{}{"schema_version": runManifestSchemaVersion}
	}
	var out map[string]interface{}
	if json.Unmarshal(raw, &out) != nil {
		return map[string]interface{}{"schema_version": runManifestSchemaVersion}
	}
	return out
}

func runManifestToRaw(m models.RunManifest) json.RawMessage {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return raw
}
